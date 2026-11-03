// Package dbtest gives each test package its own PostgreSQL database. Packages run in
// parallel, and the backup tests dump and restore the whole shared test database, so
// sharing it (even in separate schemas) makes tests fail at random.
package dbtest

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
)

// Fresh creates (or recreates) the database "<test database>_<suffix>", applies the
// migrations and returns a pool for it. The test is skipped without RHIS_TEST_DATABASE_URL.
func Fresh(t *testing.T, suffix string) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("RHIS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{strings.TrimPrefix(u.Path, "/") + "_" + suffix}.Sanitize()
	admin, err := db.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	_, err = admin.Exec(ctx, `CREATE DATABASE `+name)
	admin.Close()
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + strings.TrimPrefix(u.Path, "/") + "_" + suffix
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
