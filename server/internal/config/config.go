// Package config loads the server configuration from a YAML file and environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Known server roles. A single machine normally runs "all".
//
//	web, api   portal and JSON API (needs the database)
//	scheduler  scheduled backups; in a cluster only the elected leader runs it
//	relay      TURN/TURNS media relay; can run on its own without a database
var KnownRoles = []string{"web", "api", "scheduler", "relay"}

// DatabaseRoles need a database connection. A node with only other roles joins a
// control server over HTTPS instead.
var DatabaseRoles = []string{"web", "api", "scheduler"}

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
	// AllowInsecureHTTP permits serving plain HTTP on a non-loopback address. Off by default:
	// passwords and session cookies must never cross the network unencrypted.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
	Branding          struct {
		ProductName string `yaml:"product_name"`
		CompanyName string `yaml:"company_name"`
	} `yaml:"branding"`
	Node    NodeConfig    `yaml:"node"`
	Cluster ClusterConfig `yaml:"cluster"`
	Join    JoinConfig    `yaml:"join"`
	Relay   RelayConfig   `yaml:"relay"`
}

// NodeConfig names this server in the cluster overview.
type NodeConfig struct {
	Name   string `yaml:"name"`   // defaults to the hostname
	Region string `yaml:"region"` // free text, e.g. "eu-west"; used to pick nearby relays later
}

// ClusterConfig connects several control servers (web/api) that share one PostgreSQL
// database. Without NATS every server works on its own; that is fine for one server.
type ClusterConfig struct {
	NATS struct {
		URL      string `yaml:"url"`       // tls://nats1:4222,tls://nats2:4222
		CAFile   string `yaml:"ca_file"`   // CA that signed the NATS server certificates
		CertFile string `yaml:"cert_file"` // client certificate (mutual TLS)
		KeyFile  string `yaml:"key_file"`
	} `yaml:"nats"`
}

// JoinConfig is used by servers without a database (relays). They register with a
// control server over HTTPS using a one-time join token from the portal.
type JoinConfig struct {
	ControlURL string `yaml:"control_url"` // https://remote.example.com
	Token      string `yaml:"token"`       // one-time token; exchanged for a node credential on first start
	CAFile     string `yaml:"ca_file"`     // only for a control server with a private CA
}

// RelayConfig configures the TURN relay. WebRTC media through the relay is always
// end-to-end encrypted (DTLS-SRTP); TURNS additionally wraps the relay connection in TLS.
type RelayConfig struct {
	Listen    string `yaml:"listen"`     // TURN over UDP and TCP, default ":3478"
	TLSListen string `yaml:"tls_listen"` // TURNS, default ":5349"; use ":443" on a dedicated relay
	PublicIP  string `yaml:"public_ip"`  // address peers reach; detected when empty
	Domain    string `yaml:"domain"`     // host name for TURNS URLs and the automatic certificate
	CertFile  string `yaml:"cert_file"`  // TURNS certificate; Let's Encrypt via domain when empty
	KeyFile   string `yaml:"key_file"`
	MinPort   uint16 `yaml:"min_port"` // relayed UDP port range, default 49160-49359
	MaxPort   uint16 `yaml:"max_port"`
	// AllowPrivatePeers lets the relay forward to private (LAN) addresses. Off by default
	// so the relay cannot be abused to reach the internal network.
	AllowPrivatePeers bool `yaml:"allow_private_peers"`
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
	c.Branding.ProductName = "RHIS Remote"
	c.Branding.CompanyName = "RHIS"
	c.Relay.Listen = ":3478"
	c.Relay.TLSListen = ":5349"
	c.Relay.MinPort = 49160
	c.Relay.MaxPort = 49359
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
	env("NODE_NAME", &c.Node.Name)
	env("NATS_URL", &c.Cluster.NATS.URL)
	env("JOIN_URL", &c.Join.ControlURL)
	env("JOIN_TOKEN", &c.Join.Token)
	env("RELAY_PUBLIC_IP", &c.Relay.PublicIP)
	if v := os.Getenv("RHIS_REMOTE_ROLES"); v != "" {
		c.Roles = strings.Split(v, ",")
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	roles := []string{}
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	for _, r := range c.Roles {
		r = strings.TrimSpace(r)
		if r == "all" {
			for _, k := range KnownRoles {
				add(k)
			}
			continue
		}
		ok := false
		for _, k := range KnownRoles {
			ok = ok || k == r
		}
		if !ok {
			return fmt.Errorf("unknown role %q (known: all, %s)", r, strings.Join(KnownRoles, ", "))
		}
		add(r)
	}
	if len(roles) == 0 {
		return errors.New("roles: at least one role is required")
	}
	c.Roles = roles
	if c.Node.Name == "" {
		c.Node.Name, _ = os.Hostname()
	}
	if c.NeedsDatabase() {
		if c.DatabaseURL == "" {
			return errors.New("database_url is required for roles " + strings.Join(DatabaseRoles, ", "))
		}
		if len(c.SecretKey) < 32 {
			return errors.New("secret_key must be at least 32 characters")
		}
		if c.TLS.CertFile == "" && !c.AllowInsecureHTTP && !isLoopback(c.Listen) {
			return fmt.Errorf("listen %q would serve plain HTTP on the network: listen on 127.0.0.1 behind the HTTPS proxy (Caddy), set tls.cert_file and tls.key_file, or set allow_insecure_http: true for a lab setup", c.Listen)
		}
		if !strings.HasPrefix(c.PublicURL, "https://") && !c.AllowInsecureHTTP {
			return fmt.Errorf("public_url must start with https:// (got %q); set allow_insecure_http: true only for a lab setup", c.PublicURL)
		}
	} else {
		if c.Join.ControlURL == "" {
			return errors.New("join.control_url is required on a server without a database")
		}
	}
	if c.HasRole("relay") {
		if c.Relay.MinPort == 0 || c.Relay.MaxPort < c.Relay.MinPort {
			return errors.New("relay: min_port and max_port must form a valid range")
		}
		if c.Relay.PublicIP != "" && net.ParseIP(c.Relay.PublicIP) == nil {
			return fmt.Errorf("relay.public_ip %q is not an IP address", c.Relay.PublicIP)
		}
	}
	if !c.AllowInsecureHTTP {
		if err := c.checkEncryption(); err != nil {
			return fmt.Errorf("%w (all traffic between servers must be encrypted)", err)
		}
	}
	return nil
}

// checkEncryption refuses configurations that would send traffic to another machine
// without TLS.
func (c *Config) checkEncryption() error {
	if c.Join.ControlURL != "" {
		u, err := url.Parse(c.Join.ControlURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("join.control_url must be an https:// URL")
		}
	}
	if n := c.Cluster.NATS; n.URL != "" {
		for _, s := range strings.Split(n.URL, ",") {
			u, err := url.Parse(strings.TrimSpace(s))
			if err != nil || u.Scheme != "tls" {
				return fmt.Errorf("cluster.nats.url %q must use tls://", s)
			}
		}
		if n.CertFile == "" || n.KeyFile == "" {
			return errors.New("cluster.nats needs cert_file and key_file (mutual TLS)")
		}
	}
	if c.DatabaseURL != "" {
		if err := checkDatabaseTLS(c.DatabaseURL); err != nil {
			return err
		}
	}
	return nil
}

// checkDatabaseTLS allows a plain connection only to a database on this machine.
func checkDatabaseTLS(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// Key/value DSNs are not inspected.
		return nil
	}
	host := u.Hostname()
	if host == "" || strings.HasPrefix(host, "/") || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	switch u.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
		return nil
	}
	return fmt.Errorf("database_url points to %s: set sslmode=verify-full (or require)", host)
}

// NeedsDatabase reports whether any role on this node uses the database.
func (c *Config) NeedsDatabase() bool {
	for _, r := range DatabaseRoles {
		if c.HasRole(r) {
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
