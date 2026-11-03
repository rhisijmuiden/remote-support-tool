package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// agentConn is one open control channel.
type agentConn struct {
	deviceID int64
	conn     *websocket.Conn
	out      chan any
	once     sync.Once
	done     chan struct{}
	reason   string
}

func (c *agentConn) send(msg any) error {
	select {
	case c.out <- msg:
		return nil
	case <-c.done:
		return ErrOffline
	case <-time.After(5 * time.Second):
		return errors.New("agent not reading")
	}
}

func (c *agentConn) close(reason string) {
	c.once.Do(func() {
		c.reason = reason
		close(c.done)
	})
}

type hello struct {
	Type         string `json:"type"`
	Hostname     string `json:"hostname"`
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
	User         string `json:"user"`
	MachineID    string `json:"machine_id"`
}

type controlMsg struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	User      string `json:"user"`
}

func (s *Service) handleControl(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.deviceForToken(ctx, bearer(r))
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			fail(w, http.StatusUnauthorized, "unauthorized", nil)
		} else {
			fail(w, 500, "internal", err)
		}
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 10)
	ip := remoteIP(r)

	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	var h hello
	err = wsjson.Read(hctx, ws, &h)
	cancel()
	if err != nil || h.Type != "hello" {
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	c := &agentConn{deviceID: d.ID, conn: ws, out: make(chan any, 16), done: make(chan struct{})}

	s.mu.Lock()
	s.init()
	old := s.conns[d.ID]
	s.conns[d.ID] = c
	s.mu.Unlock()
	if old != nil {
		old.close("replaced")
	}

	_, err = s.DB.Exec(ctx, `UPDATE devices SET online=true, last_seen_at=now(), hostname=COALESCE(NULLIF($2,''), hostname),
		os_version=COALESCE(NULLIF($3,''), os_version), agent_version=COALESCE(NULLIF($4,''), agent_version), last_user=NULLIF($5,''), public_ip=$6
		WHERE id=$1`, d.ID, clean(h.Hostname, 100), clean(h.OSVersion, 200), clean(h.AgentVersion, 50), clean(h.User, 200), ip)
	if err != nil {
		slog.Error("agent presence", "device", d.ID, "err", err)
	}
	slog.Info("agent connected", "device", d.ID, "host", h.Hostname, "ip", ip)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		// Reader: pongs, status updates and session answers.
		defer c.close("read_closed")
		for {
			var m controlMsg
			if err := wsjson.Read(ctx, ws, &m); err != nil {
				return
			}
			switch m.Type {
			case "pong":
				s.DB.Exec(ctx, `UPDATE devices SET last_seen_at=now() WHERE id=$1`, d.ID)
			case "status":
				s.DB.Exec(ctx, `UPDATE devices SET last_user=NULLIF($2,''), last_seen_at=now() WHERE id=$1`, d.ID, clean(m.User, 200))
			case "session_accept":
				s.agentAccepted(d.ID, m.SessionID)
			case "session_reject":
				s.endSession(m.SessionID, d.ID, "rejected:"+clean(m.Reason, 40))
			}
		}
	}()

	write := func(v any) error {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return wsjson.Write(wctx, ws, v)
	}
	err = write(map[string]any{"type": "welcome", "device_id": d.ID, "device_code": d.Code, "ping_interval": int(s.PingInterval / time.Second)})
	ping := time.NewTicker(s.PingInterval)
	defer ping.Stop()
	missed := 0
	for err == nil {
		select {
		case m := <-c.out:
			err = write(m)
		case <-ping.C:
			// Pings double as a liveness check: three missed write deadlines close the socket.
			if err = write(map[string]string{"type": "ping"}); err != nil {
				missed++
				if missed < 3 {
					err = nil
				}
			} else {
				missed = 0
			}
		case <-c.done:
			err = errors.New(c.reason)
		}
	}

	s.mu.Lock()
	current := s.conns[d.ID] == c
	if current {
		delete(s.conns, d.ID)
	}
	s.mu.Unlock()
	c.close("closed")
	ws.Close(websocket.StatusNormalClosure, c.reason)
	if current {
		s.DB.Exec(context.Background(), `UPDATE devices SET online=false, last_seen_at=now() WHERE id=$1`, d.ID)
		s.endDeviceSessions(d.ID, "agent_disconnected")
	}
	slog.Info("agent disconnected", "device", d.ID, "reason", c.reason)
}

// mustJSON is used for small fixed messages.
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
