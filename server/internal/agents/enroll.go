package agents

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
)

// EnrollRequest is what an agent sends the first time it starts.
type EnrollRequest struct {
	EnrollToken  string `json:"enroll_token"`
	MachineID    string `json:"machine_id"`
	Hostname     string `json:"hostname"`
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
}

type EnrollResponse struct {
	DeviceID    int64  `json:"device_id"`
	DeviceCode  string `json:"device_code"`
	DeviceToken string `json:"device_token"`
	// UpdateKeys are the public keys (base64 Ed25519) that sign agent updates,
	// for agents enrolled without a client-builder configuration.
	UpdateKeys []string `json:"update_keys,omitempty"`
	// Channel is the server's release channel, the default for such agents.
	Channel string `json:"channel,omitempty"`
}

func clean(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// resolveToken checks the portal's enrollment tokens and then any registered resolvers.
// A matching portal token is used up by one within tx.
func (s *Service) resolveToken(ctx context.Context, tx pgx.Tx, tok string) (*int64, error) {
	if tok == "" {
		return nil, ErrInvalidToken
	}
	var id int64
	var cust *int64
	err := tx.QueryRow(ctx, `SELECT id, customer_id FROM enrollment_tokens
		WHERE token_hash=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		AND (max_uses IS NULL OR uses < max_uses) FOR UPDATE`, hashToken(tok)).Scan(&id, &cust)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE enrollment_tokens SET uses=uses+1 WHERE id=$1`, id)
		return cust, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	for _, res := range s.Resolvers {
		c, ok, err := res(ctx, tok)
		if err != nil {
			return nil, err
		}
		if ok {
			return c, nil
		}
	}
	return nil, ErrInvalidToken
}

// Enroll registers a machine (or re-registers it, keeping its device row) and returns a new device token.
func (s *Service) Enroll(ctx context.Context, in EnrollRequest, ip string) (*EnrollResponse, error) {
	in.MachineID = clean(in.MachineID, 100)
	in.Hostname = clean(in.Hostname, 100)
	in.OSVersion = clean(in.OSVersion, 200)
	in.AgentVersion = clean(in.AgentVersion, 50)
	if in.MachineID == "" || in.Hostname == "" {
		return nil, errors.New("invalid_input")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	cust, err := s.resolveToken(ctx, tx, in.EnrollToken)
	if err != nil {
		return nil, err
	}
	tok := newToken()
	out := &EnrollResponse{DeviceToken: tok}
	err = tx.QueryRow(ctx, `UPDATE devices SET token_hash=$2, hostname=$3, os_version=$4, agent_version=$5,
		customer_id=COALESCE($6, customer_id), public_ip=$7, enrolled_at=now()
		WHERE kind='agent' AND machine_id=$1 RETURNING id, device_code`,
		in.MachineID, hashToken(tok), in.Hostname, in.OSVersion, in.AgentVersion, cust, ip).Scan(&out.DeviceID, &out.DeviceCode)
	reenrolled := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		code, cerr := NewDeviceCode(ctx, s.DB)
		if cerr != nil {
			return nil, cerr
		}
		err = tx.QueryRow(ctx, `INSERT INTO devices(kind, machine_id, token_hash, device_code, hostname, os_version, agent_version, customer_id, public_ip, enrolled_at)
			VALUES('agent',$1,$2,$3,$4,$5,$6,$7,$8,now()) RETURNING id, device_code`,
			in.MachineID, hashToken(tok), code, in.Hostname, in.OSVersion, in.AgentVersion, cust, ip).Scan(&out.DeviceID, &out.DeviceCode)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// An old control channel of this machine now has an invalid token.
	if reenrolled {
		s.Disconnect(out.DeviceID, "reenrolled")
	}
	s.Audit.Record(ctx, audit.Actor{IP: ip}, "device.enrolled", in.Hostname, map[string]any{"device_id": out.DeviceID, "reenrolled": reenrolled})
	if s.Quick.UpdateKeys != nil {
		out.UpdateKeys = s.Quick.UpdateKeys()
	}
	if s.Quick.Channel != nil {
		out.Channel = s.Quick.Channel(ctx)
	}
	return out, nil
}

func (s *Service) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if s.enrollRL.blocked(ip, 20, 15*time.Minute) {
		fail(w, http.StatusTooManyRequests, "too_many_attempts", nil)
		return
	}
	var in EnrollRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := s.Enroll(r.Context(), in, ip)
	switch {
	case errors.Is(err, ErrInvalidToken):
		s.enrollRL.add(ip)
		fail(w, http.StatusForbidden, "invalid_enroll_token", nil)
	case err != nil && err.Error() == "invalid_input":
		fail(w, http.StatusBadRequest, "invalid_input", nil)
	case err != nil:
		fail(w, 500, "internal", err)
	default:
		writeJSON(w, 200, out)
	}
}

// ---- enrollment tokens in the portal ----

type EnrollToken struct {
	ID         int64      `json:"id"`
	Label      string     `json:"label"`
	CustomerID *int64     `json:"customer_id"`
	Customer   *string    `json:"customer"`
	CreatedBy  *string    `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	MaxUses    *int       `json:"max_uses"`
	Uses       int        `json:"uses"`
	Active     bool       `json:"active"`
	Token      string     `json:"token,omitempty"` // only in the create response
}

func (s *Service) ListTokens(ctx context.Context) ([]EnrollToken, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.label, t.customer_id, c.name, u.name, t.created_at, t.expires_at, t.max_uses, t.uses,
		t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now()) AND (t.max_uses IS NULL OR t.uses < t.max_uses)
		FROM enrollment_tokens t LEFT JOIN customers c ON c.id=t.customer_id LEFT JOIN users u ON u.id=t.created_by
		WHERE t.revoked_at IS NULL ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnrollToken{}
	for rows.Next() {
		var t EnrollToken
		if err := rows.Scan(&t.ID, &t.Label, &t.CustomerID, &t.Customer, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.MaxUses, &t.Uses, &t.Active); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken makes a new enrollment token. The plain token is only returned here.
func (s *Service) CreateToken(ctx context.Context, userID int64, label string, customerID *int64, validDays int, maxUses *int) (*EnrollToken, error) {
	tok := newToken()
	var exp *time.Time
	if validDays > 0 {
		e := time.Now().Add(time.Duration(validDays) * 24 * time.Hour)
		exp = &e
	}
	t := &EnrollToken{Label: clean(label, 100), CustomerID: customerID, ExpiresAt: exp, MaxUses: maxUses, Active: true, Token: tok}
	err := s.DB.QueryRow(ctx, `INSERT INTO enrollment_tokens(token_hash, label, customer_id, created_by, expires_at, max_uses)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id, created_at`, hashToken(tok), t.Label, customerID, userID, exp, maxUses).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) RevokeToken(ctx context.Context, id int64) error {
	ct, err := s.DB.Exec(ctx, `UPDATE enrollment_tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
