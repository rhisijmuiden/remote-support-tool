// Package update checks GitHub releases for a newer server version and installs it.
//
// Channels map to branches: "production" follows releases built from main,
// "test" also includes pre-releases built from the test branch.
package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

const SettingsKey = "update"

type Settings struct {
	Channel string `json:"channel"` // "production" or "test"
}

type Release struct {
	Version     string    `json:"version"`
	Name        string    `json:"name"`
	Notes       string    `json:"notes"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Prerelease  bool      `json:"prerelease"`
	assetURL    string
	sumsURL     string
}

type Status struct {
	Current      string    `json:"current"`
	Commit       string    `json:"commit"`
	BuildDate    string    `json:"build_date"`
	BuildChannel string    `json:"build_channel"`
	Channel      string    `json:"channel"`
	Latest       *Release  `json:"latest"`
	Available    bool      `json:"available"`
	IsDowngrade  bool      `json:"is_downgrade"`
	CheckedAt    time.Time `json:"checked_at"`
	CheckError   string    `json:"check_error,omitempty"`
}

type Service struct {
	Store  *settings.Store
	APIURL string
	Repo   string
	Asset  string
	Token  string
	HTTP   *http.Client
	// Executable is the binary to replace; defaults to os.Executable().
	Executable string
	// AgentDir receives the client downloads (agent/ in the release archive), if set.
	AgentDir string

	mu   sync.Mutex
	last *Status
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	st := Settings{Channel: version.Channel}
	_, err := s.Store.Get(ctx, SettingsKey, &st)
	if st.Channel != "production" && st.Channel != "test" {
		st.Channel = "production"
	}
	return st, err
}

func (s *Service) SetChannel(ctx context.Context, ch string) error {
	if ch != "production" && ch != "test" {
		return errors.New("invalid_channel")
	}
	return s.Store.Put(ctx, SettingsKey, Settings{Channel: ch})
}

func (s *Service) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "rhis-remote/"+version.Version)
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check finds the newest release for the configured channel.
func (s *Service) Check(ctx context.Context) (*Status, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	status := &Status{Current: version.Version, Commit: version.Commit, BuildDate: version.BuildDate,
		BuildChannel: version.Channel, Channel: st.Channel, CheckedAt: time.Now().UTC()}
	rel, err := s.latest(ctx, st.Channel)
	if err != nil {
		status.CheckError = err.Error()
	} else if rel != nil {
		status.Latest = rel
		cmp := semver.Compare(rel.Version, version.Version)
		status.Available = cmp > 0 || (cmp != 0 && st.Channel != version.Channel)
		status.IsDowngrade = cmp < 0
	}
	s.mu.Lock()
	s.last = status
	s.mu.Unlock()
	return status, nil
}

func (s *Service) Last() *Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) latest(ctx context.Context, channel string) (*Release, error) {
	resp, err := s.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=50", strings.TrimRight(s.APIURL, "/"), s.Repo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rels []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rels); err != nil {
		return nil, err
	}
	var best *Release
	for _, r := range rels {
		if r.Draft || !semver.IsValid(r.TagName) || (channel == "production" && r.Prerelease) {
			continue
		}
		rel := &Release{Version: r.TagName, Name: r.Name, Notes: r.Body, URL: r.HTMLURL, PublishedAt: r.PublishedAt, Prerelease: r.Prerelease}
		for _, a := range r.Assets {
			switch a.Name {
			case s.Asset:
				rel.assetURL = a.URL
			case "checksums.txt":
				rel.sumsURL = a.URL
			}
		}
		if rel.assetURL == "" || rel.sumsURL == "" {
			continue
		}
		if best == nil || semver.Compare(rel.Version, best.Version) > 0 {
			best = rel
		}
	}
	return best, nil
}

// Apply downloads the latest release for the channel, verifies its SHA-256 checksum and
// replaces the running binary. The caller restarts the process (systemd brings it back up).
func (s *Service) Apply(ctx context.Context) (*Release, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	rel, err := s.latest(ctx, st.Channel)
	if err != nil {
		return nil, err
	}
	if rel == nil {
		return nil, errors.New("no_release")
	}
	if rel.Version == version.Version {
		return nil, errors.New("already_current")
	}
	return rel, applyRelease(ctx, s, rel)
}

func applyRelease(ctx context.Context, s *Service, rel *Release) error {
	want, err := s.checksum(ctx, rel.sumsURL)
	if err != nil {
		return err
	}
	exe := s.Executable
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".rhis-remote-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name())

	resp, err := s.get(ctx, rel.assetURL)
	if err != nil {
		tmp.Close()
		return err
	}
	defer resp.Body.Close()
	h := sha256.New()
	staged, err := extractRelease(io.TeeReader(resp.Body, h), tmp, s.AgentDir)
	defer func() {
		for _, f := range staged {
			os.Remove(f.tmp)
		}
	}()
	if err != nil {
		tmp.Close()
		return err
	}
	// Drain the rest so the hash covers the whole archive.
	io.Copy(h, resp.Body)
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		tmp.Close()
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The archive is verified: put the new client downloads in place.
	for _, f := range staged {
		if err := os.Rename(f.tmp, f.final); err != nil {
			return fmt.Errorf("install %s: %w", filepath.Base(f.final), err)
		}
	}
	// Keep the previous binary for a manual rollback.
	os.Remove(exe + ".previous")
	if err := os.Link(exe, exe+".previous"); err != nil {
		copyFile(exe, exe+".previous")
	}
	return os.Rename(tmp.Name(), exe)
}

func (s *Service) checksum(ctx context.Context, url string) (string, error) {
	resp, err := s.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == s.Asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", errors.New("checksum for asset not found")
}

type stagedFile struct{ tmp, final string }

// extractRelease copies the "rhis-remote" binary from a tar.gz stream to dst, and the files
// under agent/ to temporary files in agentDir (renamed into place once the checksum matches).
func extractRelease(r io.Reader, dst io.Writer, agentDir string) ([]stagedFile, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var staged []stagedFile
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return staged, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(filepath.ToSlash(h.Name), "./")
		switch {
		case name == "rhis-remote":
			if _, err := io.Copy(dst, io.LimitReader(tr, 512<<20)); err != nil {
				return staged, err
			}
			found = true
		case agentDir != "" && strings.HasPrefix(name, "agent/") && !strings.Contains(name[6:], "/") && len(name) > 6:
			// Older installs cannot write the agent folder (systemd ReadWritePaths); the server
			// update still goes ahead and install.sh fixes the folder on its next run.
			os.MkdirAll(agentDir, 0o755)
			f, err := os.CreateTemp(agentDir, ".update-*")
			if err != nil {
				slog.Warn("cannot update the client downloads", "dir", agentDir, "err", err)
				agentDir = ""
				continue
			}
			staged = append(staged, stagedFile{tmp: f.Name(), final: filepath.Join(agentDir, name[6:])})
			_, err = io.Copy(f, io.LimitReader(tr, 256<<20))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return staged, err
			}
			os.Chmod(f.Name(), 0o644)
		}
	}
	// Read to the end of the gzip stream so the caller's hash sees all bytes.
	io.Copy(io.Discard, gz)
	if !found {
		return staged, errors.New("binary not found in release archive")
	}
	return staged, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
