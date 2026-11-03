package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/config"
)

// NATSBus is a Bus backed by a NATS cluster. Connections always use TLS with a client
// certificate; config validation refuses anything else.
type NATSBus struct{ nc *nats.Conn }

func NewNATSBus(cfg config.ClusterConfig, nodeName string) (*NATSBus, error) {
	n := cfg.NATS
	tc, err := natsTLS(n.CAFile, n.CertFile, n.KeyFile)
	if err != nil {
		return nil, err
	}
	nc, err := nats.Connect(n.URL,
		nats.Name("rhis-remote/"+nodeName),
		nats.Secure(tc),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) { slog.Info("nats reconnected", "url", c.ConnectedUrlRedacted()) }),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}
	return &NATSBus{nc: nc}, nil
}

func natsTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("cluster.nats.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("cluster.nats.ca_file: no certificates found")
		}
		tc.RootCAs = pool
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("cluster.nats client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

func (b *NATSBus) Publish(subject string, data []byte) error { return b.nc.Publish(subject, data) }

func (b *NATSBus) Subscribe(subject string, h func(Msg)) (Subscription, error) {
	return b.nc.Subscribe(subject, func(m *nats.Msg) {
		msg := Msg{Subject: m.Subject, Data: m.Data}
		if m.Reply != "" {
			msg.Reply = m.Respond
		}
		h(msg)
	})
}

func (b *NATSBus) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	m, err := b.nc.RequestWithContext(ctx, subject, data)
	if errors.Is(err, nats.ErrNoResponders) {
		return nil, ErrNoResponders
	}
	if err != nil {
		return nil, err
	}
	return m.Data, nil
}

func (b *NATSBus) Close() error {
	return b.nc.Drain()
}

// Connected reports whether the bus currently has a NATS connection.
func (b *NATSBus) Connected() bool { return b.nc.IsConnected() }
