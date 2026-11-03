package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

// memTarget is an in-memory backup location.
type memTarget struct {
	mu    *sync.Mutex
	files map[string][]byte
}

func (m memTarget) Upload(_ context.Context, name string, r io.Reader) error {
	b, err := io.ReadAll(r)
	m.mu.Lock()
	m.files[name] = b
	m.mu.Unlock()
	return err
}
func (m memTarget) Download(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m memTarget) List(context.Context) ([]RemoteFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RemoteFile
	for n, b := range m.files {
		out = append(out, RemoteFile{Name: n, Size: int64(len(b))})
	}
	sortFiles(out)
	return out, nil
}
func (m memTarget) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	delete(m.files, name)
	m.mu.Unlock()
	return nil
}
func (memTarget) Close() error { return nil }

func testDB(t *testing.T) (*pgxpool.Pool, string) {
	url := os.Getenv("RHIS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

func write(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFullIncrementalRestore(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			pool, url := testDB(t)
			ctx := context.Background()
			box, _ := secret.New("0123456789abcdef0123456789abcdef")
			data := filepath.Join(t.TempDir(), "data")
			target := memTarget{mu: &sync.Mutex{}, files: map[string][]byte{}}
			svc := &Service{DB: pool, Store: &settings.Store{DB: pool}, Box: box, DatabaseURL: url, DataDir: data,
				Dial: func(context.Context, TargetConfig, string, string, *string) (Target, error) { return target, nil }}
			st := Defaults()
			st.Target.Host = "backup.example"
			if encrypted {
				st.Passphrase = "correct horse battery staple"
			}
			if err := svc.Save(ctx, st); err != nil {
				t.Fatal(err)
			}

			write(t, filepath.Join(data, "a.txt"), "one")
			write(t, filepath.Join(data, "rec/b.bin"), "two")
			pool.Exec(ctx, `INSERT INTO customers(name) VALUES('Bakkerij Smit')`)
			full, err := svc.Run(ctx, "full")
			if err != nil {
				t.Fatal(err)
			}

			time.Sleep(1100 * time.Millisecond) // names have second resolution
			write(t, filepath.Join(data, "a.txt"), "one changed")
			os.Remove(filepath.Join(data, "rec/b.bin"))
			write(t, filepath.Join(data, "c.txt"), "three")
			pool.Exec(ctx, `INSERT INTO customers(name) VALUES('Tandarts Wognum')`)
			inc, err := svc.Run(ctx, "incremental")
			if err != nil {
				t.Fatal(err)
			}
			if inc.Kind != "incremental" || inc.ParentName == nil || *inc.ParentName != full.Name {
				t.Fatalf("incremental not chained to full: %+v", inc)
			}
			if encrypted && filepath.Ext(inc.Name) != ".age" {
				t.Fatalf("expected encrypted name, got %s", inc.Name)
			}
			// The incremental archive must not contain the unchanged-but-deleted file.
			m, err := svc.readManifest(writeTemp(t, target.files[inc.Name], inc.Name), st2(svc, ctx))
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(m.Included)
			if len(m.Included) != 2 || m.Included[0] != "a.txt" || m.Included[1] != "c.txt" {
				t.Fatalf("included = %v", m.Included)
			}

			// Wreck the current state, then restore the incremental.
			write(t, filepath.Join(data, "junk.txt"), "x")
			pool.Exec(ctx, `DELETE FROM customers`)
			if err := svc.Restore(ctx, inc.Name); err != nil {
				t.Fatal(err)
			}
			got := func(p string) string { b, _ := os.ReadFile(filepath.Join(data, p)); return string(b) }
			if got("a.txt") != "one changed" || got("c.txt") != "three" || got("rec/b.bin") != "" || got("junk.txt") != "" {
				t.Fatalf("restored files wrong: a=%q c=%q b=%q junk=%q", got("a.txt"), got("c.txt"), got("rec/b.bin"), got("junk.txt"))
			}
			var n int
			pool.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&n)
			if n != 2 {
				t.Fatalf("customers after restore = %d, want 2", n)
			}

			// Restoring the full backup brings back the older state.
			if err := svc.Restore(ctx, full.Name); err != nil {
				t.Fatal(err)
			}
			if got("a.txt") != "one" || got("rec/b.bin") != "two" || got("c.txt") != "" {
				t.Fatalf("full restore wrong: a=%q b=%q c=%q", got("a.txt"), got("rec/b.bin"), got("c.txt"))
			}
		})
	}
}

func st2(s *Service, ctx context.Context) Settings { st, _ := s.Load(ctx); return st }

func writeTemp(t *testing.T, b []byte, name string) string {
	p := filepath.Join(t.TempDir(), name)
	os.WriteFile(p, b, 0o600)
	return p
}

func TestPrune(t *testing.T) {
	pool, _ := testDB(t)
	ctx := context.Background()
	box, _ := secret.New("0123456789abcdef0123456789abcdef")
	target := memTarget{mu: &sync.Mutex{}, files: map[string][]byte{}}
	for _, n := range []string{
		"rhis-remote-20260101T030000Z-full.tar.gz", "rhis-remote-20260102T030000Z-incremental.tar.gz",
		"rhis-remote-20260108T030000Z-full.tar.gz", "rhis-remote-20260109T030000Z-incremental.tar.gz",
		"rhis-remote-20260115T030000Z-full.tar.gz", "unrelated.txt",
	} {
		target.files[n] = []byte("x")
	}
	svc := &Service{DB: pool, Store: &settings.Store{DB: pool}, Box: box,
		Dial: func(context.Context, TargetConfig, string, string, *string) (Target, error) { return target, nil }}
	if err := svc.prune(ctx, Settings{KeepFull: 2}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"rhis-remote-20260101T030000Z-full.tar.gz", "rhis-remote-20260102T030000Z-incremental.tar.gz"} {
		if _, ok := target.files[gone]; ok {
			t.Errorf("%s should be pruned", gone)
		}
	}
	if len(target.files) != 4 {
		t.Errorf("left %d files, want 4", len(target.files))
	}
}
