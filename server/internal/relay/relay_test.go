package relay

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v4"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
)

func freePort(t *testing.T) int {
	l, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port
}

func TestAllocateWithClusterCredential(t *testing.T) {
	port := freePort(t)
	cfg := config.Default().Relay
	cfg.Listen = "127.0.0.1:" + strconv.Itoa(port)
	cfg.TLSListen = ""
	secrets := []string{"current", "previous"}
	s := &Server{Cfg: cfg, DataDir: t.TempDir(), PublicIP: net.ParseIP("192.0.2.10"), Secrets: func() []string { return secrets }}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	urls := s.URLs()
	if len(urls) != 3 || !strings.HasPrefix(urls[1], "turn:192.0.2.10:") {
		t.Fatalf("urls: %v", urls)
	}

	try := func(secret string) error {
		user, pass := cluster.NewTURNCredential(secret, "u1", time.Now(), time.Hour)
		if secret == "wrong" {
			pass = "nope"
		}
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		c, err := turn.NewClient(&turn.ClientConfig{
			TURNServerAddr: cfg.Listen, STUNServerAddr: cfg.Listen,
			Username: user, Password: pass, Realm: Realm, Conn: conn, RTO: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Listen(); err != nil {
			t.Fatal(err)
		}
		relayConn, err := c.Allocate()
		if err != nil {
			return err
		}
		defer relayConn.Close()
		if got := relayConn.LocalAddr().(*net.UDPAddr).IP.String(); got != "192.0.2.10" {
			t.Fatalf("relayed address %s, want the public IP", got)
		}
		// Private peers are refused by default.
		if _, err := relayConn.WriteTo([]byte("x"), &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 9}); err == nil {
			t.Error("permission to a private peer was granted")
		}
		return nil
	}
	if err := try("current"); err != nil {
		t.Fatalf("current secret: %v", err)
	}
	if err := try("previous"); err != nil {
		t.Fatalf("previous secret: %v", err)
	}
	if err := try("wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := try("unknown"); err == nil {
		t.Fatal("unknown secret accepted")
	}
	if n := s.Stats()["allocations"]; n != 0 {
		// Allocations are released when the clients close.
		t.Logf("allocations after close: %v", n)
	}
}

func TestPeerAllowed(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2001:4860:4860::8888": true,
		"10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "100.64.0.1": false,
		"127.0.0.1": false, "::1": false, "169.254.1.1": false, "0.0.0.0": false, "224.0.0.1": false,
		"fe80::1": false, "fd00::1": false, "255.255.255.255": false,
	} {
		if got := PeerAllowed(net.ParseIP(ip), false); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
	if !PeerAllowed(net.ParseIP("10.1.2.3"), true) {
		t.Error("allow_private_peers should allow LAN addresses")
	}
	if PeerAllowed(net.ParseIP("127.0.0.1"), true) {
		t.Error("loopback must always be refused")
	}
}

func TestTLSConfigRequirements(t *testing.T) {
	s := &Server{Cfg: config.Default().Relay}
	if _, _, err := s.tlsConfig(); err == nil {
		t.Fatal("TURNS without domain or certificate should be disabled")
	}
	s.Cfg.Domain = "relay.example.com"
	if _, _, err := s.tlsConfig(); err == nil || !strings.Contains(err.Error(), "443") {
		t.Fatalf("automatic certificate needs port 443: %v", err)
	}
	s.Cfg.TLSListen = ":443"
	if _, host, err := s.tlsConfig(); err != nil || host != "relay.example.com" {
		t.Fatalf("autocert: %v", err)
	}
}
