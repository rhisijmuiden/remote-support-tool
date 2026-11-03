// Package cluster lets several RHIS Remote servers work together: it keeps the list of
// servers (control servers and relays), hands out relay credentials, elects the server
// that runs scheduled jobs and carries messages between servers.
//
// One server: everything runs in-process and nothing extra is needed.
// Separate relays: relays join a control server over HTTPS with a one-time token.
// Cluster: control servers share PostgreSQL and talk over NATS with mutual TLS.
package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Heartbeat timing. A server that missed three heartbeats counts as offline.
const (
	HeartbeatInterval = 10 * time.Second
	OfflineAfter      = 35 * time.Second
)

// Node is one server as shown in the portal.
type Node struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Region    string         `json:"region"`
	Roles     []string       `json:"roles"`
	Version   string         `json:"version"`
	Address   string         `json:"address"`
	RelayURLs []string       `json:"relay_urls"`
	Stats     map[string]any `json:"stats"`
	Remote    bool           `json:"remote"`
	Draining  bool           `json:"draining"`
	StartedAt time.Time      `json:"started_at"`
	LastSeen  time.Time      `json:"last_seen"`
	CreatedAt time.Time      `json:"created_at"`
	Online    bool           `json:"online"`
	Self      bool           `json:"self"`
}

// Identity is what a server remembers about itself between restarts.
type Identity struct {
	ID string `json:"id"`
	// Credential authenticates a remote server's heartbeats. Only set on relays that
	// joined over HTTPS.
	Credential string `json:"credential,omitempty"`
	ControlURL string `json:"control_url,omitempty"`
}

func identityPath(dataDir string) string { return filepath.Join(dataDir, "node.json") }

// LoadIdentity reads data_dir/node.json, creating a new ID on first start.
func LoadIdentity(dataDir string) (*Identity, error) {
	b, err := os.ReadFile(identityPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		id := &Identity{ID: newID()}
		return id, id.Save(dataDir)
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, err
	}
	if id.ID == "" {
		id.ID = newID()
	}
	return &id, nil
}

// Save writes the identity with owner-only permissions; it may hold a credential.
func (id *Identity) Save(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	tmp := identityPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, identityPath(dataDir))
}

func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "n_" + hex.EncodeToString(b)
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// StatsFunc adds role specific numbers (such as relay allocations) to a heartbeat.
type StatsFunc func() map[string]any

// SystemStats returns process and host load figures for the servers overview.
func SystemStats(started time.Time) map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st := map[string]any{
		"uptime_s":   int64(time.Since(started).Seconds()),
		"goroutines": runtime.NumGoroutine(),
		"mem_mb":     m.Sys >> 20,
		"cpus":       runtime.NumCPU(),
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				st["load1"] = v
			}
		}
	}
	return st
}

func mergeStats(base map[string]any, extra []StatsFunc) map[string]any {
	for _, f := range extra {
		for k, v := range f() {
			base[k] = v
		}
	}
	return base
}
