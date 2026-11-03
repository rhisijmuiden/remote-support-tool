package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startSSH runs an in-process SSH server with the SFTP subsystem, rooted at dir.
func startSSH(t *testing.T, dir string) (port int, fingerprint string) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "backup" && string(pw) == "s3cret" {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, _ := nch.Accept()
					go func() {
						for req := range creqs {
							ok := req.Type == "subsystem" && string(req.Payload[4:]) == "sftp"
							req.Reply(ok, nil)
							if ok {
								srv, _ := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(dir))
								srv.Serve()
								ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, ssh.FingerprintSHA256(signer.PublicKey())
}

func TestSSHTarget(t *testing.T) {
	dir := t.TempDir()
	port, fp := startSSH(t, dir)
	ctx := context.Background()
	cfg := TargetConfig{Type: "scp", Host: "127.0.0.1", Port: port, Username: "backup", Dir: "backups/server1"}

	var seen string
	tg, err := Open(ctx, cfg, "s3cret", "", &seen)
	if err != nil {
		t.Fatal(err)
	}
	if seen != fp {
		t.Fatalf("seen host key %s, want %s", seen, fp)
	}
	name := "rhis-remote-20261103T010000Z-full.tar.gz"
	if err := tg.Upload(ctx, name, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	fs, err := tg.List(ctx)
	if err != nil || len(fs) != 1 || fs[0].Name != name || fs[0].Size != 7 {
		t.Fatalf("list = %+v, %v", fs, err)
	}
	rc, err := tg.Download(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "archive" {
		t.Fatalf("download = %q", b)
	}
	if err := tg.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	tg.Close()

	// A pinned key that does not match must refuse to connect.
	cfg.HostKey = "SHA256:not-the-right-key"
	if _, err := Open(ctx, cfg, "s3cret", "", nil); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected host key mismatch, got %v", err)
	}
	// Wrong password fails.
	cfg.HostKey = fp
	if _, err := Open(ctx, cfg, "wrong", "", nil); err == nil {
		t.Fatal("expected login failure")
	}
}
