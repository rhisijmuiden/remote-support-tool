// Package backup makes full and incremental backups of the database and the data folder,
// uploads them to FTP, FTPS or SCP, and restores them from there.
//
// Each backup is one archive (tar + gzip, optionally encrypted with age):
//
//	manifest.json   what is in this backup and which backup it builds on
//	database.dump   complete database dump (pg_dump custom format)
//	files/...       data files; in an incremental backup only the files that changed
//
// The database is always dumped whole because it is small next to recordings and installers.
// The data folder is incremental: a file is included when its SHA-256 differs from the previous backup.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

const SettingsKey = "backup"

type Settings struct {
	Target TargetConfig `json:"target"`
	// Cron expressions (5 fields). Empty disables the schedule.
	FullSchedule        string `json:"full_schedule"`
	IncrementalSchedule string `json:"incremental_schedule"`
	// KeepFull is how many full backups (with their incrementals) to keep on the target. 0 keeps all.
	KeepFull int `json:"keep_full"`
	// Passphrase encrypts archives with age (scrypt). Stored sealed. Empty means no encryption.
	Passphrase string `json:"passphrase,omitempty"`
}

func Defaults() Settings {
	return Settings{Target: TargetConfig{Type: "scp", Dir: "rhis-remote-backups"}, FullSchedule: "0 3 * * 0", IncrementalSchedule: "0 3 * * 1-6", KeepFull: 4}
}

type FileInfo struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Format    int                 `json:"format"`
	Kind      string              `json:"kind"`
	Name      string              `json:"name"`
	Parent    string              `json:"parent,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	Version   string              `json:"version"`
	Files     map[string]FileInfo `json:"files"`    // complete state of the data folder at this point
	Included  []string            `json:"included"` // files stored in this archive
}

type Run struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	ParentName *string    `json:"parent_name"`
	Target     string     `json:"target"`
	Status     string     `json:"status"`
	SizeBytes  *int64     `json:"size_bytes"`
	Error      *string    `json:"error"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type Service struct {
	DB          *pgxpool.Pool
	Store       *settings.Store
	Box         *secret.Box
	DatabaseURL string
	DataDir     string
	PgDump      string // defaults to "pg_dump"
	PgRestore   string // defaults to "pg_restore"
	// Dial replaces Open in tests.
	Dial func(ctx context.Context, t TargetConfig, password, key string, seenHostKey *string) (Target, error)
	mu   sync.Mutex
}

var ErrBusy = errors.New("backup_busy")

func (s *Service) Load(ctx context.Context) (Settings, error) {
	st := Defaults()
	_, err := s.Store.Get(ctx, SettingsKey, &st)
	return st, err
}

// Save stores settings; empty secret fields keep their current value.
func (s *Service) Save(ctx context.Context, in Settings) error {
	old, err := s.Load(ctx)
	if err != nil {
		return err
	}
	seal := func(nv *string, ov string) error {
		if *nv == "" {
			*nv = ov
			return nil
		}
		v, err := s.Box.Seal(*nv)
		*nv = v
		return err
	}
	if err := seal(&in.Target.Password, old.Target.Password); err != nil {
		return err
	}
	if err := seal(&in.Target.PrivateKey, old.Target.PrivateKey); err != nil {
		return err
	}
	if err := seal(&in.Passphrase, old.Passphrase); err != nil {
		return err
	}
	// A different host means a different host key.
	if in.Target.Host != old.Target.Host || in.Target.Type != old.Target.Type {
		if in.Target.HostKey == old.Target.HostKey {
			in.Target.HostKey = ""
		}
	}
	return s.Store.Put(ctx, SettingsKey, in)
}

func (s *Service) open(ctx context.Context, st Settings) (Target, string, error) {
	pw, err := s.Box.Open(st.Target.Password)
	if err != nil {
		return nil, "", err
	}
	key, err := s.Box.Open(st.Target.PrivateKey)
	if err != nil {
		return nil, "", err
	}
	var seen string
	dial := s.Dial
	if dial == nil {
		dial = Open
	}
	t, err := dial(ctx, st.Target, pw, key, &seen)
	return t, seen, err
}

// Test connects to the target and lists it. For SCP it pins the host key on first use.
func (s *Service) Test(ctx context.Context) (hostKey string, err error) {
	st, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	t, seen, err := s.open(ctx, st)
	if err != nil {
		return seen, err
	}
	defer t.Close()
	if _, err := t.List(ctx); err != nil {
		return seen, err
	}
	if st.Target.Type == "scp" && st.Target.HostKey == "" && seen != "" {
		st.Target.HostKey = seen
		if err := s.Store.Put(ctx, SettingsKey, st); err != nil {
			return seen, err
		}
	}
	return seen, nil
}

func (s *Service) ListRemote(ctx context.Context) ([]RemoteFile, error) {
	st, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	t, _, err := s.open(ctx, st)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	return t.List(ctx)
}

func (s *Service) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, kind, name, parent_name, target, status, size_bytes, error, started_at, finished_at
		FROM backup_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.ParentName, &r.Target, &r.Status, &r.SizeBytes, &r.Error, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var nameRe = regexp.MustCompile(`^rhis-remote-(\d{8}T\d{6}Z)-(full|incremental)\.tar\.gz(\.age)?$`)

func isBackupName(n string) bool { return nameRe.MatchString(n) }

func (s *Service) tool(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// Run makes a backup. kind is "full" or "incremental"; an incremental without a previous
// successful backup on the same target becomes a full backup.
func (s *Service) Run(ctx context.Context, kind string) (*Run, error) {
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	defer s.mu.Unlock()
	// In a cluster only one node may run a backup at a time.
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(727002)`).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrBusy
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727002)`)

	st, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	if st.Target.Host == "" {
		return nil, errors.New("backup_not_configured")
	}
	label := st.Target.Label()

	var parent *Manifest
	if kind == "incremental" {
		var raw []byte
		err := s.DB.QueryRow(ctx, `SELECT manifest FROM backup_runs WHERE status='ok' AND target=$1 ORDER BY started_at DESC LIMIT 1`, label).Scan(&raw)
		if err == nil {
			parent = &Manifest{}
			if json.Unmarshal(raw, parent) != nil {
				parent = nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if parent == nil {
			kind = "full"
		}
	} else {
		kind = "full"
	}

	now := time.Now().UTC()
	name := fmt.Sprintf("rhis-remote-%s-%s.tar.gz", now.Format("20060102T150405Z"), kind)
	if st.Passphrase != "" {
		name += ".age"
	}
	var parentName *string
	if parent != nil {
		parentName = &parent.Name
	}
	run := &Run{Kind: kind, Name: name, ParentName: parentName, Target: label, Status: "running", StartedAt: now}
	if err := s.DB.QueryRow(ctx, `INSERT INTO backup_runs(kind, parent_name, name, target, status, started_at) VALUES($1,$2,$3,$4,'running',$5) RETURNING id`,
		kind, parentName, name, label, now).Scan(&run.ID); err != nil {
		return nil, err
	}

	size, man, runErr := s.build(ctx, st, kind, name, parent)
	fin := time.Now().UTC()
	run.FinishedAt = &fin
	if runErr != nil {
		msg := runErr.Error()
		run.Status, run.Error = "failed", &msg
		s.DB.Exec(context.WithoutCancel(ctx), `UPDATE backup_runs SET status='failed', error=$2, finished_at=$3 WHERE id=$1`, run.ID, msg, fin)
		return run, runErr
	}
	mj, _ := json.Marshal(man)
	run.Status, run.SizeBytes = "ok", &size
	if _, err := s.DB.Exec(ctx, `UPDATE backup_runs SET status='ok', size_bytes=$2, manifest=$3, finished_at=$4 WHERE id=$1`, run.ID, size, mj, fin); err != nil {
		return run, err
	}
	if st.KeepFull > 0 {
		if err := s.prune(ctx, st); err != nil {
			slog.Warn("backup retention failed", "err", err)
		}
	}
	return run, nil
}

func (s *Service) build(ctx context.Context, st Settings, kind, name string, parent *Manifest) (int64, *Manifest, error) {
	tmpDir, err := os.MkdirTemp("", "rhis-remote-backup-")
	if err != nil {
		return 0, nil, err
	}
	defer os.RemoveAll(tmpDir)

	dump := filepath.Join(tmpDir, "database.dump")
	cmd := exec.CommandContext(ctx, s.tool(s.PgDump, "pg_dump"), "--format=custom", "--no-owner", "--file="+dump, "--dbname="+s.DatabaseURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, nil, fmt.Errorf("pg_dump: %v: %s", err, strings.TrimSpace(string(out)))
	}

	files, err := scanDir(s.DataDir)
	if err != nil {
		return 0, nil, err
	}
	man := &Manifest{Format: 1, Kind: kind, Name: name, CreatedAt: time.Now().UTC(), Version: version.Version, Files: files}
	if parent != nil {
		man.Parent = parent.Name
	}
	for p, fi := range files {
		if parent == nil || parent.Files[p].SHA256 != fi.SHA256 {
			man.Included = append(man.Included, p)
		}
	}

	archive := filepath.Join(tmpDir, name)
	if err := s.writeArchive(archive, st, man, dump); err != nil {
		return 0, nil, err
	}
	fi, err := os.Stat(archive)
	if err != nil {
		return 0, nil, err
	}

	t, _, err := s.open(ctx, st)
	if err != nil {
		return 0, nil, err
	}
	defer t.Close()
	f, err := os.Open(archive)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	if err := t.Upload(ctx, name, f); err != nil {
		return 0, nil, err
	}
	return fi.Size(), man, nil
}

func scanDir(root string) (map[string]FileInfo, error) {
	out := map[string]FileInfo{}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = FileInfo{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
		return nil
	})
	return out, err
}

func (s *Service) writeArchive(dst string, st Settings, man *Manifest, dump string) (err error) {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	var w io.Writer = out
	if st.Passphrase != "" {
		pass, err := s.Box.Open(st.Passphrase)
		if err != nil {
			return err
		}
		rcp, err := age.NewScryptRecipient(pass)
		if err != nil {
			return err
		}
		aw, err := age.Encrypt(out, rcp)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := aw.Close(); err == nil {
				err = cerr
			}
		}()
		w = aw
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mj, _ := json.MarshalIndent(man, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mj)), ModTime: man.CreatedAt}); err != nil {
		return err
	}
	if _, err := tw.Write(mj); err != nil {
		return err
	}
	if err := addFile(tw, dump, "database.dump"); err != nil {
		return err
	}
	for _, p := range man.Included {
		if err := addFile(tw, filepath.Join(s.DataDir, filepath.FromSlash(p)), "files/"+p); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

// prune keeps the newest KeepFull full backups and every incremental newer than the oldest kept full.
func (s *Service) prune(ctx context.Context, st Settings) error {
	t, _, err := s.open(ctx, st)
	if err != nil {
		return err
	}
	defer t.Close()
	files, err := t.List(ctx) // newest first
	if err != nil {
		return err
	}
	fulls := 0
	cutoff := ""
	for _, f := range files {
		m := nameRe.FindStringSubmatch(f.Name)
		if m != nil && m[2] == "full" {
			fulls++
			if fulls == st.KeepFull {
				cutoff = m[1]
				break
			}
		}
	}
	if cutoff == "" {
		return nil
	}
	for _, f := range files {
		m := nameRe.FindStringSubmatch(f.Name)
		if m != nil && m[1] < cutoff {
			if err := t.Delete(ctx, f.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
