package sessions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

// testService migrates a private schema so this package can run next to other
// database tests in the same database.
func testService(t *testing.T) *Service {
	url := os.Getenv("RHIS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS sessions_test CASCADE; CREATE SCHEMA sessions_test`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "sessions_test"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	rec, err := NewStore(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: pool, Store: &settings.Store{DB: pool}, Rec: rec}
}

func seed(t *testing.T, s *Service) (dev, user int64) {
	ctx := context.Background()
	var cust int64
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.DB.QueryRow(ctx, `INSERT INTO customers(name) VALUES('Bakkerij Jansen') RETURNING id`).Scan(&cust))
	must(s.DB.QueryRow(ctx, `INSERT INTO devices(customer_id, device_code, hostname) VALUES($1,'123 456 789','KASSA-01') RETURNING id`, cust).Scan(&dev))
	must(s.DB.QueryRow(ctx, `INSERT INTO users(email, name, password_hash, role) VALUES('tech@example.com','Tess Tech','x','technician') RETURNING id`).Scan(&user))
	return
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		mode string
		req  bool
		want bool
	}{{"off", true, false}, {"optional", false, false}, {"optional", true, true}, {"always", false, true}}
	for _, c := range cases {
		if got := (Policy{Mode: c.mode}).ShouldRecord(c.req); got != c.want {
			t.Errorf("%s/%v: got %v", c.mode, c.req, got)
		}
	}
	if (Policy{Mode: "sometimes", ShareDays: 7}).Validate() == nil {
		t.Error("invalid mode accepted")
	}
	if DefaultPolicy().Validate() != nil {
		t.Error("default policy invalid")
	}
}

func TestSessionLifecycleAndRecording(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	dev, user := seed(t, s)

	// Policy "optional" without a request: no recording.
	plain, err := s.Start(ctx, StartParams{DeviceID: dev, UserID: user, Kind: KindUnattended, Transport: "p2p", ViewerIP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.MustRecord() || *plain.DeviceName != "KASSA-01" || *plain.CustomerName != "Bakkerij Jansen" || *plain.UserEmail != "tech@example.com" {
		t.Fatalf("unexpected session %+v", plain)
	}
	if err := s.AppendChunk(ctx, plain.ID, 0, "video/webm", []byte("x")); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("chunk on unrecorded session: %v", err)
	}

	sess, err := s.Start(ctx, StartParams{DeviceID: dev, UserID: user, Kind: KindAttended, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.MustRecord() {
		t.Fatal("expected recording")
	}
	parts := [][]byte{[]byte("webm-header|"), []byte("cluster-1|"), []byte("cluster-2")}
	for i, p := range parts {
		if err := s.AppendChunk(ctx, sess.ID, i, `video/webm;codecs="vp9"`, p); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	// A retried chunk is accepted; a gap is not.
	if err := s.AppendChunk(ctx, sess.ID, 1, "video/webm", []byte("dup")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := s.AppendChunk(ctx, sess.ID, 5, "video/webm", []byte("gap")); !errors.Is(err, ErrChunkOrder) {
		t.Fatalf("gap: %v", err)
	}
	// Chunks are encrypted on disk.
	raw, _ := os.ReadFile(s.Rec.path(sess.ID, 0))
	if bytes.Contains(raw, []byte("webm-header")) {
		t.Fatal("chunk stored in plain text")
	}
	if err := s.SetTransport(ctx, sess.ID, "turn"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, sess.ID, "viewer_closed"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recording != RecComplete || got.RecChunks != 3 || got.EndedAt == nil || *got.Transport != "turn" {
		t.Fatalf("after finish: %+v", got)
	}
	var buf bytes.Buffer
	if err := s.WriteRecording(ctx, got, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "webm-header|cluster-1|cluster-2" {
		t.Fatalf("playback = %q", buf.String())
	}
	if int64(buf.Len()) != got.RecBytes {
		t.Fatalf("bytes %d != %d", buf.Len(), got.RecBytes)
	}
	// Seeking reader, as used for HTTP Range requests.
	rd, err := s.OpenRecording(got)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Size() != got.RecBytes {
		t.Fatalf("reader size %d", rd.Size())
	}
	rd.Seek(10, io.SeekStart) // spans the end of chunk 0 into chunk 1
	part := make([]byte, 6)
	if _, err := io.ReadFull(rd, part); err != nil || string(part) != "r|clus" {
		t.Fatalf("seek read %q %v", part, err)
	}
	rd.Seek(-9, io.SeekEnd)
	if rest, _ := io.ReadAll(rd); string(rest) != "cluster-2" {
		t.Fatalf("tail %q", rest)
	}

	ev, _ := s.Events(ctx, sess.ID)
	kinds := []string{}
	for _, e := range ev {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"session.started", "session.transport", "recording.finished", "session.ended"}
	if len(kinds) != len(want) {
		t.Fatalf("events %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events %v", kinds)
		}
	}

	// Filters.
	list, total, err := s.List(ctx, Filter{Recorded: true})
	if err != nil || total != 1 || list[0].ID != sess.ID {
		t.Fatalf("recorded filter: %v %d %v", err, total, list)
	}
	if _, total, _ := s.List(ctx, Filter{Query: "kassa"}); total != 2 {
		t.Fatalf("query filter total %d", total)
	}
	if _, total, _ := s.List(ctx, Filter{Query: "100%_"}); total != 0 {
		t.Fatalf("like escaping total %d", total)
	}
	if _, total, _ := s.List(ctx, Filter{From: time.Now().Add(time.Hour)}); total != 0 {
		t.Fatalf("from filter total %d", total)
	}

	// Share links.
	tok, sh, err := s.CreateShare(ctx, sess.ID, user, "tech@example.com", "Ticket 42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveShare(ctx, tok, true); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, _, err := s.ResolveShare(ctx, tok[:len(tok)-1]+"A", false); !errors.Is(err, ErrShareInvalid) {
		t.Fatalf("wrong token resolved: %v", err)
	}
	if _, _, err := s.CreateShare(ctx, plain.ID, user, "", "", 7); !errors.Is(err, ErrNoRecording) {
		t.Fatalf("share without recording: %v", err)
	}
	if err := s.RevokeShare(ctx, sess.ID, sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveShare(ctx, tok, false); !errors.Is(err, ErrShareInvalid) {
		t.Fatal("revoked share still works")
	}

	// Deleting the recording keeps the session in the log.
	if err := s.DeleteRecording(ctx, sess.ID, "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, sess.ID)
	if got.Recording != RecDeleted || got.Recorded() {
		t.Fatalf("after delete: %+v", got)
	}
	if _, err := os.Stat(s.Rec.dir(sess.ID)); !os.IsNotExist(err) {
		t.Fatal("recording files left behind")
	}
}

func TestCleanup(t *testing.T) {
	s := testService(t)
	ctx := context.Background()
	dev, user := seed(t, s)
	if err := s.SavePolicy(ctx, Policy{Mode: "always", RetentionDays: 30, ShareDays: 7}); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Start(ctx, StartParams{DeviceID: dev, UserID: user, Kind: KindQuick})
	s.AppendChunk(ctx, old.ID, 0, "video/webm", []byte("old"))
	s.Finish(ctx, old.ID)
	s.DB.Exec(ctx, `UPDATE remote_sessions SET started_at = now() - interval '40 days', ended_at = now() - interval '40 days' WHERE id=$1`, old.ID)

	stale, _ := s.Start(ctx, StartParams{DeviceID: dev, UserID: user, Kind: KindQuick})
	s.AppendChunk(ctx, stale.ID, 0, "video/webm", []byte("stale"))
	s.DB.Exec(ctx, `UPDATE remote_sessions SET ended_at = now() - interval '1 hour' WHERE id=$1`, stale.ID)

	fresh, _ := s.Start(ctx, StartParams{DeviceID: dev, UserID: user, Kind: KindQuick})
	s.AppendChunk(ctx, fresh.ID, 0, "video/webm", []byte("fresh"))

	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{old.ID: RecDeleted, stale.ID: RecIncomplete, fresh.ID: RecRecording} {
		got, _ := s.Get(ctx, id)
		if got.Recording != want {
			t.Errorf("session %d: %s, want %s", id, got.Recording, want)
		}
	}
}
