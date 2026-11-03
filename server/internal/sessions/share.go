package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrShareInvalid = errors.New("share_invalid")
	ErrShareDays    = errors.New("share_days_invalid")
)

type Share struct {
	ID           int64      `json:"id"`
	SessionID    int64      `json:"session_id"`
	CreatedBy    *string    `json:"created_by"`
	Note         *string    `json:"note"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
	Views        int        `json:"views"`
	LastViewedAt *time.Time `json:"last_viewed_at"`
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// CreateShare makes a link token for one recording. The token is only returned here;
// the database keeps its SHA-256.
func (s *Service) CreateShare(ctx context.Context, sessionID, userID int64, email, note string, days int) (string, *Share, error) {
	if days < 1 || days > 90 {
		return "", nil, ErrShareDays
	}
	sess, err := s.Get(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	if !sess.Recorded() {
		return "", nil, ErrNoRecording
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	var sh Share
	err = s.DB.QueryRow(ctx, `INSERT INTO recording_shares(session_id, token_hash, created_by, created_by_email, note, expires_at)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id, session_id, created_by_email, note, created_at, expires_at, revoked_at, views, last_viewed_at`,
		sessionID, hashToken(tok), nzi(userID), nz(email), nz(note), time.Now().Add(time.Duration(days)*24*time.Hour)).
		Scan(&sh.ID, &sh.SessionID, &sh.CreatedBy, &sh.Note, &sh.CreatedAt, &sh.ExpiresAt, &sh.RevokedAt, &sh.Views, &sh.LastViewedAt)
	if err != nil {
		return "", nil, err
	}
	s.AddEvent(ctx, sessionID, "recording.shared", map[string]any{"share_id": sh.ID, "by": email, "expires_at": sh.ExpiresAt})
	return tok, &sh, nil
}

func (s *Service) Shares(ctx context.Context, sessionID int64) ([]Share, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, session_id, created_by_email, note, created_at, expires_at, revoked_at, views, last_viewed_at
		FROM recording_shares WHERE session_id = $1 ORDER BY created_at DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		var sh Share
		if err := rows.Scan(&sh.ID, &sh.SessionID, &sh.CreatedBy, &sh.Note, &sh.CreatedAt, &sh.ExpiresAt, &sh.RevokedAt, &sh.Views, &sh.LastViewedAt); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *Service) RevokeShare(ctx context.Context, sessionID, shareID int64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE recording_shares SET revoked_at = now() WHERE id = $1 AND session_id = $2 AND revoked_at IS NULL`, shareID, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrShareInvalid
	}
	return s.AddEvent(ctx, sessionID, "recording.share_revoked", map[string]int64{"share_id": shareID})
}

// ResolveShare returns the session behind a valid, unexpired, unrevoked token.
// count=true records a view (used when the video itself is fetched).
func (s *Service) ResolveShare(ctx context.Context, tok string, count bool) (*Session, *Share, error) {
	if len(tok) < 40 || len(tok) > 64 {
		return nil, nil, ErrShareInvalid
	}
	var sh Share
	err := s.DB.QueryRow(ctx, `SELECT id, session_id, created_by_email, note, created_at, expires_at, revoked_at, views, last_viewed_at
		FROM recording_shares WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`, hashToken(tok)).
		Scan(&sh.ID, &sh.SessionID, &sh.CreatedBy, &sh.Note, &sh.CreatedAt, &sh.ExpiresAt, &sh.RevokedAt, &sh.Views, &sh.LastViewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrShareInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.Get(ctx, sh.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if !sess.Recorded() {
		return nil, nil, ErrShareInvalid
	}
	if count {
		s.DB.Exec(ctx, `UPDATE recording_shares SET views = views + 1, last_viewed_at = now() WHERE id = $1`, sh.ID)
	}
	return sess, &sh, nil
}
