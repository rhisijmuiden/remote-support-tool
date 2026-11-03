// SPDX-License-Identifier: AGPL-3.0-only

// Package codesign Authenticode-signs Windows executables on the Linux server.
//
// Two modes:
//   - "pfx": a PKCS#12 certificate file is stored (encrypted) and used with osslsigncode.
//   - "command": a command from config.yaml signs the file, for keys that live on a
//     hardware token, an HSM or a cloud signing service (since 2023 most public code
//     signing certificates are issued that way). {in} and {out} are replaced by paths.
//     The command can only be set in config.yaml, never from the portal, so a portal
//     account can not make the server run programs.
package codesign

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

const SettingsKey = "code_signing"

type Settings struct {
	Mode         string `json:"mode"` // "", "pfx" or "command"
	PFX          string `json:"pfx,omitempty"`
	Password     string `json:"password,omitempty"`
	TimestampURL string `json:"timestamp_url"`
	// Read from the certificate when it is saved, for the portal.
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	NotAfter   time.Time `json:"not_after"`
	Thumbprint string    `json:"thumbprint"`
}

// Status is what the portal shows.
type Status struct {
	Mode         string    `json:"mode"`
	Command      string    `json:"command"`
	TimestampURL string    `json:"timestamp_url"`
	HasPFX       bool      `json:"has_pfx"`
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	NotAfter     time.Time `json:"not_after"`
	Thumbprint   string    `json:"thumbprint"`
	Tool         string    `json:"tool"` // osslsigncode version, empty when missing
	Ready        bool      `json:"ready"`
}

type Service struct {
	Store   *settings.Store
	Box     *secret.Box
	TempDir string
	// Name and URL are written into the signature (shown by Windows in the UAC prompt).
	Name, URL string
	// Command is code_signing.command from config.yaml.
	Command string
}

const defaultTimestamp = "http://timestamp.digicert.com"

func (s *Service) load(ctx context.Context) (Settings, error) {
	st := Settings{TimestampURL: defaultTimestamp}
	_, err := s.Store.Get(ctx, SettingsKey, &st)
	return st, err
}

func toolVersion() string {
	out, err := exec.Command("osslsigncode", "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSuffix(strings.TrimSpace(line), ", using:")
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	st, err := s.load(ctx)
	if err != nil {
		return Status{}, err
	}
	out := Status{Mode: st.Mode, Command: s.Command, TimestampURL: st.TimestampURL, HasPFX: st.PFX != "",
		Subject: st.Subject, Issuer: st.Issuer, NotAfter: st.NotAfter, Thumbprint: st.Thumbprint, Tool: toolVersion()}
	switch st.Mode {
	case "pfx":
		out.Ready = out.HasPFX && out.Tool != "" && time.Now().Before(st.NotAfter)
	case "command":
		out.Ready = s.Command != ""
	}
	return out, nil
}

// Update is what the portal sends. An empty PFX or password keeps the stored one.
type Update struct {
	Mode         string `json:"mode"`
	PFXBase64    string `json:"pfx_base64"`
	Password     string `json:"password"`
	TimestampURL string `json:"timestamp_url"`
}

// Save validates and stores the settings. A PFX is opened with its password first, so a
// wrong password or an expired certificate is reported right away.
func (s *Service) Save(ctx context.Context, in Update) error {
	st, err := s.load(ctx)
	if err != nil {
		return err
	}
	if in.TimestampURL != "" && !strings.HasPrefix(in.TimestampURL, "http://") && !strings.HasPrefix(in.TimestampURL, "https://") {
		return errors.New("codesign_timestamp")
	}
	st.TimestampURL = in.TimestampURL
	switch in.Mode {
	case "":
		st = Settings{TimestampURL: in.TimestampURL}
	case "pfx":
		pfx := []byte(nil)
		if in.PFXBase64 != "" {
			if pfx, err = base64.StdEncoding.DecodeString(in.PFXBase64); err != nil || len(pfx) > 1<<20 {
				return errors.New("codesign_pfx_invalid")
			}
		} else if st.PFX != "" {
			b64, err := s.Box.Open(st.PFX)
			if err != nil {
				return err
			}
			pfx, _ = base64.StdEncoding.DecodeString(b64)
		} else {
			return errors.New("codesign_pfx_required")
		}
		pass := in.Password
		if pass == "" && in.PFXBase64 == "" && st.Password != "" {
			if pass, err = s.Box.Open(st.Password); err != nil {
				return err
			}
		}
		_, cert, _, err := pkcs12.DecodeChain(pfx, pass)
		if err != nil {
			return errors.New("codesign_pfx_password")
		}
		if !hasCodeSigning(cert) {
			return errors.New("codesign_not_code_signing")
		}
		if time.Now().After(cert.NotAfter) {
			return errors.New("codesign_expired")
		}
		if st.PFX, err = s.Box.Seal(base64.StdEncoding.EncodeToString(pfx)); err != nil {
			return err
		}
		if st.Password, err = s.Box.Seal(pass); err != nil {
			return err
		}
		sum := sha1.Sum(cert.Raw) // Windows shows SHA-1 thumbprints
		st.Subject, st.Issuer, st.NotAfter = cert.Subject.String(), cert.Issuer.String(), cert.NotAfter
		st.Thumbprint = strings.ToUpper(hex.EncodeToString(sum[:]))
		st.Mode = "pfx"
	case "command":
		if err := checkCommand(s.Command); err != nil {
			return err
		}
		st = Settings{Mode: "command", TimestampURL: in.TimestampURL}
	default:
		return errors.New("codesign_mode")
	}
	return s.Store.Put(ctx, SettingsKey, st)
}

func hasCodeSigning(c *x509.Certificate) bool {
	if len(c.ExtKeyUsage) == 0 {
		return true
	}
	for _, u := range c.ExtKeyUsage {
		if u == x509.ExtKeyUsageCodeSigning || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// Enabled reports whether files should be signed.
func (s *Service) Enabled(ctx context.Context) bool {
	st, err := s.load(ctx)
	return err == nil && st.Mode != ""
}

// Sign returns exe signed with the configured certificate. ok is false when signing is not configured.
func (s *Service) Sign(ctx context.Context, exe []byte, description string) (out []byte, ok bool, err error) {
	st, err := s.load(ctx)
	if err != nil || st.Mode == "" {
		return exe, false, err
	}
	dir, err := os.MkdirTemp(s.TempDir, "codesign-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	in, outPath := filepath.Join(dir, "in.exe"), filepath.Join(dir, "out.exe")
	if err := os.WriteFile(in, exe, 0o600); err != nil {
		return nil, false, err
	}
	if description == "" {
		description = s.Name
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	switch st.Mode {
	case "pfx":
		b64, err := s.Box.Open(st.PFX)
		if err != nil {
			return nil, false, err
		}
		pfx, _ := base64.StdEncoding.DecodeString(b64)
		pass, err := s.Box.Open(st.Password)
		if err != nil {
			return nil, false, err
		}
		pfxPath, passPath := filepath.Join(dir, "cert.pfx"), filepath.Join(dir, "pass")
		if err := os.WriteFile(pfxPath, pfx, 0o600); err != nil {
			return nil, false, err
		}
		if err := os.WriteFile(passPath, []byte(pass), 0o600); err != nil {
			return nil, false, err
		}
		args := []string{"sign", "-pkcs12", pfxPath, "-readpass", passPath, "-h", "sha256", "-n", description, "-in", in, "-out", outPath}
		if s.URL != "" {
			args = append(args, "-i", s.URL)
		}
		if st.TimestampURL != "" {
			args = append(args, "-ts", st.TimestampURL)
		}
		cmd = exec.CommandContext(ctx, "osslsigncode", args...)
	case "command":
		if err := checkCommand(s.Command); err != nil {
			return nil, false, err
		}
		parts, _ := splitArgs(s.Command)
		for i := range parts {
			parts[i] = strings.NewReplacer("{in}", in, "{out}", outPath, "{name}", description, "{url}", s.URL, "{timestamp}", st.TimestampURL).Replace(parts[i])
		}
		cmd = exec.CommandContext(ctx, parts[0], parts[1:]...)
		if !strings.Contains(s.Command, "{out}") {
			outPath = in // the tool signs in place
		}
	}
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr // both end up in the error message
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return nil, false, fmt.Errorf("signing failed: %v: %s", err, msg)
	}
	signed, err := os.ReadFile(outPath)
	if err != nil {
		return nil, false, err
	}
	return signed, true, nil
}

func checkCommand(c string) error {
	args, err := splitArgs(c)
	if err != nil || len(args) == 0 || !strings.Contains(c, "{in}") {
		return errors.New("codesign_command")
	}
	return nil
}

// splitArgs splits a command line on spaces, honouring single and double quotes. No shell is involved.
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out, nil
}
