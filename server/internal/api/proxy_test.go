package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
)

func TestTrustedProxyIP(t *testing.T) {
	cfg := config.Default()
	cfg.DatabaseURL, cfg.SecretKey = "postgres://localhost/x", "0123456789abcdef0123456789abcdef"
	cfg.TrustedProxies = []string{"10.10.4.10", "192.168.50.0/24"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var got string
	h := trustedProxyIP(cfg.IsTrustedProxy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = clientIP(r) }))
	cases := []struct{ name, remote, xff, realIP, want string }{
		{"local caddy", "127.0.0.1:5555", "203.0.113.9", "", "203.0.113.9"},
		{"client-supplied prefix ignored", "127.0.0.1:5555", "1.1.1.1, 203.0.113.9", "", "203.0.113.9"},
		{"direct client never trusted", "198.51.100.7:5555", "203.0.113.9", "", "198.51.100.7"},
		{"ipv6", "[::1]:5555", "2001:db8::1", "", "2001:db8::1"},
		{"zoraxy then caddy", "127.0.0.1:5555", "203.0.113.9, 10.10.4.10", "", "203.0.113.9"},
		{"zoraxy range, spoofed prefix", "127.0.0.1:5555", "6.6.6.6, 203.0.113.9, 192.168.50.3", "", "203.0.113.9"},
		{"untrusted lan host via caddy", "127.0.0.1:5555", "203.0.113.9, 10.10.4.99", "", "10.10.4.99"},
		{"only proxies", "127.0.0.1:5555", "10.10.4.10", "", "10.10.4.10"},
		{"garbage stops walk", "127.0.0.1:5555", "203.0.113.9, nonsense, 10.10.4.10", "", "10.10.4.10"},
		{"x-real-ip fallback", "127.0.0.1:5555", "", "203.0.113.9", "203.0.113.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			r.Header.Set("X-Real-IP", c.realIP)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if cfg.IsTrustedProxy(net.ParseIP("10.10.4.11")) {
		t.Error("10.10.4.11 should not be trusted")
	}
}

func TestTrustedProxiesInvalid(t *testing.T) {
	cfg := config.Default()
	cfg.DatabaseURL, cfg.SecretKey = "postgres://localhost/x", "0123456789abcdef0123456789abcdef"
	cfg.TrustedProxies = []string{"zoraxy.local"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for a host name")
	}
}
