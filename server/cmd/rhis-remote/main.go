// Command rhis-remote is the RHIS Remote server. One binary runs every role;
// the roles setting decides which parts are active on this machine.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/agentupdate"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/api"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/backup"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/codesign"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/mail"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/relay"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/sessions"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/update"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/web"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/rhis-remote/config.yaml", "path to the configuration file")
		fs.Parse(args)
		if err := serve(*cfgPath); err != nil {
			slog.Error("server stopped", "err", err)
			if errors.Is(err, cluster.ErrPermanent) {
				os.Exit(3)
			}
			os.Exit(1)
		}
	case "version":
		fmt.Printf("%s (commit %s, built %s, channel %s)\n", version.Version, version.Commit, version.BuildDate, version.Channel)
	case "gen-secret":
		b := make([]byte, 32)
		rand.Read(b)
		fmt.Println(base64.RawURLEncoding.EncodeToString(b))
	default:
		fmt.Fprintf(os.Stderr, "usage: rhis-remote [serve|version|gen-secret] [--config path]\n")
		os.Exit(2)
	}
}

func serve(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	slog.Info("starting", "version", version.Version, "roles", cfg.Roles)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	ident, err := cluster.LoadIdentity(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("node identity: %w", err)
	}
	if !cfg.NeedsDatabase() {
		return serveRemote(ctx, stop, &cfg, ident)
	}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	store := &settings.Store{DB: pool}
	tmpDir := filepath.Join(cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	code := &codesign.Service{Store: store, Box: box, TempDir: tmpDir, Name: cfg.Branding.ProductName,
		URL: cfg.PublicURL, Command: cfg.CodeSigning.Command}
	signer, err := agentupdate.LoadSigner(ctx, store, box)
	if err != nil {
		return fmt.Errorf("agent update key: %w", err)
	}
	agentsRepo := cfg.Agents.Repo
	if agentsRepo == "" {
		agentsRepo = cfg.Update.Repo
	}
	agents := &agentupdate.Service{DB: pool, Store: store, Signer: signer, Code: code, DataDir: cfg.DataDir,
		APIURL: cfg.Update.APIURL, Repo: agentsRepo, Token: cfg.Update.Token, Keep: cfg.Agents.Keep}

	var bus cluster.Bus = cluster.NewLocalBus()
	if cfg.Cluster.NATS.URL != "" {
		nb, err := cluster.NewNATSBus(cfg.Cluster, cfg.Node.Name)
		if err != nil {
			return err
		}
		bus = nb
		slog.Info("cluster bus connected", "nats", cfg.Cluster.NATS.URL)
	}
	defer bus.Close()

	reg := &cluster.Registry{DB: pool, Store: store, Box: box, Bus: bus, Self: ident,
		Name: cfg.Node.Name, Region: cfg.Node.Region, Roles: cfg.Roles}
	if cfg.HasRole("relay") {
		rl, err := startLocalRelay(ctx, &cfg, reg)
		if err != nil {
			// The portal stays usable without a relay; sessions then rely on direct
			// connections and the gateway's WebSocket fallback.
			slog.Error("relay not started", "err", err)
		} else {
			defer rl.Close()
			reg.RelayURLs = rl.URLs
			reg.Stats = append(reg.Stats, rl.Stats)
		}
	}
	go reg.Run(ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reg.PruneOffline(ctx, 7*24*time.Hour)
			}
		}
	}()

	recStore, err := sessions.NewStore(cfg.DataDir, cfg.SecretKey)
	if err != nil {
		return err
	}
	a := &api.API{
		Cfg:      &cfg,
		DB:       pool,
		Auth:     &auth.Service{DB: pool, SecureCookie: strings.HasPrefix(cfg.PublicURL, "https://")},
		Audit:    &audit.Log{DB: pool},
		Mail:     &mail.Service{Store: store, Box: box},
		Backup:   &backup.Service{DB: pool, Store: store, Box: box, DatabaseURL: cfg.DatabaseURL, DataDir: cfg.DataDir},
		Builder:  &builder.Service{DB: pool, Box: box, Src: agents, DataDir: cfg.DataDir},
		Agents:   agents,
		CodeSign: code,
		Sessions: &sessions.Service{DB: pool, Store: store, Rec: recStore},
		Update:   &update.Service{Store: store, APIURL: cfg.Update.APIURL, Repo: cfg.Update.Repo, Asset: cfg.Update.Asset, Token: cfg.Update.Token},
		Cluster:  reg,
		Bus:      bus,
		Restart: func() {
			slog.Info("restarting after update")
			stop()
		},
	}
	if cfg.HasRole("scheduler") {
		sch := &backup.Scheduler{Svc: a.Backup, OnDone: func(kind string, run *backup.Run, err error) {
			// Scheduled runs are logged as the system.
			apiRecord(a, kind, run, err)
		}}
		a.Scheduler = sch
		bus.Subscribe(cluster.EventBackupSettings, func(cluster.Msg) {
			if err := sch.Reload(context.Background()); err != nil {
				slog.Error("backup schedule", "err", err)
			}
		})
		// In a cluster only one server runs the scheduled jobs (backups, cleanup).
		go cluster.RunAsLeader(ctx, pool, cluster.LockScheduler, "scheduler", func(lctx context.Context) {
			if err := sch.Start(lctx); err != nil {
				slog.Error("backup schedule", "err", err)
			}
			defer sch.Stop()
			// Agent releases are imported by the same single leader.
			if every, err := time.ParseDuration(cfg.Agents.SyncInterval); err == nil && every > 0 {
				go agents.RunSync(lctx, every)
			}
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				if err := a.Sessions.Cleanup(lctx); err != nil && lctx.Err() == nil {
					slog.Error("session cleanup", "err", err)
				}
				select {
				case <-lctx.Done():
					return
				case <-t.C:
				}
			}
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	if cfg.HasRole("api") {
		mux.Handle("/api/", http.StripPrefix("/api", a.Routes()))
	}
	if cfg.HasRole("update") {
		// Agent endpoints authenticate with a device token, not a portal session.
		ar := chi.NewRouter()
		ar.Use(middleware.Recoverer)
		h := &agentupdate.Handler{S: agents, Auth: agentupdate.SQLDeviceAuth{S: agents}, PublicURL: cfg.PublicURL}
		ar.Route("/api/agent/v1", h.Mount)
		mux.Handle("/api/agent/v1/", ar)
		mux.HandleFunc("GET /d/{token}", a.PublicDownload)
	}
	if cfg.HasRole("web") {
		mux.Handle("/", web.Handler())
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL)
		if cfg.TLS.CertFile != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}

// startLocalRelay runs the relay on a server that also has the database.
func startLocalRelay(ctx context.Context, cfg *config.Config, reg *cluster.Registry) (*relay.Server, error) {
	ip, err := relayPublicIP(ctx, cfg, "")
	if err != nil {
		return nil, err
	}
	rl := &relay.Server{Cfg: cfg.Relay, DataDir: cfg.DataDir, PublicIP: ip, Secrets: func() []string {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := reg.TURNSecrets(c)
		if err != nil {
			slog.Error("relay: TURN secrets", "err", err)
		}
		return s
	}}
	return rl, rl.Start()
}

// relayPublicIP picks the address peers use to reach the relay: relay.public_ip, the
// address the control server saw (for joined relays), or the public URL's host.
func relayPublicIP(ctx context.Context, cfg *config.Config, observed string) (net.IP, error) {
	if cfg.Relay.PublicIP != "" {
		return net.ParseIP(cfg.Relay.PublicIP), nil
	}
	var ip net.IP
	if observed != "" {
		ip = net.ParseIP(observed)
	} else if u, err := url.Parse(cfg.PublicURL); err == nil && u.Hostname() != "" {
		ip, _ = relay.DetectPublicIP(ctx, u.Hostname())
	}
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
		return nil, fmt.Errorf("cannot determine the relay's public IP (found %v); set relay.public_ip", ip)
	}
	return ip, nil
}

// serveRemote runs a server without a database: a relay that joined a control server.
func serveRemote(ctx context.Context, stop context.CancelFunc, cfg *config.Config, ident *cluster.Identity) error {
	rem := &cluster.Remote{Cfg: cfg, Self: ident, DataDir: cfg.DataDir, AllowInsecure: cfg.AllowInsecureHTTP}
	upd := &update.Service{APIURL: cfg.Update.APIURL, Repo: cfg.Update.Repo, Asset: cfg.Update.Asset, Token: cfg.Update.Token}
	var rl *relay.Server
	if cfg.HasRole("relay") {
		rl = &relay.Server{Cfg: cfg.Relay, DataDir: cfg.DataDir, Secrets: rem.TURNSecrets}
		rem.RelayURLs = rl.URLs
		rem.Stats = append(rem.Stats, rl.Stats)
	}
	hb, err := rem.Connect(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if rl != nil {
		ip, err := relayPublicIP(ctx, cfg, hb.ObservedIP)
		if err != nil {
			return err
		}
		rl.PublicIP = ip
		if err := rl.Start(); err != nil {
			return err
		}
		defer rl.Close()
		if err := rem.Refresh(ctx); err != nil {
			slog.Warn("heartbeat failed", "err", err)
		}
	}

	// Follow the control server's version so the whole installation is updated from
	// one place. Attempts are spaced out so a broken release is not retried in a loop.
	var lastTry time.Time
	rem.OnUpdate = func(v, channel string) {
		if time.Since(lastTry) < 15*time.Minute {
			return
		}
		lastTry = time.Now()
		slog.Info("control server runs another version, updating", "from", version.Version, "to", v)
		uctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if _, err := upd.ApplyVersion(uctx, v); err != nil {
			slog.Error("update failed", "to", v, "err", err)
			return
		}
		slog.Info("update installed, restarting", "version", v)
		stop()
	}

	fatal := make(chan error, 1)
	go rem.Run(ctx, func(err error) {
		fatal <- err
		stop()
	})

	// Health check for local monitoring only; nothing is exposed in plain text.
	_, port, _ := net.SplitHostPort(cfg.Listen)
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: net.JoinHostPort("127.0.0.1", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("health endpoint", "err", err)
		}
	}()
	slog.Info("running as remote server", "control", cfg.Join.ControlURL, "node_id", ident.ID, "roles", cfg.Roles)
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	select {
	case err := <-fatal:
		return err
	default:
		return nil
	}
}

func apiRecord(a *api.API, kind string, run *backup.Run, err error) {
	a.RecordBackup(context.Background(), audit.Actor{}, kind, run, err)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
