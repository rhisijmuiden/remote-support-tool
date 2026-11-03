package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/version"
)

func archive(t *testing.T, content string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"./install.sh", "#!/bin/sh"}, {"./rhis-remote", content}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestCheckAndApply(t *testing.T) {
	arc := archive(t, "NEW BINARY")
	sum := sha256.Sum256(arc)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			fmt.Fprintf(w, `[
			 {"tag_name":"v0.2.0-test.7","prerelease":true,"assets":[{"name":"a.tar.gz","browser_download_url":"%[1]s/a"},{"name":"checksums.txt","browser_download_url":"%[1]s/sums"}]},
			 {"tag_name":"v0.1.5","prerelease":false,"assets":[{"name":"a.tar.gz","browser_download_url":"%[1]s/a"},{"name":"checksums.txt","browser_download_url":"%[1]s/sums"}]},
			 {"tag_name":"v9.9.9","draft":true}
			]`, srv.URL)
		case "/a":
			w.Write(arc)
		case "/sums":
			fmt.Fprintf(w, "%s  a.tar.gz\n", hex.EncodeToString(sum[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	version.Version, version.Channel = "v0.1.0", "production"
	s := &Service{APIURL: srv.URL, Repo: "o/r", Asset: "a.tar.gz"}
	prod, err := s.latest(context.Background(), "production")
	if err != nil || prod.Version != "v0.1.5" {
		t.Fatalf("production latest = %+v, %v", prod, err)
	}
	test, _ := s.latest(context.Background(), "test")
	if test.Version != "v0.2.0-test.7" {
		t.Fatalf("test latest = %+v", test)
	}

	exe := filepath.Join(t.TempDir(), "rhis-remote")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	s.Executable = exe
	if err := applyRelease(context.Background(), s, prod); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW BINARY" {
		t.Fatalf("binary = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "OLD" {
		t.Fatalf("previous = %q", b)
	}

	// A wrong checksum must leave the binary alone.
	arc = archive(t, "TAMPERED")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	if err := applyRelease(context.Background(), s, prod); err == nil {
		t.Fatal("expected checksum error")
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("binary replaced despite bad checksum: %q", b)
	}
}
