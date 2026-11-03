package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Bus carries messages between the servers of a cluster. On a single server the
// in-process LocalBus is used; a cluster uses NATS with mutual TLS (NewNATSBus).
//
// Subjects use NATS syntax: tokens separated by dots, "*" matches one token and ">"
// matches the rest. Conventions:
//
//	rhis.dev.<deviceID>.sig   signaling for a device; the gateway holding its connection subscribes
//	rhis.cluster.<event>      cluster-wide events, see the Event* constants
type Bus interface {
	Publish(subject string, data []byte) error
	// Subscribe calls h for every message on subject until the subscription is closed.
	Subscribe(subject string, h func(Msg)) (Subscription, error)
	// Request sends data and waits for the first reply.
	Request(ctx context.Context, subject string, data []byte) ([]byte, error)
	Close() error
}

type Subscription interface{ Unsubscribe() error }

type Msg struct {
	Subject string
	Data    []byte
	// Reply answers a Request. It is nil for messages that were published.
	Reply func(data []byte) error
}

const (
	// EventBackupSettings is published after the backup schedule changed.
	EventBackupSettings = "rhis.cluster.backup.settings"
	// EventNodesChanged is published after a server was added, drained or removed.
	EventNodesChanged = "rhis.cluster.nodes"
)

// DeviceSubject is the subject that carries signaling for a device.
func DeviceSubject(deviceID string) string { return "rhis.dev." + deviceID + ".sig" }

var ErrNoResponders = errors.New("bus: no responders")

// LocalBus is an in-process Bus for a single server.
type LocalBus struct {
	mu     sync.RWMutex
	subs   map[*localSub]struct{}
	closed bool
}

type localSub struct {
	bus     *LocalBus
	pattern []string
	h       func(Msg)
}

func NewLocalBus() *LocalBus { return &LocalBus{subs: map[*localSub]struct{}{}} }

func (b *LocalBus) Subscribe(subject string, h func(Msg)) (Subscription, error) {
	if !validSubject(subject, true) {
		return nil, errors.New("bus: invalid subject " + subject)
	}
	s := &localSub{bus: b, pattern: strings.Split(subject, "."), h: h}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("bus: closed")
	}
	b.subs[s] = struct{}{}
	return s, nil
}

func (s *localSub) Unsubscribe() error {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	return nil
}

func (b *LocalBus) matching(subject string) []*localSub {
	tok := strings.Split(subject, ".")
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []*localSub
	for s := range b.subs {
		if subjectMatch(s.pattern, tok) {
			out = append(out, s)
		}
	}
	return out
}

func (b *LocalBus) Publish(subject string, data []byte) error {
	if !validSubject(subject, false) {
		return errors.New("bus: invalid subject " + subject)
	}
	for _, s := range b.matching(subject) {
		d := append([]byte(nil), data...)
		s.h(Msg{Subject: subject, Data: d})
	}
	return nil
}

func (b *LocalBus) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	subs := b.matching(subject)
	if len(subs) == 0 {
		return nil, ErrNoResponders
	}
	reply := make(chan []byte, len(subs))
	for _, s := range subs {
		d := append([]byte(nil), data...)
		go s.h(Msg{Subject: subject, Data: d, Reply: func(r []byte) error {
			select {
			case reply <- append([]byte(nil), r...):
			default:
			}
			return nil
		}})
	}
	select {
	case r := <-reply:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *LocalBus) Close() error {
	b.mu.Lock()
	b.closed = true
	b.subs = map[*localSub]struct{}{}
	b.mu.Unlock()
	return nil
}

func validSubject(s string, wildcards bool) bool {
	if s == "" {
		return false
	}
	tok := strings.Split(s, ".")
	for i, t := range tok {
		if t == "" || strings.ContainsAny(t, " \t\r\n") {
			return false
		}
		if t == "*" || t == ">" {
			if !wildcards || (t == ">" && i != len(tok)-1) {
				return false
			}
		}
	}
	return true
}

func subjectMatch(pattern, tok []string) bool {
	for i, p := range pattern {
		if p == ">" {
			return len(tok) > i
		}
		if i >= len(tok) || (p != "*" && p != tok[i]) {
			return false
		}
	}
	return len(pattern) == len(tok)
}
