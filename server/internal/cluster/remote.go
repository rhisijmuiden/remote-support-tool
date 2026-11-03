package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

// ErrPermanent marks errors a restart cannot fix. The service exits with status 3 so
// systemd stops restarting it (RestartPreventExitStatus=3).
var ErrPermanent = errors.New("permanent")

// Remote is the client side for a server without a database: it joins a control
// server once and then sends heartbeats over HTTPS.
type Remote struct {
	Cfg     *config.Config
	Self    *Identity
	DataDir string
	// RelayURLs and Stats describe what this server offers.
	RelayURLs func() []string
	Stats     []StatsFunc
	// OnUpdate is called when the control server runs a different version.
	OnUpdate func(version, channel string)
	// AllowInsecure permits a non-https control URL (development only).
	AllowInsecure bool

	http    *http.Client
	started time.Time
	mu      sync.RWMutex
	last    *HeartbeatResponse
}

func (r *Remote) client() (*http.Client, error) {
	if r.http != nil {
		return r.http, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if f := r.Cfg.Join.CAFile; f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("join.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("join.ca_file: no certificates found")
		}
		tc.RootCAs = pool
	}
	r.http = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
	}}
	return r.http, nil
}

func (r *Remote) controlURL() string {
	u := r.Self.ControlURL
	if u == "" {
		u = r.Cfg.Join.ControlURL
	}
	return strings.TrimRight(u, "/")
}

func (r *Remote) post(ctx context.Context, path, bearer string, in, out any) error {
	c, err := r.client()
	if err != nil {
		return err
	}
	u := r.controlURL()
	if !strings.HasPrefix(u, "https://") && !r.AllowInsecure {
		return errors.New("control URL must use https")
	}
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"/api"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "rhis-remote")
	req.Header.Set("User-Agent", "rhis-remote-node/"+version.Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		if e.Error == "invalid_token" {
			return ErrInvalidToken
		}
		return fmt.Errorf("%s%s: %s %s", u, path, resp.Status, e.Error)
	}
	return json.Unmarshal(b, out)
}

// Connect joins the control server when needed and sends the first heartbeat. It
// retries until it succeeds or ctx ends, and fails fast on a rejected token.
func (r *Remote) Connect(ctx context.Context) (*HeartbeatResponse, error) {
	r.started = time.Now()
	for {
		hb, err := r.connectOnce(ctx)
		if err == nil {
			return hb, nil
		}
		if errors.Is(err, ErrInvalidToken) {
			return nil, fmt.Errorf("%w: the join token was rejected (used, expired or revoked); create a new one under Settings > Servers", ErrPermanent)
		}
		if errors.Is(err, ErrUnauthorized) {
			return nil, fmt.Errorf("%w: the control server no longer knows this server (it was removed); join again with a new token", ErrPermanent)
		}
		slog.Warn("cannot reach control server, retrying", "url", r.controlURL(), "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *Remote) connectOnce(ctx context.Context) (*HeartbeatResponse, error) {
	if r.Self.Credential == "" {
		if r.Cfg.Join.Token == "" {
			return nil, errors.New("join.token is required for the first start")
		}
		var jr JoinResponse
		err := r.post(ctx, "/cluster/join", "", JoinRequest{
			Token: r.Cfg.Join.Token, NodeID: r.Self.ID, Name: r.Cfg.Node.Name,
			Region: r.Cfg.Node.Region, Version: version.Version, Roles: r.Cfg.Roles,
		}, &jr)
		if err != nil {
			return nil, err
		}
		r.Self.ID, r.Self.Credential, r.Self.ControlURL = jr.NodeID, jr.Credential, r.Cfg.Join.ControlURL
		if err := r.Self.Save(r.DataDir); err != nil {
			return nil, fmt.Errorf("save node identity: %w", err)
		}
		slog.Info("joined the control server", "url", r.controlURL(), "node_id", jr.NodeID, "roles", jr.Roles)
		r.setLast(&jr.HeartbeatResponse)
	}
	return r.beat(ctx)
}

func (r *Remote) beat(ctx context.Context) (*HeartbeatResponse, error) {
	var urls []string
	if r.RelayURLs != nil {
		urls = r.RelayURLs()
	}
	var hb HeartbeatResponse
	err := r.post(ctx, "/cluster/heartbeat", r.Self.ID+"."+r.Self.Credential, HeartbeatRequest{
		Name: r.Cfg.Node.Name, Region: r.Cfg.Node.Region, Version: version.Version, Roles: r.Cfg.Roles,
		RelayURLs: urls, Stats: mergeStats(SystemStats(r.started), r.Stats), StartedAt: r.started,
	}, &hb)
	if err != nil {
		return nil, err
	}
	r.setLast(&hb)
	return &hb, nil
}

func (r *Remote) setLast(hb *HeartbeatResponse) {
	r.mu.Lock()
	r.last = hb
	r.mu.Unlock()
}

// TURNSecrets returns the secrets from the last heartbeat.
func (r *Remote) TURNSecrets() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.last == nil {
		return nil
	}
	return r.last.TURNSecrets
}

// Run sends heartbeats until ctx ends. When the control server forgets this server,
// fatal is called so the service stops instead of relaying with stale secrets.
func (r *Remote) Run(ctx context.Context, fatal func(error)) {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hb, err := r.beat(ctx)
		if errors.Is(err, ErrUnauthorized) {
			fatal(fmt.Errorf("%w: the control server removed this server", ErrPermanent))
			return
		}
		if err != nil {
			failures++
			if failures == 1 || failures%30 == 0 {
				slog.Warn("heartbeat failed", "url", r.controlURL(), "err", err, "failures", failures)
			}
			continue
		}
		failures = 0
		if r.OnUpdate != nil && hb.Version != "" && hb.Version != version.Version {
			r.OnUpdate(hb.Version, hb.Channel)
		}
	}
}
