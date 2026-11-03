// Package settings stores typed JSON settings in the database.
package settings

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ DB *pgxpool.Pool }

// Get loads key into dst. It returns false when the key has never been saved.
func (s *Store) Get(ctx context.Context, key string, dst any) (bool, error) {
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, dst)
}

func (s *Store) Put(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO settings(key, value, updated_at) VALUES($1,$2,now())
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, raw)
	return err
}
