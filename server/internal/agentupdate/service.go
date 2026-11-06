// SPDX-License-Identifier: AGPL-3.0-only

package agentupdate

import (
	"bufio"
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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/mod/semver"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

// MaxFileBytes limits one agent executable.
const MaxFileBytes = 300 << 20

// CodeSigner signs a Windows executable; ok is false when no certificate is configured.
type CodeSigner interface {
	Sign(ctx context.Context, exe []byte, description string) (out []byte, ok bool, err error)
}

type Service struct {
	DB      *pgxpool.Pool
	Store   *settings.Store
	Signer  *Signer
	Code    CodeSigner
	DataDir string
	// GitHub releases to import agents from (same repository as the server by default).
	APIURL, Repo, Token string
	HTTP                *http.Client
	// Keep is how many releases per product/arch/channel stay after a sync (pinned ones always stay).
	Keep int
	// Changed is called after something changed that can give devices a new
	// update (policy, exception, new release), so online agents can check now.
	Changed func()

	syncMu sync.Mutex
}

func (s *Service) changed() {
	if s.Changed != nil {
		s.Changed()
	}
}

func (s *Service) dir() string { return filepath.Join(s.DataDir, "agents") }

// Path is where the file with this SHA-256 is stored.
func (s *Service) Path(sum string) string { return filepath.Join(s.dir(), sum+".exe") }

var sumRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ---- policy ----

func (s *Service) Policy(ctx context.Context) (Policy, error) {
	p := DefaultPolicy()
	_, err := s.Store.Get(ctx, policySettings, &p)
	return p, err
}

func (s *Service) SetPolicy(ctx context.Context, p Policy) error {
	if p.RolloutPercent < 1 || p.RolloutPercent > 100 {
		return errors.New("invalid_rollout")
	}
	for _, v := range []*string{&p.ProductionVersion, &p.TestVersion} {
		if *v != "" {
			if *v = CanonVersion(*v); *v == "" {
				return errors.New("invalid_version")
			}
		}
	}
	if err := s.Store.Put(ctx, policySettings, p); err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Service) Overrides(ctx context.Context) ([]Override, error) {
	rows, err := s.DB.Query(ctx, `SELECT o.customer_id, c.name, o.mode, coalesce(o.version,''), o.updated_at
		FROM customer_update_policies o JOIN customers c ON c.id=o.customer_id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Override, error) {
		var o Override
		err := r.Scan(&o.CustomerID, &o.Customer, &o.Mode, &o.Version, &o.UpdatedAt)
		return o, err
	})
}

func (s *Service) override(ctx context.Context, customerID *int64) (*Override, error) {
	if customerID == nil {
		return nil, nil
	}
	var o Override
	err := s.DB.QueryRow(ctx, `SELECT customer_id, mode, coalesce(version,'') FROM customer_update_policies WHERE customer_id=$1`, *customerID).
		Scan(&o.CustomerID, &o.Mode, &o.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// SetOverride stores a customer exception; mode "" removes it.
func (s *Service) SetOverride(ctx context.Context, customerID int64, mode, ver string) error {
	switch mode {
	case "":
		_, err := s.DB.Exec(ctx, `DELETE FROM customer_update_policies WHERE customer_id=$1`, customerID)
		if err == nil {
			s.changed()
		}
		return err
	case "off":
		ver = ""
	case "pin":
		if ver = CanonVersion(ver); ver == "" {
			return errors.New("invalid_version")
		}
	default:
		return errors.New("invalid_mode")
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO customer_update_policies(customer_id, mode, version) VALUES($1,$2,NULLIF($3,''))
		ON CONFLICT(customer_id) DO UPDATE SET mode=EXCLUDED.mode, version=EXCLUDED.version, updated_at=now()`, customerID, mode, ver)
	if err == nil {
		s.changed()
	}
	return err
}

// ---- releases ----

const releaseCols = `id, product, version, os, arch, channel, size_bytes, sha256, source, coalesce(notes,''), released_at, created_at`

func scanRelease(r pgx.CollectableRow) (Release, error) {
	var x Release
	err := r.Scan(&x.ID, &x.Product, &x.Version, &x.OS, &x.Arch, &x.Channel, &x.Size, &x.SHA256, &x.Source, &x.Notes, &x.ReleasedAt, &x.CreatedAt)
	return x, err
}

func (s *Service) Releases(ctx context.Context) ([]Release, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+releaseCols+` FROM agent_releases ORDER BY product, released_at DESC, version DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRelease)
}

func (s *Service) releasesFor(ctx context.Context, product, osName, arch string) ([]Release, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE product=$1 AND os=$2 AND arch=$3`, product, osName, arch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRelease)
}

// Release returns one release by id.
func (s *Service) Release(ctx context.Context, id int64) (*Release, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRelease)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("not_found")
	}
	return &r, err
}

// Newest returns the newest release of product/arch that a build on channel would install.
func (s *Service) Newest(ctx context.Context, product, arch, channel string) (*Release, error) {
	rs, err := s.releasesFor(ctx, product, "windows", arch)
	if err != nil {
		return nil, err
	}
	d := Decide(Policy{Enabled: true, RolloutPercent: 100}, nil, 0, "", channel, rs)
	if d.Release == nil {
		return nil, errors.New("no_agent_release")
	}
	return d.Release, nil
}

// Read returns the bytes of a release.
func (s *Service) Read(r *Release) ([]byte, error) {
	b, err := os.ReadFile(s.Path(r.SHA256))
	if err != nil {
		return nil, fmt.Errorf("agent file missing: %w", err)
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != r.SHA256 {
		return nil, errors.New("agent file is damaged (checksum mismatch)")
	}
	return b, nil
}

// ImportInput describes a file to add as a release.
type ImportInput struct {
	Product, Version, Arch, Channel, Source, Notes string
	ReleasedAt                                     time.Time
}

// Import stores exe as a release. Unsigned files are code-signed when a certificate is configured.
func (s *Service) Import(ctx context.Context, in ImportInput, exe []byte) (*Release, error) {
	if in.Product != "agent" && in.Product != "quick" {
		return nil, errors.New("invalid_product")
	}
	if in.Arch != "amd64" && in.Arch != "arm64" {
		return nil, errors.New("invalid_arch")
	}
	if in.Channel != "production" && in.Channel != "test" {
		return nil, errors.New("invalid_channel")
	}
	if in.Version = CanonVersion(in.Version); in.Version == "" {
		return nil, errors.New("invalid_version")
	}
	if len(exe) == 0 || len(exe) > MaxFileBytes {
		return nil, errors.New("invalid_file")
	}
	signed, err := builder.IsSigned(exe)
	if err != nil {
		return nil, errors.New("not_windows_exe")
	}
	if _, err := builder.Extract(exe); err == nil {
		return nil, errors.New("file_is_client_build")
	}
	if !signed && s.Code != nil {
		desc := "RHIS Remote agent"
		if in.Product == "quick" {
			desc = "RHIS Remote QuickSupport"
		}
		out, ok, err := s.Code.Sign(ctx, exe, desc)
		if err != nil {
			return nil, err
		}
		if ok {
			exe = out
		}
	}
	h := sha256.Sum256(exe)
	sum := hex.EncodeToString(h[:])
	if err := os.MkdirAll(s.dir(), 0o750); err != nil {
		return nil, err
	}
	if err := writeAtomic(s.Path(sum), exe); err != nil {
		return nil, err
	}
	if in.ReleasedAt.IsZero() {
		in.ReleasedAt = time.Now()
	}
	rows, err := s.DB.Query(ctx, `INSERT INTO agent_releases(product, version, os, arch, channel, size_bytes, sha256, source, notes, released_at)
		VALUES($1,$2,'windows',$3,$4,$5,$6,$7,NULLIF($8,''),$9) RETURNING `+releaseCols,
		in.Product, in.Version, in.Arch, in.Channel, len(exe), sum, in.Source, in.Notes, in.ReleasedAt)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRelease)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return nil, errors.New("release_exists")
	}
	if err == nil {
		s.changed()
	}
	return &r, err
}

// Delete removes a release that no policy pins.
func (s *Service) Delete(ctx context.Context, id int64) error {
	r, err := s.Release(ctx, id)
	if err != nil {
		return err
	}
	if s.pinned(ctx)[r.Version] {
		return errors.New("release_pinned")
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM agent_releases WHERE id=$1`, id); err != nil {
		return err
	}
	s.removeUnused(ctx, r.SHA256)
	return nil
}

func (s *Service) removeUnused(ctx context.Context, sum string) {
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM agent_releases WHERE sha256=$1`, sum).Scan(&n); err == nil && n == 0 {
		os.Remove(s.Path(sum))
	}
}

func (s *Service) pinned(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if p, err := s.Policy(ctx); err == nil {
		out[p.ProductionVersion], out[p.TestVersion] = true, true
	}
	if os, err := s.Overrides(ctx); err == nil {
		for _, o := range os {
			out[o.Version] = true
		}
	}
	delete(out, "")
	return out
}

// prune keeps the newest Keep releases per product/arch/channel.
func (s *Service) prune(ctx context.Context) {
	keep := s.Keep
	if keep <= 0 {
		keep = 5
	}
	rs, err := s.Releases(ctx)
	if err != nil {
		return
	}
	pins := s.pinned(ctx)
	groups := map[string][]Release{}
	for _, r := range rs {
		k := r.Product + "/" + r.Arch + "/" + r.Channel
		groups[k] = append(groups[k], r)
	}
	for _, g := range groups {
		sortReleases(g)
		for _, r := range g[min(keep, len(g)):] {
			if !pins[r.Version] {
				s.Delete(ctx, r.ID)
			}
		}
	}
}

func sortReleases(g []Release) {
	for i := 1; i < len(g); i++ {
		for j := i; j > 0 && semver.Compare(CanonVersion(g[j].Version), CanonVersion(g[j-1].Version)) > 0; j-- {
			g[j], g[j-1] = g[j-1], g[j]
		}
	}
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---- GitHub import ----

// Asset names the release workflow publishes, and what they are.
var assetRe = regexp.MustCompile(`^rhis-(agent|quick)_windows_(amd64|arm64)\.exe$`)

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// SyncResult tells the portal what a sync did.
type SyncResult struct {
	Imported []Release `json:"imported"`
	Skipped  int       `json:"skipped"`
	Errors   []string  `json:"errors"`
}

func (s *Service) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "rhis-remote/"+version.Version)
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: file too large", url)
	}
	return b, nil
}

// Sync imports the newest production and test release of every agent asset from GitHub.
func (s *Service) Sync(ctx context.Context) (*SyncResult, error) {
	if !s.syncMu.TryLock() {
		return nil, errors.New("sync_busy")
	}
	defer s.syncMu.Unlock()
	body, err := s.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=50", strings.TrimRight(s.APIURL, "/"), s.Repo), 16<<20)
	if err != nil {
		return nil, err
	}
	var rels []ghRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, err
	}
	// Pick the newest release per asset and channel.
	type pick struct {
		rel   *ghRelease
		asset string
		url   string
	}
	best := map[string]pick{}
	for i := range rels {
		r := &rels[i]
		if r.Draft || CanonVersion(r.TagName) == "" {
			continue
		}
		ch := "production"
		if r.Prerelease {
			ch = "test"
		}
		for _, a := range r.Assets {
			if !assetRe.MatchString(a.Name) {
				continue
			}
			k := a.Name + "/" + ch
			if cur, ok := best[k]; !ok || semver.Compare(CanonVersion(r.TagName), CanonVersion(cur.rel.TagName)) > 0 {
				best[k] = pick{r, a.Name, a.URL}
			}
		}
	}
	res := &SyncResult{Imported: []Release{}, Errors: []string{}}
	for k, p := range best {
		m := assetRe.FindStringSubmatch(p.asset)
		ch := k[strings.LastIndex(k, "/")+1:]
		var exists bool
		s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_releases WHERE product=$1 AND version=$2 AND arch=$3)`,
			m[1], CanonVersion(p.rel.TagName), m[2]).Scan(&exists)
		if exists {
			res.Skipped++
			continue
		}
		rel, err := s.importAsset(ctx, p.rel, p.asset, p.url, m[1], m[2], ch)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %v", p.rel.TagName, p.asset, err))
			continue
		}
		res.Imported = append(res.Imported, *rel)
	}
	s.prune(ctx)
	return res, nil
}

func (s *Service) importAsset(ctx context.Context, r *ghRelease, asset, url, product, arch, ch string) (*Release, error) {
	want, err := s.checksum(ctx, r, asset)
	if err != nil {
		return nil, err
	}
	exe, err := s.get(ctx, url, MaxFileBytes)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(exe)
	if got := hex.EncodeToString(h[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	notes := r.Body
	if len(notes) > 4000 {
		notes = notes[:4000]
	}
	return s.Import(ctx, ImportInput{Product: product, Version: r.TagName, Arch: arch, Channel: ch, Source: "github",
		Notes: notes, ReleasedAt: r.PublishedAt}, exe)
}

// checksum looks for the asset's SHA-256 in checksums.txt or <asset>.sha256 of the same release.
func (s *Service) checksum(ctx context.Context, r *ghRelease, asset string) (string, error) {
	for _, a := range r.Assets {
		if a.Name != "checksums.txt" && a.Name != asset+".sha256" {
			continue
		}
		b, err := s.get(ctx, a.URL, 1<<20)
		if err != nil {
			return "", err
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 1 && sumRe.MatchString(strings.ToLower(f[0])) &&
				(len(f) == 1 && a.Name != "checksums.txt" || len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset) {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	return "", errors.New("no checksum published for " + asset)
}

// RunSync syncs every interval until ctx ends (scheduler role).
func (s *Service) RunSync(ctx context.Context, every time.Duration) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if res, err := s.Sync(ctx); err != nil {
			slog.Warn("agent release sync", "err", err)
		} else if len(res.Imported) > 0 || len(res.Errors) > 0 {
			slog.Info("agent release sync", "imported", len(res.Imported), "errors", res.Errors)
		}
		t.Reset(every)
	}
}

// ---- builder.Sources ----

// AgentFor returns the newest release of product for arch on channel, with its bytes.
func (s *Service) AgentFor(ctx context.Context, product, arch, channel string) (*builder.Release, error) {
	r, err := s.Newest(ctx, product, arch, channel)
	if err != nil {
		return nil, err
	}
	b, err := s.Read(r)
	if err != nil {
		return nil, err
	}
	return &builder.Release{ID: r.ID, Version: r.Version, Bytes: b}, nil
}

// UpdateKeys are the public keys embedded in new builds.
func (s *Service) UpdateKeys() []string { return []string{s.Signer.PublicKey()} }

// Sign code-signs a generated installer when a certificate is configured.
func (s *Service) Sign(ctx context.Context, exe []byte, description string) ([]byte, bool, error) {
	if s.Code == nil {
		return exe, false, nil
	}
	return s.Code.Sign(ctx, exe, description)
}
