package config

import (
	"strings"
	"testing"
)

func TestValidateRequiresEncryption(t *testing.T) {
	base := func() Config {
		c := Default()
		c.DatabaseURL = "postgres://localhost/x"
		c.SecretKey = "0123456789abcdef0123456789abcdef"
		return c
	}
	cases := []struct {
		name   string
		edit   func(*Config)
		wantOK bool
	}{
		{"default behind proxy", func(c *Config) {}, true},
		{"plain http on network", func(c *Config) { c.Listen = ":8080" }, false},
		{"http public url", func(c *Config) { c.PublicURL = "http://10.0.0.5:8080" }, false},
		{"own tls on network", func(c *Config) { c.Listen = ":443"; c.TLS.CertFile, c.TLS.KeyFile = "c", "k" }, true},
		{"explicit lab opt-in", func(c *Config) { c.Listen = ":8080"; c.PublicURL = "http://lab:8080"; c.AllowInsecureHTTP = true }, true},
		{"ipv6 loopback", func(c *Config) { c.Listen = "[::1]:8080" }, true},
	}
	for _, tc := range cases {
		c := base()
		tc.edit(&c)
		err := c.Validate()
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: err = %v, wantOK %v", tc.name, err, tc.wantOK)
		}
	}
}

func base() Config {
	c := Default()
	c.DatabaseURL = "postgres://u:p@localhost/db?sslmode=disable"
	c.SecretKey = strings.Repeat("x", 32)
	return c
}

func TestRolesExpand(t *testing.T) {
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range KnownRoles {
		if !c.HasRole(r) {
			t.Errorf("all should include %s", r)
		}
	}
	c = base()
	c.Roles = []string{"bogus"}
	if c.Validate() == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestRelayOnlyNeedsNoDatabase(t *testing.T) {
	c := Default()
	c.Roles = []string{"relay"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "control_url") {
		t.Fatalf("want control_url error, got %v", err)
	}
	c.Join.ControlURL = "https://remote.example.com"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.NeedsDatabase() {
		t.Fatal("relay only should not need a database")
	}
}

func TestEncryptionRequired(t *testing.T) {
	cases := map[string]func(*Config){
		"plain control url": func(c *Config) { c.Roles = []string{"relay"}; c.Join.ControlURL = "http://remote.example.com" },
		"plain nats":        func(c *Config) { c.Cluster.NATS.URL = "nats://n1:4222" },
		"nats without mtls": func(c *Config) { c.Cluster.NATS.URL = "tls://n1:4222" },
		"remote db no tls":  func(c *Config) { c.DatabaseURL = "postgres://u:p@db.internal/rhis?sslmode=disable" },
		"remote db default": func(c *Config) { c.DatabaseURL = "postgres://u:p@10.0.0.5/rhis" },
	}
	for name, mut := range cases {
		c := base()
		mut(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "encrypted") {
			t.Errorf("%s: want encryption error, got %v", name, err)
		}
		c.AllowInsecureHTTP = true
		if err := c.Validate(); err != nil && strings.Contains(err.Error(), "encrypted") {
			t.Errorf("%s: allow_insecure should skip the check: %v", name, err)
		}
	}
	ok := base()
	ok.DatabaseURL = "postgres://u:p@db.internal/rhis?sslmode=verify-full"
	ok.Cluster.NATS.URL = "tls://n1:4222, tls://n2:4222"
	ok.Cluster.NATS.CertFile, ok.Cluster.NATS.KeyFile = "c.pem", "k.pem"
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	local := base()
	local.DatabaseURL = "postgres://u:p@127.0.0.1/rhis?sslmode=disable"
	if err := local.Validate(); err != nil {
		t.Fatalf("local database without TLS should be fine: %v", err)
	}
}
