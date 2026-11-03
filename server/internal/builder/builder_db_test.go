// SPDX-License-Identifier: AGPL-3.0-only

package builder_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/agentupdate"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/builder"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/codesign"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/db"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

// testDB gives this package its own database next to RHIS_TEST_DATABASE_URL, so it can run
// in parallel with the backup tests (which dump and restore their whole database).
func testDB(t *testing.T) *pgxpool.Pool {
	raw := os.Getenv("RHIS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("RHIS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	name := strings.TrimPrefix(u.Path, "/") + "_builder"
	admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	u.Path = "/" + name
	pool, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// windowsExe cross-compiles a tiny Windows program, so signing runs on a real PE file.
func windowsExe(t *testing.T) []byte {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644)
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", "a.exe", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a Windows test binary: %v %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a.exe"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testPFX(t *testing.T) string {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "RHIS Test Code Signing"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pfx, err := pkcs12.Modern.Encode(key, cert, nil, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pfx)
}

func logoPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{255, 107, 87, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

type fakeAuth struct {
	id   int64
	cust *int64
}

func (f fakeAuth) AuthenticateDevice(*http.Request) (int64, *int64, error) { return f.id, f.cust, nil }

func TestBuildFlow(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	box, _ := secret.New("0123456789abcdef0123456789abcdef")
	store := &settings.Store{DB: pool}
	data := t.TempDir()
	signer, err := agentupdate.LoadSigner(ctx, store, box)
	if err != nil {
		t.Fatal(err)
	}
	again, err := agentupdate.LoadSigner(ctx, store, box)
	if err != nil || again.PublicKey() != signer.PublicKey() {
		t.Fatal("signing key not stable across starts")
	}
	code := &codesign.Service{Store: store, Box: box, TempDir: t.TempDir(), Name: "RHIS Remote", URL: "https://remote.example.com"}
	agents := &agentupdate.Service{DB: pool, Store: store, Signer: signer, Code: code, DataDir: data}
	b := &builder.Service{DB: pool, Box: box, Src: agents, DataDir: data}

	var cust int64
	pool.QueryRow(ctx, `INSERT INTO customers(name) VALUES('Bakkerij Smit') RETURNING id`).Scan(&cust)

	// No agent yet: generating fails with a clear code.
	bd, err := b.Create(ctx, builder.Input{Name: "Smit kantoor", CustomerID: &cust, Mode: "install", ProductName: "RHIS Remote",
		CompanyName: "RHIS", AccentColor: "#FF6B57", Servers: []string{"remote.example.com", "https://backup.example.com"}, Channel: "production"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Generate(ctx, bd.ID, "amd64", 0); err == nil || err.Error() != "no_agent_release" {
		t.Fatalf("want no_agent_release, got %v", err)
	}
	if _, err := b.Create(ctx, builder.Input{Name: "x", Mode: "install", ProductName: "x", Servers: []string{"http://plain.example.com"}, Channel: "production"}, 0); err == nil || err.Error() != "server_not_https" {
		t.Fatalf("http server accepted: %v", err)
	}

	exe := windowsExe(t)
	rel, err := agents.Import(ctx, agentupdate.ImportInput{Product: "agent", Version: "0.2.0", Arch: "amd64", Channel: "production", Source: "upload"}, exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agents.Import(ctx, agentupdate.ImportInput{Product: "agent", Version: "0.2.0", Arch: "amd64", Channel: "production", Source: "upload"}, exe); err == nil {
		t.Fatal("duplicate release accepted")
	}
	if _, err := agents.Import(ctx, agentupdate.ImportInput{Product: "agent", Version: "0.2.1", Arch: "amd64", Channel: "production", Source: "upload"}, []byte("MZ nope")); err == nil {
		t.Fatal("non-PE accepted")
	}

	if err := b.SetLogo(ctx, bd.ID, logoPNG(64, 64)); err != nil {
		t.Fatal(err)
	}
	if err := b.SetLogo(ctx, bd.ID, []byte("GIF89a")); err == nil {
		t.Fatal("GIF accepted as logo")
	}
	if err := b.SetLogo(ctx, bd.ID, logoPNG(2000, 20)); err == nil {
		t.Fatal("oversized logo accepted")
	}

	f, err := b.Generate(ctx, bd.ID, "amd64", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Signed || f.AgentVersion != "v0.2.0" || f.FileName != "RHIS-Remote-Bakkerij-Smit-Setup.exe" {
		t.Fatalf("file %+v", f)
	}
	out, _ := os.ReadFile(filepath.Join(data, "builds", itoa(bd.ID), itoa(f.ID)+".exe"))
	cfg, err := builder.Extract(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0] != "https://remote.example.com" || cfg.UpdateKeys[0] != signer.PublicKey() || len(cfg.LogoPNG) == 0 || cfg.Mode != "install" {
		t.Fatalf("config %+v", cfg)
	}
	got, ok, err := b.ResolveEnrollToken(ctx, cfg.EnrollToken)
	if err != nil || !ok || got == nil || *got != cust {
		t.Fatalf("enroll token: %v %v %v", got, ok, err)
	}

	// Download link: works twice, then the limit stops it.
	two := 2
	tok, err := b.CreateLink(ctx, bd.ID, f.ID, 7, &two, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{200, 200, 410} {
		rec := httptest.NewRecorder()
		b.PublicDownload(rec, httptest.NewRequest("GET", "/d/"+tok, nil), tok)
		if rec.Code != want {
			t.Fatalf("download %d: got %d want %d", i, rec.Code, want)
		}
		if want == 200 && !bytes.Equal(rec.Body.Bytes(), out) {
			t.Fatal("downloaded bytes differ")
		}
	}

	// Revoking stops enrollment and links.
	tok2, _ := b.CreateLink(ctx, bd.ID, f.ID, 1, nil, 0)
	b.SetRevoked(ctx, bd.ID, true)
	if _, ok, _ := b.ResolveEnrollToken(ctx, cfg.EnrollToken); ok {
		t.Fatal("revoked build still enrolls")
	}
	rec := httptest.NewRecorder()
	b.PublicDownload(rec, httptest.NewRequest("GET", "/d/"+tok2, nil), tok2)
	if rec.Code != 410 {
		t.Fatalf("revoked link: %d", rec.Code)
	}
	b.SetRevoked(ctx, bd.ID, false)
	b.RotateToken(ctx, bd.ID)
	if _, ok, _ := b.ResolveEnrollToken(ctx, cfg.EnrollToken); ok {
		t.Fatal("old token still valid after rotation")
	}

	// Central update: a device on v0.1.0 is offered v0.2.0 with a manifest that verifies.
	var dev int64
	pool.QueryRow(ctx, `INSERT INTO devices(customer_id, device_code, hostname, agent_version) VALUES($1,'111 222 333','PC-01','v0.1.0') RETURNING id`, cust).Scan(&dev)
	h := &agentupdate.Handler{S: agents, Auth: fakeAuth{dev, &cust}, PublicURL: "https://remote.example.com"}
	srv := httptest.NewServer(mount(h))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/agent/v1/update?os=windows&arch=amd64&version=v0.1.0&channel=production")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("update check: %v %v", err, resp.Status)
	}
	var sg agentupdate.Signed
	decodeJSON(t, resp.Body, &sg)
	m, err := agentupdate.Verify(&sg, cfg.UpdateKeys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v0.2.0" || m.SHA256 != rel.SHA256 || m.DeviceID != dev || !strings.HasPrefix(m.URL, "https://remote.example.com/api/agent/v1/files/") {
		t.Fatalf("manifest %+v", m)
	}
	resp, _ = http.Get(srv.URL + "/api/agent/v1/files/" + rel.SHA256)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(body, exe) {
		t.Fatalf("file download %d", resp.StatusCode)
	}
	// Customer exception: updates off.
	if err := agents.SetOverride(ctx, cust, "off", ""); err != nil {
		t.Fatal(err)
	}
	resp, _ = http.Get(srv.URL + "/api/agent/v1/update?os=windows&arch=amd64&version=v0.1.0")
	if resp.StatusCode != 204 {
		t.Fatalf("customer off: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/api/agent/v1/update/report", "application/json", strings.NewReader(`{"from_version":"v0.1.0","to_version":"v0.2.0","status":"installed"}`))
	if resp.StatusCode != 204 {
		t.Fatalf("report: %d", resp.StatusCode)
	}
	ov, err := agents.Overview(ctx)
	if err != nil || len(ov.Events) != 2 || ov.Versions[0].Version != "v0.1.0" {
		t.Fatalf("overview %v %+v", err, ov)
	}

	// Code signing with a PFX: generated installers are signed and still carry their config.
	if _, err := exec.LookPath("osslsigncode"); err != nil {
		t.Skip("osslsigncode not installed")
	}
	if err := code.Save(ctx, codesign.Update{Mode: "pfx", PFXBase64: testPFX(t), Password: "wrong", TimestampURL: ""}); err == nil {
		t.Fatal("wrong PFX password accepted")
	}
	if err := code.Save(ctx, codesign.Update{Mode: "pfx", PFXBase64: testPFX(t), Password: "s3cret", TimestampURL: ""}); err != nil {
		t.Fatal(err)
	}
	st, _ := code.Status(ctx)
	if !st.Ready || !strings.Contains(st.Subject, "RHIS Test Code Signing") {
		t.Fatalf("status %+v", st)
	}
	f2, err := b.Generate(ctx, bd.ID, "amd64", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !f2.Signed {
		t.Fatal("installer not signed")
	}
	path := filepath.Join(data, "builds", itoa(bd.ID), itoa(f2.ID)+".exe")
	signed, _ := os.ReadFile(path)
	if ok, _ := builder.IsSigned(signed); !ok {
		t.Fatal("no certificate table")
	}
	if c, err := builder.Extract(signed); err != nil || c.BuildID != bd.ID {
		t.Fatalf("config lost after signing: %v", err)
	}
	// osslsigncode verify checks the Authenticode hash; the self-signed chain itself is not trusted.
	vout, _ := exec.Command("osslsigncode", "verify", "-in", path).CombinedOutput()
	if !strings.Contains(string(vout), "Calculated message digest") || strings.Contains(string(vout), "MISMATCH") {
		t.Fatalf("signature digest does not verify:\n%s", vout)
	}
}

func itoa(n int64) string { return big.NewInt(n).String() }
