package backup

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TargetConfig describes a remote backup location. Secrets are stored sealed.
type TargetConfig struct {
	Type     string `json:"type"` // "ftp", "ftps-explicit", "ftps-implicit", "scp"
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	// PrivateKey is a PEM/OpenSSH private key for SCP (optional; password is used otherwise).
	PrivateKey string `json:"private_key,omitempty"`
	// HostKey pins the SSH host key fingerprint (SHA256:...). It is filled in on the first successful test.
	HostKey string `json:"host_key"`
	// SkipTLSVerify allows self-signed FTPS certificates. Off by default.
	SkipTLSVerify bool   `json:"skip_tls_verify"`
	Dir           string `json:"dir"`
}

func (t TargetConfig) Label() string {
	return fmt.Sprintf("%s://%s@%s:%d%s", t.Type, t.Username, t.Host, t.port(), t.dir())
}

func (t TargetConfig) port() int {
	if t.Port > 0 {
		return t.Port
	}
	switch t.Type {
	case "scp":
		return 22
	case "ftps-implicit":
		return 990
	default:
		return 21
	}
}

func (t TargetConfig) dir() string {
	d := strings.TrimSpace(t.Dir)
	if d == "" {
		return "."
	}
	return d
}

type RemoteFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Target is an open connection to a backup location.
type Target interface {
	Upload(ctx context.Context, name string, r io.Reader) error
	Download(ctx context.Context, name string) (io.ReadCloser, error)
	List(ctx context.Context) ([]RemoteFile, error)
	Delete(ctx context.Context, name string) error
	Close() error
}

// Open connects to the target. password and key are already decrypted.
// For SCP, seenHostKey receives the server's fingerprint so the caller can pin it.
func Open(ctx context.Context, t TargetConfig, password, key string, seenHostKey *string) (Target, error) {
	switch t.Type {
	case "ftp", "ftps-explicit", "ftps-implicit":
		return openFTP(ctx, t, password)
	case "scp":
		return openSSH(ctx, t, password, key, seenHostKey)
	}
	return nil, errors.New("backup_target_type")
}

func validName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return errors.New("invalid backup name")
	}
	return nil
}

// ---- FTP / FTPS ----

type ftpTarget struct {
	c   *ftp.ServerConn
	dir string
}

func openFTP(ctx context.Context, t TargetConfig, password string) (Target, error) {
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.port()))
	tlsCfg := &tls.Config{ServerName: t.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.SkipTLSVerify} //nolint:gosec // user opt-in
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(30 * time.Second)}
	switch t.Type {
	case "ftps-explicit":
		opts = append(opts, ftp.DialWithExplicitTLS(tlsCfg))
	case "ftps-implicit":
		opts = append(opts, ftp.DialWithTLS(tlsCfg))
	}
	c, err := ftp.Dial(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("ftp connect: %w", err)
	}
	if err := c.Login(t.Username, password); err != nil {
		c.Quit()
		return nil, fmt.Errorf("ftp login: %w", err)
	}
	d := t.dir()
	if d != "." {
		mkdirAllFTP(c, d)
	}
	return &ftpTarget{c: c, dir: d}, nil
}

func mkdirAllFTP(c *ftp.ServerConn, d string) {
	cur := ""
	if strings.HasPrefix(d, "/") {
		cur = "/"
	}
	for _, p := range strings.Split(strings.Trim(d, "/"), "/") {
		cur = path.Join(cur, p)
		c.MakeDir(cur) // ignore "exists"
	}
}

func (f *ftpTarget) p(name string) string { return path.Join(f.dir, name) }

func (f *ftpTarget) Upload(_ context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	// Upload under a temporary name and rename, so a broken transfer never looks like a complete backup.
	tmp := f.p("." + name + ".part")
	if err := f.c.Stor(tmp, r); err != nil {
		return fmt.Errorf("ftp upload: %w", err)
	}
	return f.c.Rename(tmp, f.p(name))
}

func (f *ftpTarget) Download(_ context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return f.c.Retr(f.p(name))
}

func (f *ftpTarget) List(context.Context) ([]RemoteFile, error) {
	es, err := f.c.List(f.dir)
	if err != nil {
		return nil, err
	}
	var out []RemoteFile
	for _, e := range es {
		if e.Type == ftp.EntryTypeFile && isBackupName(e.Name) {
			out = append(out, RemoteFile{Name: path.Base(e.Name), Size: int64(e.Size), ModTime: e.Time})
		}
	}
	sortFiles(out)
	return out, nil
}

func (f *ftpTarget) Delete(_ context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return f.c.Delete(f.p(name))
}

func (f *ftpTarget) Close() error { return f.c.Quit() }

// ---- SCP over SSH (file transfer via the SFTP subsystem of the same SSH login) ----

type sshTarget struct {
	conn *ssh.Client
	c    *sftp.Client
	dir  string
}

func openSSH(ctx context.Context, t TargetConfig, password, key string, seen *string) (Target, error) {
	var auths []ssh.AuthMethod
	if key != "" {
		var signer ssh.Signer
		var err error
		if password != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(password))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key))
		}
		if err != nil {
			return nil, fmt.Errorf("ssh key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	} else {
		auths = append(auths, ssh.Password(password))
	}
	cfg := &ssh.ClientConfig{
		User:    t.Username,
		Auth:    auths,
		Timeout: 30 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(k)
			if seen != nil {
				*seen = fp
			}
			if t.HostKey != "" && t.HostKey != fp {
				return fmt.Errorf("host key mismatch: expected %s, got %s", t.HostKey, fp)
			}
			return nil
		},
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.port()))
	d := net.Dialer{Timeout: 30 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ssh login: %w", err)
	}
	conn := ssh.NewClient(cc, chans, reqs)
	c, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh file transfer: %w", err)
	}
	dir := t.dir()
	if dir != "." {
		c.MkdirAll(dir)
	}
	return &sshTarget{conn: conn, c: c, dir: dir}, nil
}

func (s *sshTarget) p(name string) string { return path.Join(s.dir, name) }

func (s *sshTarget) Upload(_ context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	tmp := s.p("." + name + ".part")
	f, err := s.c.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.ReadFrom(r); err != nil {
		f.Close()
		s.c.Remove(tmp)
		return fmt.Errorf("scp upload: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return s.c.PosixRename(tmp, s.p(name))
}

func (s *sshTarget) Download(_ context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return s.c.Open(s.p(name))
}

func (s *sshTarget) List(context.Context) ([]RemoteFile, error) {
	fis, err := s.c.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []RemoteFile
	for _, fi := range fis {
		if fi.Mode().IsRegular() && isBackupName(fi.Name()) {
			out = append(out, RemoteFile{Name: fi.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
		}
	}
	sortFiles(out)
	return out, nil
}

func (s *sshTarget) Delete(_ context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return s.c.Remove(s.p(name))
}

func (s *sshTarget) Close() error {
	s.c.Close()
	return s.conn.Close()
}

func sortFiles(fs []RemoteFile) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Name > fs[j].Name }) // names start with a UTC timestamp
}
