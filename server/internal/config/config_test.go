package config

import "testing"

func TestValidateRequiresEncryption(t *testing.T) {
	base := func() Config {
		c := Default()
		c.DatabaseURL = "postgres://x"
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
