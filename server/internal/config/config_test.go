package config

import (
	"strings"
	"testing"
)

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
		c.AllowInsecure = true
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
