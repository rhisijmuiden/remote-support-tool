// Package audit records who did what, for the activity log.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	UserEmail *string         `json:"user_email"`
	Action    string          `json:"action"`
	Target    *string         `json:"target"`
	Details   json.RawMessage `json:"details"`
	IP        *string         `json:"ip"`
}

type Log struct{ DB *pgxpool.Pool }

// Actor identifies who performed an action. A zero Actor means the system.
type Actor struct {
	UserID int64
	Email  string
	IP     string
}

func (l *Log) Record(ctx context.Context, a Actor, action, target string, details any) {
	var d []byte
	if details != nil {
		d, _ = json.Marshal(details)
	}
	var uid any
	if a.UserID != 0 {
		uid = a.UserID
	}
	nz := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	if _, err := l.DB.Exec(context.WithoutCancel(ctx),
		`INSERT INTO audit_log(user_id, user_email, action, target, details, ip) VALUES($1,$2,$3,$4,$5,$6)`,
		uid, nz(a.Email), action, nz(target), d, nz(a.IP)); err != nil {
		slog.Error("audit write failed", "action", action, "err", err)
	}
}

func (l *Log) List(ctx context.Context, limit, offset int) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := l.DB.Query(ctx, `SELECT id, at, user_email, action, target, details, ip FROM audit_log ORDER BY at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.At, &e.UserEmail, &e.Action, &e.Target, &e.Details, &e.IP); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
