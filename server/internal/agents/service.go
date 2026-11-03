// Package agents is the gateway role: it enrolls Windows agents, keeps their control
// channels open, and pairs a technician's browser with an agent for a remote session.
//
// The wire protocol is documented in docs/agent-protocol.md.
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
)

var (
	ErrNotFound     = errors.New("not_found")
	ErrOffline      = errors.New("device_offline")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInvalidToken = errors.New("invalid_enroll_token")
)

// ICEServer is one STUN or TURN server handed to both peers of a session.
type ICEServer struct {
	URLs       []string `json:"urls" yaml:"urls"`
	Username   string   `json:"username,omitempty" yaml:"username"`
	Credential string   `json:"credential,omitempty" yaml:"credential"`
}

// ICEProvider returns the ICE servers for a new session. The relay role (TURN) can
// provide short-lived credentials per user; StaticICE serves a fixed list from the config.
type ICEProvider interface {
	ICEServers(ctx context.Context, userTag string) ([]ICEServer, error)
}

// StaticICE is an ICEProvider with a fixed list.
type StaticICE []ICEServer

func (s StaticICE) ICEServers(context.Context, string) ([]ICEServer, error) { return s, nil }

// EnrollResolver maps an enrollment token that is not in the enrollment_tokens table
// (for example one embedded in a client build) to a customer. ok=false means unknown.
type EnrollResolver func(ctx context.Context, token string) (customerID *int64, ok bool, err error)

// Service holds the connected agents and the running sessions of this server.
type Service struct {
	DB        *pgxpool.Pool
	Audit     *audit.Log
	ICE       ICEProvider
	Sessions  SessionLog
	Resolvers []EnrollResolver
	// AgentDir holds rhis-agent_windows_amd64.exe for the public download link.
	AgentDir string
	// Quick is written into each downloaded QuickSupport exe (see quick.go).
	Quick QuickOptions
	// PingInterval is how often the server pings each agent (default 25 s).
	PingInterval time.Duration
	// RequestTimeout is how long a session may take to get both sides connected.
	RequestTimeout time.Duration

	mu       sync.Mutex
	conns    map[int64]*agentConn
	sessions map[string]*session
	enrollRL rateLimiter
}

func (s *Service) init() {
	if s.conns == nil {
		s.conns = map[int64]*agentConn{}
		s.sessions = map[string]*session{}
	}
	if s.PingInterval == 0 {
		s.PingInterval = 25 * time.Second
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = 60 * time.Second
	}
	if s.ICE == nil {
		s.ICE = StaticICE{}
	}
	if s.Sessions == nil {
		s.Sessions = &DBSessionLog{DB: s.DB}
	}
}

// Start resets presence on boot: no agent is connected to a server that just started.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	_, err := s.DB.Exec(ctx, `UPDATE devices SET online=false WHERE online`)
	if err == nil {
		_, err = s.DB.Exec(ctx, `UPDATE remote_sessions SET ended_at=now(), end_reason='server_shutdown' WHERE ended_at IS NULL`)
	}
	return err
}

// Online reports whether a device has an open control channel on this server.
func (s *Service) Online(deviceID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[deviceID]
	return ok
}

// Notify writes a JSON message on a device's control channel.
func (s *Service) Notify(deviceID int64, msg any) error {
	s.mu.Lock()
	c := s.conns[deviceID]
	s.mu.Unlock()
	if c == nil {
		return ErrOffline
	}
	return c.send(msg)
}

// Disconnect closes a device's control channel, for example after it was deleted.
func (s *Service) Disconnect(deviceID int64, reason string) {
	s.mu.Lock()
	c := s.conns[deviceID]
	s.mu.Unlock()
	if c != nil {
		c.close(reason)
	}
}

// ---- tokens ----

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

type device struct {
	ID         int64
	Code       string
	Kind       string
	Hostname   string
	CustomerID *int64
}

func (s *Service) deviceForToken(ctx context.Context, tok string) (*device, error) {
	if tok == "" {
		return nil, ErrUnauthorized
	}
	var d device
	err := s.DB.QueryRow(ctx, `SELECT id, device_code, kind, hostname, customer_id FROM devices WHERE token_hash=$1`, hashToken(tok)).
		Scan(&d.ID, &d.Code, &d.Kind, &d.Hostname, &d.CustomerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AuthenticateDevice checks the device token in the Authorization header.
func (s *Service) AuthenticateDevice(r *http.Request) (deviceID int64, customerID *int64, err error) {
	d, err := s.deviceForToken(r.Context(), bearer(r))
	if err != nil {
		return 0, nil, err
	}
	return d.ID, d.CustomerID, nil
}

// NewDeviceCode returns an unused 9-digit device code (stored without spaces).
func NewDeviceCode(ctx context.Context, db *pgxpool.Pool) (string, error) {
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(900_000_000))
		if err != nil {
			return "", err
		}
		code := fmt.Sprintf("%09d", n.Int64()+100_000_000)
		var used bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE device_code=$1)`, code).Scan(&used); err != nil {
			return "", err
		}
		if !used {
			return code, nil
		}
	}
	return "", errors.New("no free device code")
}

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// remoteIP is the client address. The router in front (api.TrustedProxyIP) has already
// replaced RemoteAddr with the real client when the request came through a trusted proxy.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter allows max failures per key within a window.
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *rateLimiter) blocked(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	cut := time.Now().Add(-window)
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = keep
	}
	return len(keep) >= max
}

func (l *rateLimiter) add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.hits[key] = append(l.hits[key], time.Now())
}
