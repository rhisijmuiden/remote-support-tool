package agents

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StartParams describes a new remote session for the session log.
type StartParams struct {
	DeviceID  int64
	UserID    int64
	Kind      string // unattended, attended, quick
	Transport string
	ViewerIP  string
	Record    bool // the technician asked to record
}

// SessionLog stores the history of remote sessions. The session audit package can replace
// the default implementation; it then also decides whether a session must be recorded.
type SessionLog interface {
	Start(ctx context.Context, p StartParams) (id int64, mustRecord bool, err error)
	SetTransport(ctx context.Context, id int64, transport string) error
	End(ctx context.Context, id int64, reason string) error
}

// DBSessionLog writes remote_sessions directly. It records when the technician asks.
type DBSessionLog struct{ DB *pgxpool.Pool }

func (l *DBSessionLog) Start(ctx context.Context, p StartParams) (int64, bool, error) {
	var id int64
	err := l.DB.QueryRow(ctx, `INSERT INTO remote_sessions(device_id, user_id, kind, transport) VALUES($1,$2,$3,NULLIF($4,'')) RETURNING id`,
		p.DeviceID, p.UserID, p.Kind, p.Transport).Scan(&id)
	return id, p.Record, err
}

func (l *DBSessionLog) SetTransport(ctx context.Context, id int64, t string) error {
	_, err := l.DB.Exec(ctx, `UPDATE remote_sessions SET transport=$2 WHERE id=$1`, id, t)
	return err
}

func (l *DBSessionLog) End(ctx context.Context, id int64, reason string) error {
	_, err := l.DB.Exec(ctx, `UPDATE remote_sessions SET ended_at=now(), end_reason=$2 WHERE id=$1 AND ended_at IS NULL`, id, reason)
	return err
}
