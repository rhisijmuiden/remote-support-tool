// Package config loads the server configuration from a YAML file and environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Known server roles. A single machine normally runs "all".
// "update" serves agent updates and installer download links.
var KnownRoles = []string{"web", "api", "scheduler", "update"}

type Config struct {
	Listen      string   `yaml:"listen"`       // e.g. ":8080" (behind a reverse proxy) or ":443"
	PublicURL   string   `yaml:"public_url"`   // e.g. https://remote.rhis.nl
	DatabaseURL string   `yaml:"database_url"` // postgres://user:pass@host/db
	DataDir     string   `yaml:"data_dir"`     // recordings, installers, uploads
	SecretKey   string   `yaml:"secret_key"`   // encrypts stored secrets; 32+ random chars
	Roles       []string `yaml:"roles"`
	TLS         struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
	Update struct {
		Repo   string `yaml:"repo"`  // GitHub owner/name used to look for releases
		Asset  string `yaml:"asset"` // release asset name
		Token  string `yaml:"token"` // optional GitHub token (private repos / rate limits)
		APIURL string `yaml:"api_url"`
	} `yaml:"update"`
	// Agents are imported from GitHub releases of this repository (defaults to update.repo).
	Agents struct {
		Repo         string `yaml:"repo"`
		SyncInterval string `yaml:"sync_interval"` // e.g. "1h"; "0" turns automatic import off
		Keep         int    `yaml:"keep"`          // releases kept per product/arch/channel
	} `yaml:"agents"`
	CodeSigning struct {
		// Command signs a Windows executable with a key on a token, HSM or cloud service,
		// for example: jsign --storetype YUBIKEY --storepass "..." --tsaurl {timestamp} {in}
		// {in}, {out}, {name}, {url} and {timestamp} are replaced. Only settable here, not in the portal.
		Command string `yaml:"command"`
	} `yaml:"code_signing"`
	// AllowInsecureHTTP permits serving plain HTTP on a non-loopback address. Off by default:
	// passwords and session cookies must never cross the network unencrypted.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
	// TrustedProxies lists extra reverse proxies (IPs or CIDR ranges) in front of the local
	// Caddy, e.g. Zoraxy. X-Forwarded-For hops added by these are skipped when looking for
	// the real client address. Loopback is always trusted.
	TrustedProxies []string `yaml:"trusted_proxies"`
	trustedNets    []*net.IPNet
	Branding       struct {
		ProductName string `yaml:"product_name"`
		CompanyName string `yaml:"company_name"`
	} `yaml:"branding"`
}

func Default() Config {
	var c Config
	c.Listen = "127.0.0.1:8080"
	c.PublicURL = "https://localhost"
	c.DataDir = "/var/lib/rhis-remote"
	c.Roles = []string{"all"}
	c.Update.Repo = "rhisijmuiden/remote-support-tool"
	c.Update.Asset = "rhis-remote_linux_amd64.tar.gz"
	c.Update.APIURL = "https://api.github.com"
	c.Agents.SyncInterval = "1h"
	c.Agents.Keep = 5
	c.Branding.ProductName = "RHIS Remote"
	c.Branding.CompanyName = "RHIS"
	return c
}

// Load reads path (if it exists) and applies RHIS_REMOTE_* environment overrides.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(b, &c); err != nil {
				return c, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return c, err
		}
	}
	env := func(k string, dst *string) {
		if v, ok := os.LookupEnv("RHIS_REMOTE_" + k); ok {
			*dst = v
		}
	}
	env("LISTEN", &c.Listen)
	env("PUBLIC_URL", &c.PublicURL)
	env("DATABASE_URL", &c.DatabaseURL)
	env("DATA_DIR", &c.DataDir)
	env("SECRET_KEY", &c.SecretKey)
	env("UPDATE_TOKEN", &c.Update.Token)
	if v := os.Getenv("RHIS_REMOTE_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = strings.Split(v, ",")
	}
	if v := os.Getenv("RHIS_REMOTE_ROLES"); v != "" {
		c.Roles = strings.Split(v, ",")
	}
	err := c.Validate()
	return c, err
}

func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database_url is required")
	}
	if len(c.SecretKey) < 32 {
		return errors.New("secret_key must be at least 32 characters")
	}
	roles := []string{}
	for _, r := range c.Roles {
		r = strings.TrimSpace(r)
		if r == "all" {
			roles = append(roles, KnownRoles...)
			continue
		}
		ok := false
		for _, k := range KnownRoles {
			ok = ok || k == r
		}
		if !ok {
			return fmt.Errorf("unknown role %q (known: all, %s)", r, strings.Join(KnownRoles, ", "))
		}
		roles = append(roles, r)
	}
	c.Roles = roles
	if c.TLS.CertFile == "" && !c.AllowInsecureHTTP && !isLoopback(c.Listen) {
		return fmt.Errorf("listen %q would serve plain HTTP on the network: listen on 127.0.0.1 behind the HTTPS proxy (Caddy), set tls.cert_file and tls.key_file, or set allow_insecure_http: true for a lab setup", c.Listen)
	}
	if !strings.HasPrefix(c.PublicURL, "https://") && !c.AllowInsecureHTTP {
		return fmt.Errorf("public_url must start with https:// (got %q); set allow_insecure_http: true only for a lab setup", c.PublicURL)
	}
	c.trustedNets = nil
	for _, p := range c.TrustedProxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			if ip := net.ParseIP(p); ip != nil && ip.To4() != nil {
				p += "/32"
			} else {
				p += "/128"
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return fmt.Errorf("trusted_proxies: %q is not an IP address or CIDR range", p)
		}
		c.trustedNets = append(c.trustedNets, n)
	}
	return nil
}

// IsTrustedProxy reports whether ip is loopback or listed in trusted_proxies.
func (c *Config) IsTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range c.trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) HasRole(r string) bool {
	for _, x := range c.Roles {
		if x == r {
			return true
		}
	}
	return false
}
