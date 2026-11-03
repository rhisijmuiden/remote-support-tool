// SPDX-License-Identifier: AGPL-3.0-only

// Package builder makes branded client installers: it takes a prebuilt agent
// executable, appends a per-build configuration (logo, server list, enrollment
// token, update keys) and optionally signs the result with a code signing certificate.
package builder

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Magic ends the configuration trailer. The layout at the end of the file (or
// directly before the Authenticode certificate table, when the file is signed) is:
//
//	[zero padding][UTF-8 JSON][uint32 little-endian JSON length]["RHISCFG1"]
//
// The padding makes the trailer end on an 8-byte boundary, which is where a
// certificate table has to start.
const Magic = "RHISCFG1"

// MaxLogoBytes limits the logo embedded in a client.
const MaxLogoBytes = 256 << 10

// ClientConfig is what the agent reads from its own executable. Field names are
// part of the agent contract; add fields, never rename them.
type ClientConfig struct {
	V           int      `json:"v"`
	BuildID     int64    `json:"build_id"`
	ProductName string   `json:"product_name"`
	CompanyName string   `json:"company_name"`
	LogoPNG     []byte   `json:"logo_png,omitempty"` // base64 in JSON
	AccentColor string   `json:"accent_color,omitempty"`
	Servers     []string `json:"servers"`
	EnrollToken string   `json:"enroll_token,omitempty"`
	Mode        string   `json:"mode"`    // "install" or "quick"
	Channel     string   `json:"channel"` // "production" or "test"
	UpdateKeys  []string `json:"update_keys"`
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// NormalizeServers checks a server list and returns it cleaned up. Every server must
// use https: all traffic between client and server is encrypted.
func NormalizeServers(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("server_invalid: %s", s)
		}
		if u.Scheme != "https" {
			return nil, fmt.Errorf("server_not_https: %s", s)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("server_invalid: %s", s)
		}
		host := u.Hostname()
		if host == "" || strings.ContainsAny(host, " _") && net.ParseIP(host) == nil {
			return nil, fmt.Errorf("server_invalid: %s", s)
		}
		norm := "https://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	if len(out) == 0 {
		return nil, errors.New("servers_required")
	}
	if len(out) > 16 {
		return nil, errors.New("too_many_servers")
	}
	return out, nil
}

// Validate checks a configuration before it is embedded.
func (c *ClientConfig) Validate() error {
	if c.V != 1 {
		return errors.New("unsupported config version")
	}
	if c.Mode != "install" && c.Mode != "quick" {
		return errors.New("invalid_mode")
	}
	if c.Channel != "production" && c.Channel != "test" {
		return errors.New("invalid_channel")
	}
	if strings.TrimSpace(c.ProductName) == "" {
		return errors.New("product_name_required")
	}
	if c.AccentColor != "" && !colorRe.MatchString(c.AccentColor) {
		return errors.New("invalid_color")
	}
	if len(c.LogoPNG) > MaxLogoBytes {
		return errors.New("logo_too_large")
	}
	if len(c.LogoPNG) > 0 && !bytes.HasPrefix(c.LogoPNG, []byte("\x89PNG\r\n\x1a\n")) {
		return errors.New("logo_not_png")
	}
	if len(c.UpdateKeys) == 0 {
		return errors.New("update key missing")
	}
	if _, err := NormalizeServers(c.Servers); err != nil {
		return err
	}
	return nil
}

// peSecurityDir returns the file offset and size of the Authenticode certificate
// table, or 0, 0 when the file is not signed. It errors when exe is not a PE file.
func peSecurityDir(exe []byte) (off, size uint32, dirPos int, err error) {
	if len(exe) < 0x40 || exe[0] != 'M' || exe[1] != 'Z' {
		return 0, 0, 0, errors.New("not a Windows executable")
	}
	pe := int(binary.LittleEndian.Uint32(exe[0x3c:]))
	if pe < 0x40 || pe+24 > len(exe) || string(exe[pe:pe+4]) != "PE\x00\x00" {
		return 0, 0, 0, errors.New("not a Windows executable")
	}
	opt := pe + 24
	if opt+2 > len(exe) {
		return 0, 0, 0, errors.New("truncated PE header")
	}
	var dd, nPos int
	switch binary.LittleEndian.Uint16(exe[opt:]) {
	case 0x10b: // PE32
		nPos, dd = opt+92, opt+96
	case 0x20b: // PE32+
		nPos, dd = opt+108, opt+112
	default:
		return 0, 0, 0, errors.New("unknown PE optional header")
	}
	if dd+5*8 > len(exe) || binary.LittleEndian.Uint32(exe[nPos:]) < 5 {
		return 0, 0, 0, nil
	}
	dirPos = dd + 4*8
	off = binary.LittleEndian.Uint32(exe[dirPos:])
	size = binary.LittleEndian.Uint32(exe[dirPos+4:])
	if size != 0 && (int(off) > len(exe) || int(off)+int(size) > len(exe)) {
		return 0, 0, 0, errors.New("certificate table outside the file")
	}
	return off, size, dirPos, nil
}

// payloadEnd is where the trailer has to end: the start of the certificate table, or the end of the file.
func payloadEnd(exe []byte) (int, error) {
	off, size, _, err := peSecurityDir(exe)
	if err != nil {
		return 0, err
	}
	if size != 0 {
		return int(off), nil
	}
	return len(exe), nil
}

// StripSignature removes an existing Authenticode signature so data can be appended.
func StripSignature(exe []byte) ([]byte, error) {
	off, size, pos, err := peSecurityDir(exe)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return exe, nil
	}
	out := append([]byte(nil), exe[:off]...)
	binary.LittleEndian.PutUint64(out[pos:], 0)
	return out, nil
}

// Embed returns exe with cfg appended as a trailer. A signature on exe is removed first.
func Embed(exe []byte, cfg *ClientConfig) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base, err := StripSignature(exe)
	if err != nil {
		return nil, err
	}
	if _, err := Extract(base); err == nil {
		return nil, errors.New("executable already contains a configuration")
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	total := len(base) + len(js) + 12
	pad := (8 - total%8) % 8
	out := make([]byte, 0, total+pad)
	out = append(out, base...)
	out = append(out, make([]byte, pad)...)
	out = append(out, js...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(js)))
	out = append(out, Magic...)
	return out, nil
}

// Extract reads the configuration trailer from exe. It is the reference for the agent's reader.
func Extract(exe []byte) (*ClientConfig, error) {
	end, err := payloadEnd(exe)
	if err != nil {
		return nil, err
	}
	if end < 12 || string(exe[end-8:end]) != Magic {
		return nil, errors.New("no configuration found")
	}
	n := int(binary.LittleEndian.Uint32(exe[end-12:]))
	if n <= 0 || n > end-12 || n > 4<<20 {
		return nil, errors.New("invalid configuration length")
	}
	var c ClientConfig
	if err := json.Unmarshal(exe[end-12-n:end-12], &c); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return &c, nil
}

// IsSigned reports whether exe is a Windows executable with an Authenticode signature.
func IsSigned(exe []byte) (bool, error) {
	_, size, _, err := peSecurityDir(exe)
	return size != 0, err
}
