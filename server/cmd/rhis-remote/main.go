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
	"net/http"
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
	"github.com/rhisijmuiden/remote-support-tool/server/internal/codesign"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/mail"
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

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data_dir: %w", err)
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
		Restart: func() {
			slog.Info("restarting after update")
			stop()
		},
	}
	if cfg.HasRole("scheduler") {
		a.Scheduler = &backup.Scheduler{Svc: a.Backup, OnDone: func(kind string, run *backup.Run, err error) {
			// Scheduled runs are logged as the system.
			apiRecord(a, kind, run, err)
		}}
		if err := a.Scheduler.Reload(ctx); err != nil {
			slog.Error("backup schedule", "err", err)
		}
		defer a.Scheduler.Stop()
		if every, err := time.ParseDuration(cfg.Agents.SyncInterval); err == nil && every > 0 {
			go agents.RunSync(ctx, every)
		}
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				if err := a.Sessions.Cleanup(ctx); err != nil && ctx.Err() == nil {
					slog.Error("session cleanup", "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
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
