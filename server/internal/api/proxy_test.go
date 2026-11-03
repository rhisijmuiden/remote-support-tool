package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyIP(t *testing.T) {
	var got string
	h := trustedProxyIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = clientIP(r) }))
	cases := []struct{ remote, xff, want string }{
		{"127.0.0.1:5555", "203.0.113.9", "203.0.113.9"},          // from local Caddy: trust it
		{"127.0.0.1:5555", "1.1.1.1, 203.0.113.9", "203.0.113.9"}, // client-supplied prefix is ignored
		{"198.51.100.7:5555", "203.0.113.9", "198.51.100.7"},      // direct client: never trust headers
		{"[::1]:5555", "2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		r.Header.Set("X-Forwarded-For", c.xff)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("remote %s xff %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
