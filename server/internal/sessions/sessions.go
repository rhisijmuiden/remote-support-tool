// Package sessions keeps the audit log of remote sessions (who connected to which
// device, when, and what happened) and stores their optional video recordings.
//
// The signalling/gateway code calls Start, AddEvent and End. The browser viewer
// records the incoming video with MediaRecorder and uploads it in numbered chunks
// (AppendChunk, Finish). Every chunk is encrypted at rest with AES-256-GCM.
package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

// Session kinds and recording states, as stored in the database.
const (
	KindUnattended = "unattended"
	KindAttended   = "attended"
	KindQuick      = "quick"

	RecNone       = "none"
	RecRecording  = "recording"
	RecComplete   = "complete"
	RecIncomplete = "incomplete"
	RecDeleted    = "deleted"
)

var (
	ErrNotFound     = errors.New("session_not_found")
	ErrNotRecording = errors.New("not_recording")
	ErrChunkOrder   = errors.New("chunk_out_of_order")
	ErrNoRecording  = errors.New("no_recording")
	ErrPolicy       = errors.New("recording_policy_invalid")
)

// Policy is the recording setting admins choose in the portal.
type Policy struct {
	// Mode is "off" (never record), "optional" (the technician chooses per session)
	// or "always" (every session is recorded).
	Mode string `json:"mode"`
	// RetentionDays deletes recordings older than this. 0 keeps them forever.
	RetentionDays int `json:"retention_days"`
	// ShareDays is the default lifetime of a customer share link.
	ShareDays int `json:"share_days"`
}

func DefaultPolicy() Policy { return Policy{Mode: "optional", RetentionDays: 90, ShareDays: 7} }

func (p Policy) Validate() error {
	switch p.Mode {
	case "off", "optional", "always":
	default:
		return ErrPolicy
	}
	if p.RetentionDays < 0 || p.RetentionDays > 3650 || p.ShareDays < 1 || p.ShareDays > 90 {
		return ErrPolicy
	}
	return nil
}

// ShouldRecord applies the policy to the technician's choice.
func (p Policy) ShouldRecord(requested bool) bool {
	return p.Mode == "always" || (p.Mode == "optional" && requested)
}

const policyKey = "recording"

type Service struct {
	DB    *pgxpool.Pool
	Store *settings.Store
	Rec   *Store
}

func (s *Service) Policy(ctx context.Context) (Policy, error) {
	p := DefaultPolicy()
	_, err := s.Store.Get(ctx, policyKey, &p)
	return p, err
}

func (s *Service) SavePolicy(ctx context.Context, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.Store.Put(ctx, policyKey, p)
}

type Session struct {
	ID           int64      `json:"id"`
	DeviceID     *int64     `json:"device_id"`
	DeviceName   *string    `json:"device_name"`
	CustomerID   *int64     `json:"customer_id"`
	CustomerName *string    `json:"customer_name"`
	UserID       *int64     `json:"user_id"`
	UserEmail    *string    `json:"user_email"`
	UserName     *string    `json:"user_name"`
	Kind         string     `json:"kind"`
	Transport    *string    `json:"transport"`
	ViewerIP     *string    `json:"viewer_ip"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	EndReason    *string    `json:"end_reason"`
	Recording    string     `json:"recording"`
	RecChunks    int        `json:"recording_chunks"`
	RecBytes     int64      `json:"recording_bytes"`
	RecMIME      *string    `json:"recording_mime"`
}

// MustRecord tells the gateway the viewer must record and the agent must show its
// recording banner.
func (s *Session) MustRecord() bool { return s.Recording == RecRecording }

// Recorded reports whether a playable recording exists.
func (s *Session) Recorded() bool {
	return (s.Recording == RecComplete || s.Recording == RecIncomplete || s.Recording == RecRecording) && s.RecChunks > 0
}

type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Details json.RawMessage `json:"details"`
}

type StartParams struct {
	DeviceID  int64
	UserID    int64
	Kind      string
	Transport string // "p2p", "turn" or "websocket"; may be empty and set later
	ViewerIP  string
	// Record is the technician's choice; the policy decides in the end.
	Record bool
}

const cols = `id, device_id, device_name, customer_id, customer_name, user_id, user_email, user_name, kind, transport,
	viewer_ip, started_at, ended_at, end_reason, recording_status, recording_chunks, recording_bytes, recording_mime`

func scan(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.DeviceID, &s.DeviceName, &s.CustomerID, &s.CustomerName, &s.UserID, &s.UserEmail, &s.UserName,
		&s.Kind, &s.Transport, &s.ViewerIP, &s.StartedAt, &s.EndedAt, &s.EndReason, &s.Recording, &s.RecChunks, &s.RecBytes, &s.RecMIME)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nzi(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

// Start opens a session row and returns it. Session.Recording is "recording" when
// the viewer must record (and the agent must show its recording indicator).
func (s *Service) Start(ctx context.Context, p StartParams) (*Session, error) {
	switch p.Kind {
	case KindUnattended, KindAttended, KindQuick:
	default:
		return nil, fmt.Errorf("sessions: unknown kind %q", p.Kind)
	}
	pol, err := s.Policy(ctx)
	if err != nil {
		return nil, err
	}
	rec := RecNone
	if pol.ShouldRecord(p.Record) {
		rec = RecRecording
	}
	sess, err := scan(s.DB.QueryRow(ctx, `
		INSERT INTO remote_sessions(device_id, device_name, customer_id, customer_name, user_id, user_email, user_name,
			kind, transport, viewer_ip, recording_status)
		SELECT d.id, d.hostname, c.id, c.name, u.id, u.email, u.name, $3, $4, $5, $6
		FROM (SELECT 1) one
		LEFT JOIN devices d ON d.id = $1
		LEFT JOIN customers c ON c.id = d.customer_id
		LEFT JOIN users u ON u.id = $2
		RETURNING `+cols, nzi(p.DeviceID), nzi(p.UserID), p.Kind, nz(p.Transport), nz(p.ViewerIP), rec))
	if err != nil {
		return nil, err
	}
	s.AddEvent(ctx, sess.ID, "session.started", map[string]any{"kind": p.Kind, "transport": p.Transport, "recording": rec == RecRecording})
	return sess, nil
}

// AddEvent appends to the session timeline, for example file_transfer, clipboard,
// elevation or chat. Failures are returned but callers usually only log them.
func (s *Service) AddEvent(ctx context.Context, id int64, kind string, details any) error {
	var d []byte
	if details != nil {
		var err error
		if d, err = json.Marshal(details); err != nil {
			return err
		}
	}
	_, err := s.DB.Exec(context.WithoutCancel(ctx), `INSERT INTO remote_session_events(session_id, kind, details) VALUES($1,$2,$3)`, id, kind, d)
	return err
}

// SetTransport records which path the connection ended up using ("p2p", "turn" or
// "websocket"). The viewer may report a change mid-session; each one is logged.
func (s *Service) SetTransport(ctx context.Context, id int64, transport string) error {
	if _, err := s.DB.Exec(ctx, `UPDATE remote_sessions SET transport = $2 WHERE id = $1`, id, nz(transport)); err != nil {
		return err
	}
	return s.AddEvent(ctx, id, "session.transport", map[string]string{"transport": transport})
}

// End closes the session. Calling it twice keeps the first end time.
func (s *Service) End(ctx context.Context, id int64, reason string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE remote_sessions SET ended_at = now(), end_reason = $2 WHERE id = $1 AND ended_at IS NULL`, id, nz(reason))
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		s.AddEvent(ctx, id, "session.ended", map[string]string{"reason": reason})
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id int64) (*Session, error) {
	return scan(s.DB.QueryRow(ctx, `SELECT `+cols+` FROM remote_sessions WHERE id = $1`, id))
}

func (s *Service) Events(ctx context.Context, id int64) ([]Event, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, at, kind, details FROM remote_session_events WHERE session_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type Filter struct {
	DeviceID   int64
	UserID     int64
	CustomerID int64
	From, To   time.Time
	Query      string // matches device, customer or user
	Recorded   bool   // only sessions with a recording
	Active     bool   // only sessions that have not ended
	Limit      int
	Offset     int
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.DeviceID != 0 {
		add("device_id = $%d", f.DeviceID)
	}
	if f.UserID != 0 {
		add("user_id = $%d", f.UserID)
	}
	if f.CustomerID != 0 {
		add("customer_id = $%d", f.CustomerID)
	}
	if !f.From.IsZero() {
		add("started_at >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("started_at < $%d", f.To)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+escapeLike(strings.ToLower(q))+"%")
		n := len(args)
		conds = append(conds, fmt.Sprintf("(lower(coalesce(device_name,'')) LIKE $%[1]d OR lower(coalesce(customer_name,'')) LIKE $%[1]d OR lower(coalesce(user_email,'')) LIKE $%[1]d OR lower(coalesce(user_name,'')) LIKE $%[1]d)", n))
	}
	if f.Recorded {
		conds = append(conds, "recording_status IN ('complete','incomplete','recording') AND recording_chunks > 0")
	}
	if f.Active {
		conds = append(conds, "ended_at IS NULL")
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// List returns one page of sessions, newest first, plus the total that match.
func (s *Service) List(ctx context.Context, f Filter) ([]Session, int, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 50
	}
	where, args := f.where()
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM remote_sessions`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM remote_sessions%s ORDER BY started_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		cols, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		ss, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *ss)
	}
	return out, total, rows.Err()
}
