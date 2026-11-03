// Package relay is the TURN/TURNS media relay. Viewers and agents use it when a direct
// WebRTC connection is not possible (strict NAT, firewalls). The media inside is always
// end-to-end encrypted with DTLS-SRTP, so the relay never sees screen contents; TURNS
// additionally wraps the connection to the relay in TLS for networks that only allow 443.
package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/pion/turn/v4"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/cluster"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
)

// Realm is the TURN realm. It is part of the credential hash, so all relays use the same.
const Realm = "rhis-remote"

type Server struct {
	Cfg      config.RelayConfig
	DataDir  string
	PublicIP net.IP
	// Secrets returns the TURN secrets, newest first.
	Secrets func() []string

	srv     *turn.Server
	tlsOn   bool
	tlsHost string
	mu      sync.Mutex
}

// Start opens the listeners. It returns once the relay is serving.
func (s *Server) Start() error {
	if s.PublicIP == nil {
		return errors.New("relay: public IP unknown; set relay.public_ip")
	}
	if s.PublicIP.IsUnspecified() || s.PublicIP.IsLoopback() {
		return fmt.Errorf("relay: %s cannot be used as public IP", s.PublicIP)
	}
	gen := func() turn.RelayAddressGenerator {
		return &turn.RelayAddressGeneratorPortRange{
			RelayAddress: s.PublicIP,
			Address:      "0.0.0.0",
			MinPort:      s.Cfg.MinPort,
			MaxPort:      s.Cfg.MaxPort,
		}
	}
	perm := s.permission
	udp, err := net.ListenPacket("udp4", s.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("relay: listen udp %s: %w", s.Cfg.Listen, err)
	}
	tcp, err := net.Listen("tcp4", s.Cfg.Listen)
	if err != nil {
		udp.Close()
		return fmt.Errorf("relay: listen tcp %s: %w", s.Cfg.Listen, err)
	}
	listeners := []turn.ListenerConfig{{Listener: tcp, RelayAddressGenerator: gen(), PermissionHandler: perm}}

	if tc, host, err := s.tlsConfig(); err != nil {
		slog.Warn("relay: TURNS disabled", "reason", err)
	} else if s.Cfg.TLSListen != "" {
		tl, err := tls.Listen("tcp4", s.Cfg.TLSListen, tc)
		if err != nil {
			udp.Close()
			tcp.Close()
			return fmt.Errorf("relay: listen tls %s: %w", s.Cfg.TLSListen, err)
		}
		listeners = append(listeners, turn.ListenerConfig{Listener: tl, RelayAddressGenerator: gen(), PermissionHandler: perm})
		s.tlsOn, s.tlsHost = true, host
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm:             Realm,
		AuthHandler:       s.auth,
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: udp, RelayAddressGenerator: gen(), PermissionHandler: perm}},
		ListenerConfigs:   listeners,
	})
	if err != nil {
		udp.Close()
		tcp.Close()
		return fmt.Errorf("relay: %w", err)
	}
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
	slog.Info("relay listening", "turn", s.Cfg.Listen, "turns", s.Cfg.TLSListen, "turns_enabled", s.tlsOn,
		"public_ip", s.PublicIP.String(), "ports", fmt.Sprintf("%d-%d", s.Cfg.MinPort, s.Cfg.MaxPort))
	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// tlsConfig returns the TURNS certificate: configured files, or a Let's Encrypt
// certificate for relay.domain (TLS-ALPN-01, needs TURNS on port 443).
func (s *Server) tlsConfig() (*tls.Config, string, error) {
	if s.Cfg.TLSListen == "" {
		return nil, "", errors.New("relay.tls_listen is empty")
	}
	if s.Cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(s.Cfg.CertFile, s.Cfg.KeyFile)
		if err != nil {
			return nil, "", err
		}
		if s.Cfg.Domain == "" {
			return nil, "", errors.New("relay.domain is required with cert_file (TURNS URLs use the host name)")
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, s.Cfg.Domain, nil
	}
	if s.Cfg.Domain == "" {
		return nil, "", errors.New("set relay.domain (automatic certificate) or relay.cert_file")
	}
	if _, port, _ := net.SplitHostPort(s.Cfg.TLSListen); port != "443" {
		return nil, "", errors.New("an automatic certificate needs relay.tls_listen on port 443; or set relay.cert_file")
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(s.Cfg.Domain),
		Cache:      autocert.DirCache(filepath.Join(s.DataDir, "acme")),
	}
	tc := m.TLSConfig()
	tc.MinVersion = tls.VersionTLS12
	tc.NextProtos = []string{acme.ALPNProto}
	return tc, s.Cfg.Domain, nil
}

// auth checks a TURN REST credential against the shared secrets.
func (s *Server) auth(username, realm string, src net.Addr) ([]byte, bool) {
	sec, err := cluster.TURNSecretFor(username, s.Secrets(), time.Now())
	if err != nil {
		slog.Debug("relay: auth rejected", "src", src.String(), "err", err)
		return nil, false
	}
	return turn.GenerateAuthKey(username, realm, cluster.TURNPassword(sec, username)), true
}

// permission decides which peers a client may send to through the relay. Private,
// loopback and link-local addresses are refused by default so the relay cannot be
// used to reach the network it runs in.
func (s *Server) permission(_ net.Addr, peer net.IP) bool {
	return PeerAllowed(peer, s.Cfg.AllowPrivatePeers)
}

func PeerAllowed(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && (ip4[0] == 0 || ip4.Equal(net.IPv4bcast)) {
		return false
	}
	if !allowPrivate && (ip.IsPrivate() || isCGNAT(ip)) {
		return false
	}
	return true
}

func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64
}

// URLs returns the ICE URLs that reach this relay.
func (s *Server) URLs() []string {
	host := s.Cfg.Domain
	if host == "" && s.PublicIP != nil {
		host = s.PublicIP.String()
	}
	if host == "" {
		return nil
	}
	_, port, _ := net.SplitHostPort(s.Cfg.Listen)
	hp := net.JoinHostPort(host, port)
	urls := []string{"stun:" + hp, "turn:" + hp + "?transport=udp", "turn:" + hp + "?transport=tcp"}
	if s.tlsOn {
		_, tport, _ := net.SplitHostPort(s.Cfg.TLSListen)
		urls = append(urls, "turns:"+net.JoinHostPort(s.tlsHost, tport)+"?transport=tcp")
	}
	return urls
}

// Stats reports the number of active allocations for the servers overview and for
// picking the least busy relay.
func (s *Server) Stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	if s.srv != nil {
		n = s.srv.AllocationCount()
	}
	return map[string]any{"allocations": n, "turns": s.tlsOn}
}

// DetectPublicIP resolves host (for example the public URL's host name) to an IPv4 address.
func DetectPublicIP(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no IPv4 address for " + host)
	}
	return addrs[0], nil
}

// PortRange formats the relayed port range for messages and docs.
func PortRange(c config.RelayConfig) string {
	return strconv.Itoa(int(c.MinPort)) + "-" + strconv.Itoa(int(c.MaxPort))
}
