package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// Restore downloads the named backup and every backup it builds on, then restores the
// data folder and the database to the state of that backup. The current data folder is
// kept next to it as <data_dir>.before-restore-<time>.
func (s *Service) Restore(ctx context.Context, name string) error {
	if !isBackupName(name) {
		return errors.New("invalid backup name")
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()
	st, err := s.Load(ctx)
	if err != nil {
		return err
	}
	t, _, err := s.open(ctx, st)
	if err != nil {
		return err
	}
	defer t.Close()

	work, err := os.MkdirTemp(filepath.Dir(filepath.Clean(s.DataDir)), ".rhis-remote-restore-")
	if err != nil {
		// Fall back to the system temp dir when the parent of the data dir is not writable.
		if work, err = os.MkdirTemp("", "rhis-remote-restore-"); err != nil {
			return err
		}
	}
	defer os.RemoveAll(work)

	// Download the chain, newest first, until we reach the full backup.
	var chain []string
	var mans []*Manifest
	for cur := name; cur != ""; {
		if len(chain) > 1000 {
			return errors.New("backup chain too long")
		}
		local := filepath.Join(work, cur)
		if err := download(ctx, t, cur, local); err != nil {
			return fmt.Errorf("download %s: %w", cur, err)
		}
		m, err := s.readManifest(local, st)
		if err != nil {
			return fmt.Errorf("%s: %w", cur, err)
		}
		chain = append(chain, local)
		mans = append(mans, m)
		cur = m.Parent
	}
	target := mans[0]

	// Rebuild the data folder in a staging dir: oldest archive first, newer files overwrite.
	stage := filepath.Join(work, "data")
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return err
	}
	dump := filepath.Join(work, "database.dump")
	for i := len(chain) - 1; i >= 0; i-- {
		wantDump := ""
		if i == 0 {
			wantDump = dump
		}
		if err := s.extract(chain[i], st, stage, wantDump); err != nil {
			return err
		}
	}
	// Remove files that no longer existed at the time of the target backup.
	filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(stage, p)
			if _, ok := target.Files[filepath.ToSlash(rel)]; !ok {
				os.Remove(p)
			}
		}
		return nil
	})

	cmd := exec.CommandContext(ctx, s.tool(s.PgRestore, "pg_restore"), "--clean", "--if-exists", "--no-owner", "--single-transaction", "--dbname="+s.DatabaseURL, dump)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_restore: %v: %s", err, strings.TrimSpace(string(out)))
	}

	if _, err := os.Stat(s.DataDir); err == nil {
		old := s.DataDir + ".before-restore-" + time.Now().UTC().Format("20060102T150405Z")
		for i := 2; ; i++ {
			if _, err := os.Stat(old); errors.Is(err, os.ErrNotExist) {
				break
			}
			old = fmt.Sprintf("%s.before-restore-%s-%d", s.DataDir, time.Now().UTC().Format("20060102T150405Z"), i)
		}
		if err := os.Rename(s.DataDir, old); err != nil {
			return fmt.Errorf("move current data folder: %w", err)
		}
	}
	if err := os.Rename(stage, s.DataDir); err != nil {
		// Different filesystem: copy instead.
		if err := copyTree(stage, s.DataDir); err != nil {
			return err
		}
	}
	return nil
}

func download(ctx context.Context, t Target, name, dst string) error {
	rc, err := t.Download(ctx, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Service) openArchive(path string, st Settings) (*tar.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	var r io.Reader = f
	if strings.HasSuffix(path, ".age") {
		pass, err := s.Box.Open(st.Passphrase)
		if err != nil || pass == "" {
			f.Close()
			return nil, nil, errors.New("backup_passphrase_needed")
		}
		id, err := age.NewScryptIdentity(pass)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		if r, err = age.Decrypt(f, id); err != nil {
			f.Close()
			return nil, nil, errors.New("backup_passphrase_wrong")
		}
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return tar.NewReader(gz), func() { gz.Close(); f.Close() }, nil
}

func (s *Service) readManifest(path string, st Settings) (*Manifest, error) {
	tr, done, err := s.openArchive(path, st)
	if err != nil {
		return nil, err
	}
	defer done()
	h, err := tr.Next()
	if err != nil || h.Name != "manifest.json" {
		return nil, errors.New("not a backup archive")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 64<<20)).Decode(&m); err != nil {
		return nil, err
	}
	if m.Format != 1 {
		return nil, fmt.Errorf("unsupported backup format %d", m.Format)
	}
	if m.Parent != "" && !isBackupName(m.Parent) {
		return nil, errors.New("invalid parent name in manifest")
	}
	return &m, nil
}

func (s *Service) extract(path string, st Settings, stage, dumpDst string) error {
	tr, done, err := s.openArchive(path, st)
	if err != nil {
		return err
	}
	defer done()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var dst string
		switch {
		case h.Name == "database.dump" && dumpDst != "":
			dst = dumpDst
		case strings.HasPrefix(h.Name, "files/"):
			rel := filepath.FromSlash(strings.TrimPrefix(h.Name, "files/"))
			dst = filepath.Join(stage, rel)
			// Never write outside the staging folder.
			if !strings.HasPrefix(dst, filepath.Clean(stage)+string(os.PathSeparator)) {
				return fmt.Errorf("unsafe path in backup: %s", h.Name)
			}
		default:
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
