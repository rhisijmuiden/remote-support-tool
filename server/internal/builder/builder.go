// SPDX-License-Identifier: AGPL-3.0-only

package builder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
)

// Release is the agent executable a build starts from.
type Release struct {
	ID      int64
	Version string
	Bytes   []byte
}

// Sources supplies agent executables, update keys and code signing.
type Sources interface {
	// AgentFor returns the newest agent ("agent" or "quick") for arch on channel.
	AgentFor(ctx context.Context, product, arch, channel string) (*Release, error)
	UpdateKeys() []string
	Sign(ctx context.Context, exe []byte, description string) (out []byte, ok bool, err error)
}

type Service struct {
	DB      *pgxpool.Pool
	Box     *secret.Box
	Src     Sources
	DataDir string
}

type Build struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	CustomerID  *int64    `json:"customer_id"`
	Customer    *string   `json:"customer"`
	Mode        string    `json:"mode"`
	ProductName string    `json:"product_name"`
	CompanyName string    `json:"company_name"`
	AccentColor string    `json:"accent_color"`
	HasLogo     bool      `json:"has_logo"`
	Servers     []string  `json:"servers"`
	Channel     string    `json:"channel"`
	Revoked     bool      `json:"revoked"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Files       []File    `json:"files,omitempty"`
}

type File struct {
	ID           int64     `json:"id"`
	BuildID      int64     `json:"build_id"`
	AgentVersion string    `json:"agent_version"`
	Arch         string    `json:"arch"`
	FileName     string    `json:"file_name"`
	Size         int64     `json:"size_bytes"`
	SHA256       string    `json:"sha256"`
	Signed       bool      `json:"signed"`
	CreatedAt    time.Time `json:"created_at"`
	Links        []Link    `json:"links"`
}

type Link struct {
	ID           int64     `json:"id"`
	FileID       int64     `json:"file_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads *int      `json:"max_downloads"`
	Downloads    int       `json:"downloads"`
	Revoked      bool      `json:"revoked"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
}

// Input is what the portal sends to create or change a build.
type Input struct {
	Name        string   `json:"name"`
	CustomerID  *int64   `json:"customer_id"`
	Mode        string   `json:"mode"`
	ProductName string   `json:"product_name"`
	CompanyName string   `json:"company_name"`
	AccentColor string   `json:"accent_color"`
	Servers     []string `json:"servers"`
	Channel     string   `json:"channel"`
}

func (in *Input) clean() error {
	in.Name, in.ProductName, in.CompanyName = strings.TrimSpace(in.Name), strings.TrimSpace(in.ProductName), strings.TrimSpace(in.CompanyName)
	if in.Name == "" || len(in.Name) > 120 {
		return errors.New("build_name_required")
	}
	if in.ProductName == "" || len(in.ProductName) > 60 || len(in.CompanyName) > 80 {
		return errors.New("product_name_required")
	}
	if in.Mode != "install" && in.Mode != "quick" {
		return errors.New("invalid_mode")
	}
	if in.Channel != "production" && in.Channel != "test" {
		return errors.New("invalid_channel")
	}
	if in.AccentColor != "" && !colorRe.MatchString(in.AccentColor) {
		return errors.New("invalid_color")
	}
	s, err := NormalizeServers(in.Servers)
	if err != nil {
		code, _, _ := strings.Cut(err.Error(), ":")
		return errors.New(code)
	}
	in.Servers = s
	return nil
}

func newToken(prefix string) (string, []byte) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := prefix + base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(tok))
	return tok, h[:]
}

const buildCols = `b.id, b.name, b.customer_id, c.name, b.mode, b.product_name, b.company_name, coalesce(b.accent_color,''),
	b.logo_png IS NOT NULL, b.servers, b.channel, b.revoked_at IS NOT NULL, b.created_at, b.updated_at`

func scanBuild(r pgx.CollectableRow) (Build, error) {
	var b Build
	var servers []byte
	err := r.Scan(&b.ID, &b.Name, &b.CustomerID, &b.Customer, &b.Mode, &b.ProductName, &b.CompanyName, &b.AccentColor,
		&b.HasLogo, &servers, &b.Channel, &b.Revoked, &b.CreatedAt, &b.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(servers, &b.Servers)
	}
	return b, err
}

func (s *Service) List(ctx context.Context) ([]Build, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+buildCols+` FROM client_builds b LEFT JOIN customers c ON c.id=b.customer_id ORDER BY b.revoked_at IS NOT NULL, lower(b.name)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanBuild)
}

// Get returns a build with its files and links.
func (s *Service) Get(ctx context.Context, id int64) (*Build, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+buildCols+` FROM client_builds b LEFT JOIN customers c ON c.id=b.customer_id WHERE b.id=$1`, id)
	if err != nil {
		return nil, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, scanBuild)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("not_found")
	}
	if err != nil {
		return nil, err
	}
	if b.Files, err = s.files(ctx, id); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Service) files(ctx context.Context, buildID int64) ([]File, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, build_id, agent_version, arch, file_name, size_bytes, sha256, signed, created_at
		FROM client_build_files WHERE build_id=$1 ORDER BY created_at DESC`, buildID)
	if err != nil {
		return nil, err
	}
	fs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (File, error) {
		var f File
		err := r.Scan(&f.ID, &f.BuildID, &f.AgentVersion, &f.Arch, &f.FileName, &f.Size, &f.SHA256, &f.Signed, &f.CreatedAt)
		f.Links = []Link{}
		return f, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT l.id, l.file_id, l.expires_at, l.max_downloads, l.downloads, l.revoked_at IS NOT NULL, l.created_at
		FROM download_links l JOIN client_build_files f ON f.id=l.file_id WHERE f.build_id=$1 ORDER BY l.created_at DESC`, buildID)
	if err != nil {
		return nil, err
	}
	links, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Link, error) {
		var l Link
		err := r.Scan(&l.ID, &l.FileID, &l.ExpiresAt, &l.MaxDownloads, &l.Downloads, &l.Revoked, &l.CreatedAt)
		l.Active = !l.Revoked && time.Now().Before(l.ExpiresAt) && (l.MaxDownloads == nil || l.Downloads < *l.MaxDownloads)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		for i := range fs {
			if fs[i].ID == l.FileID {
				fs[i].Links = append(fs[i].Links, l)
			}
		}
	}
	return fs, nil
}

// Create adds a build with a fresh enrollment token.
func (s *Service) Create(ctx context.Context, in Input, userID int64) (*Build, error) {
	if err := in.clean(); err != nil {
		return nil, err
	}
	tok, hash := newToken("rbe_")
	sealed, err := s.Box.Seal(tok)
	if err != nil {
		return nil, err
	}
	servers, _ := json.Marshal(in.Servers)
	var id int64
	err = s.DB.QueryRow(ctx, `INSERT INTO client_builds(name, customer_id, mode, product_name, company_name, accent_color, servers, channel,
		enroll_token_hash, enroll_token, created_by) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,NULLIF($11,0)) RETURNING id`,
		in.Name, in.CustomerID, in.Mode, in.ProductName, in.CompanyName, in.AccentColor, servers, in.Channel, hash, sealed, userID).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "customer_id") {
			return nil, errors.New("invalid_customer")
		}
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Update(ctx context.Context, id int64, in Input) error {
	if err := in.clean(); err != nil {
		return err
	}
	servers, _ := json.Marshal(in.Servers)
	t, err := s.DB.Exec(ctx, `UPDATE client_builds SET name=$2, customer_id=$3, mode=$4, product_name=$5, company_name=$6,
		accent_color=NULLIF($7,''), servers=$8, channel=$9, updated_at=now() WHERE id=$1`,
		id, in.Name, in.CustomerID, in.Mode, in.ProductName, in.CompanyName, in.AccentColor, servers, in.Channel)
	if err != nil {
		if strings.Contains(err.Error(), "customer_id") {
			return errors.New("invalid_customer")
		}
		return err
	}
	if t.RowsAffected() == 0 {
		return errors.New("not_found")
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	if _, err := s.DB.Exec(ctx, `DELETE FROM client_builds WHERE id=$1`, id); err != nil {
		return err
	}
	return os.RemoveAll(s.buildDir(id))
}

// SetRevoked stops (or allows again) new installs enrolling with this build's token.
// Devices that already enrolled keep working.
func (s *Service) SetRevoked(ctx context.Context, id int64, revoked bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE client_builds SET revoked_at=CASE WHEN $2 THEN now() ELSE NULL END, updated_at=now() WHERE id=$1`, id, revoked)
	return err
}

// RotateToken gives the build a new enrollment token; installers made before stop enrolling.
func (s *Service) RotateToken(ctx context.Context, id int64) error {
	tok, hash := newToken("rbe_")
	sealed, err := s.Box.Seal(tok)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE client_builds SET enroll_token_hash=$2, enroll_token=$3, updated_at=now() WHERE id=$1`, id, hash, sealed)
	return err
}

// MaxLogoUpload is the largest image accepted before it is re-encoded.
const MaxLogoUpload = 2 << 20

// SetLogo stores a PNG logo. The image is decoded and encoded again, which strips
// metadata and anything that is not a plain image; nil removes the logo.
func (s *Service) SetLogo(ctx context.Context, id int64, raw []byte) error {
	var logo []byte
	if raw != nil {
		if len(raw) > MaxLogoUpload {
			return errors.New("logo_too_large")
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return errors.New("logo_not_png")
		}
		if cfg.Width > 1024 || cfg.Height > 1024 || cfg.Width < 16 || cfg.Height < 16 {
			return errors.New("logo_size")
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			return errors.New("logo_not_png")
		}
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return err
		}
		if buf.Len() > MaxLogoBytes {
			return errors.New("logo_too_large")
		}
		logo = buf.Bytes()
	}
	_, err := s.DB.Exec(ctx, `UPDATE client_builds SET logo_png=$2, updated_at=now() WHERE id=$1`, id, logo)
	return err
}

func (s *Service) Logo(ctx context.Context, id int64) ([]byte, error) {
	var b []byte
	err := s.DB.QueryRow(ctx, `SELECT logo_png FROM client_builds WHERE id=$1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b == nil) {
		return nil, errors.New("not_found")
	}
	return b, err
}

// ResolveEnrollToken maps a build's enrollment token to its customer. It matches agents.EnrollResolver.
func (s *Service) ResolveEnrollToken(ctx context.Context, token string) (customerID *int64, ok bool, err error) {
	if !strings.HasPrefix(token, "rbe_") {
		return nil, false, nil
	}
	h := sha256.Sum256([]byte(token))
	err = s.DB.QueryRow(ctx, `SELECT customer_id FROM client_builds WHERE enroll_token_hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return customerID, true, nil
}

func (s *Service) buildDir(id int64) string {
	return filepath.Join(s.DataDir, "builds", strconv.FormatInt(id, 10))
}

func (s *Service) filePath(buildID, fileID int64) string {
	return filepath.Join(s.buildDir(buildID), strconv.FormatInt(fileID, 10)+".exe")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func fileName(b *Build) string {
	parts := []string{b.ProductName}
	if b.Mode == "quick" {
		parts = append(parts, "QuickSupport")
	} else {
		if b.Customer != nil {
			parts = append(parts, *b.Customer)
		}
		parts = append(parts, "Setup")
	}
	n := strings.Trim(unsafeName.ReplaceAllString(strings.Join(parts, "-"), "-"), "-.")
	if n == "" {
		n = "client"
	}
	return n + ".exe"
}

// Generate makes an installer for a build from the newest agent of its channel.
func (s *Service) Generate(ctx context.Context, id int64, arch string, userID int64) (*File, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("invalid_arch")
	}
	b, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.Revoked {
		return nil, errors.New("build_revoked")
	}
	product := "agent"
	if b.Mode == "quick" {
		product = "quick"
	}
	rel, err := s.Src.AgentFor(ctx, product, arch, b.Channel)
	if err != nil {
		return nil, err
	}
	var sealed string
	var logo []byte
	if err := s.DB.QueryRow(ctx, `SELECT enroll_token, logo_png FROM client_builds WHERE id=$1`, id).Scan(&sealed, &logo); err != nil {
		return nil, err
	}
	tok, err := s.Box.Open(sealed)
	if err != nil {
		return nil, err
	}
	cfg := &ClientConfig{V: 1, BuildID: b.ID, ProductName: b.ProductName, CompanyName: b.CompanyName, LogoPNG: logo,
		AccentColor: b.AccentColor, Servers: b.Servers, EnrollToken: tok, Mode: b.Mode, Channel: b.Channel, UpdateKeys: s.Src.UpdateKeys()}
	out, err := Embed(rel.Bytes, cfg)
	if err != nil {
		return nil, err
	}
	out, signed, err := s.Src.Sign(ctx, out, b.ProductName)
	if err != nil {
		return nil, fmt.Errorf("codesign_failed: %w", err)
	}
	h := sha256.Sum256(out)
	f := File{BuildID: id, AgentVersion: rel.Version, Arch: arch, FileName: fileName(b), Size: int64(len(out)),
		SHA256: hex.EncodeToString(h[:]), Signed: signed, Links: []Link{}}
	if err := os.MkdirAll(s.buildDir(id), 0o750); err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `INSERT INTO client_build_files(build_id, agent_release_id, agent_version, arch, file_name, size_bytes, sha256, signed, created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,0)) RETURNING id, created_at`,
		id, rel.ID, rel.Version, arch, f.FileName, f.Size, f.SHA256, signed, userID).Scan(&f.ID, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.filePath(id, f.ID), out, 0o640); err != nil {
		return nil, err
	}
	return &f, tx.Commit(ctx)
}

// DeleteFile removes one generated installer and its links.
func (s *Service) DeleteFile(ctx context.Context, buildID, fileID int64) error {
	t, err := s.DB.Exec(ctx, `DELETE FROM client_build_files WHERE id=$1 AND build_id=$2`, fileID, buildID)
	if err != nil {
		return err
	}
	if t.RowsAffected() == 0 {
		return errors.New("not_found")
	}
	os.Remove(s.filePath(buildID, fileID))
	return nil
}

// ServeFile sends a generated installer as a download.
func (s *Service) ServeFile(w http.ResponseWriter, r *http.Request, buildID, fileID int64) {
	var name string
	if err := s.DB.QueryRow(r.Context(), `SELECT file_name FROM client_build_files WHERE id=$1 AND build_id=$2`, fileID, buildID).Scan(&name); err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.filePath(buildID, fileID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// CreateLink makes a download link for a file. The token is returned once and only its hash is stored.
func (s *Service) CreateLink(ctx context.Context, buildID, fileID int64, days int, maxDownloads *int, userID int64) (string, error) {
	if days < 1 || days > 90 {
		return "", errors.New("invalid_link_days")
	}
	if maxDownloads != nil && (*maxDownloads < 1 || *maxDownloads > 10000) {
		return "", errors.New("invalid_link_max")
	}
	var ok bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM client_build_files f JOIN client_builds b ON b.id=f.build_id
		WHERE f.id=$1 AND f.build_id=$2 AND b.revoked_at IS NULL)`, fileID, buildID).Scan(&ok); err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("not_found")
	}
	tok, hash := newToken("")
	_, err := s.DB.Exec(ctx, `INSERT INTO download_links(token_hash, file_id, expires_at, max_downloads, created_by)
		VALUES($1,$2,now() + make_interval(days => $3),$4,NULLIF($5,0))`, hash, fileID, days, maxDownloads, userID)
	return tok, err
}

func (s *Service) RevokeLink(ctx context.Context, buildID, linkID int64) error {
	_, err := s.DB.Exec(ctx, `UPDATE download_links l SET revoked_at=now() FROM client_build_files f
		WHERE l.id=$1 AND f.id=l.file_id AND f.build_id=$2`, linkID, buildID)
	return err
}

// PublicDownload serves GET /d/{token}: a download link sent to a customer.
func (s *Service) PublicDownload(w http.ResponseWriter, r *http.Request, token string) {
	if len(token) < 40 || len(token) > 60 {
		http.NotFound(w, r)
		return
	}
	h := sha256.Sum256([]byte(token))
	var buildID, fileID int64
	// Count the download atomically so max_downloads holds under concurrent requests.
	err := s.DB.QueryRow(r.Context(), `UPDATE download_links l SET downloads=downloads+1
		FROM client_build_files f JOIN client_builds b ON b.id=f.build_id
		WHERE l.token_hash=$1 AND f.id=l.file_id AND l.revoked_at IS NULL AND b.revoked_at IS NULL AND l.expires_at > now()
		  AND (l.max_downloads IS NULL OR l.downloads < l.max_downloads)
		RETURNING f.build_id, f.id`, h[:]).Scan(&buildID, &fileID)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		w.Write([]byte("This download link has expired or was revoked. Ask your IT contact for a new link.\n" +
			"Deze downloadlink is verlopen of ingetrokken. Vraag je IT-contactpersoon om een nieuwe link.\n"))
		return
	}
	s.ServeFile(w, r, buildID, fileID)
}
