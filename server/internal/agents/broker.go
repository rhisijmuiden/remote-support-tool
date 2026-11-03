package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/audit"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/auth"
)

const (
	sideAgent  = 0
	sideViewer = 1
)

var sideName = [2]string{"agent", "viewer"}

// session pairs one viewer (browser) with one agent session socket.
type session struct {
	ID         string
	DeviceID   int64
	Hostname   string
	DeviceCode string
	Kind       string
	UserID     int64
	UserName   string
	UserEmail  string
	ViewerIP   string
	Record     bool
	LogID      int64
	Created    time.Time

	mu        sync.Mutex
	accepted  bool
	transport string
	peers     [2]*peer
	paired    bool
	done      chan struct{}
	endOnce   sync.Once
	reason    string
}

type frame struct {
	typ  websocket.MessageType
	data []byte
}

type peer struct {
	conn *websocket.Conn
	out  chan frame
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// StartResult is returned when a session has been requested.
type StartResult struct {
	SessionID  string      `json:"session_id"`
	DeviceID   int64       `json:"device_id"`
	ICEServers []ICEServer `json:"ice_servers"`
}

// StartSession asks an online device to open a session for user u. It returns at once;
// the browser then connects to the session socket and waits for the agent.
func (s *Service) StartSession(ctx context.Context, deviceID int64, u *auth.User, kind string, record bool, viewerIP string) (*StartResult, error) {
	if kind != "unattended" && kind != "attended" && kind != "quick" {
		return nil, errors.New("invalid_kind")
	}
	s.mu.Lock()
	s.init()
	c := s.conns[deviceID]
	s.mu.Unlock()
	var hostname, code string
	err := s.DB.QueryRow(ctx, `SELECT hostname, device_code FROM devices WHERE id=$1`, deviceID).Scan(&hostname, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrOffline
	}
	ice, err := s.ICE.ICEServers(ctx, fmt.Sprintf("u%d", u.ID))
	if err != nil {
		return nil, err
	}
	logID, mustRecord, err := s.Sessions.Start(ctx, StartParams{DeviceID: deviceID, UserID: u.ID, Kind: kind, ViewerIP: viewerIP, Record: record})
	if err != nil {
		return nil, err
	}
	ss := &session{
		ID: newSessionID(), DeviceID: deviceID, Hostname: hostname, DeviceCode: code, Kind: kind,
		UserID: u.ID, UserName: u.Name, UserEmail: u.Email, ViewerIP: viewerIP, Record: mustRecord,
		LogID: logID, Created: time.Now(), done: make(chan struct{}),
	}
	s.mu.Lock()
	s.sessions[ss.ID] = ss
	s.mu.Unlock()

	agentICE, _ := s.ICE.ICEServers(ctx, fmt.Sprintf("d%d", deviceID))
	err = c.send(map[string]any{
		"type": "session_request", "session_id": ss.ID, "kind": kind, "user_name": u.Name,
		"recording": mustRecord, "ice_servers": agentICE,
	})
	if err != nil {
		s.endSession(ss.ID, 0, "agent_unreachable")
		return nil, ErrOffline
	}
	s.Audit.Record(ctx, audit.Actor{UserID: u.ID, Email: u.Email, IP: viewerIP}, "session.started", hostname,
		map[string]any{"device_id": deviceID, "kind": kind, "session": ss.ID, "recording": mustRecord})
	go func() {
		select {
		case <-time.After(s.RequestTimeout):
			ss.mu.Lock()
			paired := ss.paired
			ss.mu.Unlock()
			if !paired {
				s.endSession(ss.ID, 0, "timeout")
			}
		case <-ss.done:
		}
	}()
	return &StartResult{SessionID: ss.ID, DeviceID: deviceID, ICEServers: ice}, nil
}

func (s *Service) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Service) agentAccepted(deviceID int64, id string) {
	if ss := s.session(id); ss != nil && ss.DeviceID == deviceID {
		ss.mu.Lock()
		ss.accepted = true
		ss.mu.Unlock()
	}
}

// endSession closes a session. deviceID, when not 0, must match (an agent can only end its own sessions).
func (s *Service) endSession(id string, deviceID int64, reason string) {
	ss := s.session(id)
	if ss == nil || (deviceID != 0 && ss.DeviceID != deviceID) {
		return
	}
	ss.endOnce.Do(func() {
		s.mu.Lock()
		delete(s.sessions, id)
		c := s.conns[ss.DeviceID]
		s.mu.Unlock()
		ss.mu.Lock()
		ss.reason = reason
		peers := ss.peers
		agentJoined := peers[sideAgent] != nil
		ss.mu.Unlock()
		close(ss.done)
		msg := mustJSON(map[string]string{"type": "end", "reason": reason})
		for _, p := range peers {
			if p != nil {
				// Closing waits for the peer's close frame, so do it in the background.
				go func(c *websocket.Conn) {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					c.Write(ctx, websocket.MessageText, msg)
					cancel()
					c.Close(websocket.StatusNormalClosure, "session ended")
				}(p.conn)
			}
		}
		if !agentJoined && c != nil {
			go c.send(map[string]string{"type": "session_cancel", "session_id": id})
		}
		ctx := context.Background()
		if err := s.Sessions.End(ctx, ss.LogID, reason); err != nil {
			slog.Error("session log end", "err", err)
		}
		s.Audit.Record(ctx, audit.Actor{UserID: ss.UserID, Email: ss.UserEmail, IP: ss.ViewerIP}, "session.ended", ss.Hostname,
			map[string]any{"device_id": ss.DeviceID, "session": id, "reason": reason, "duration_s": int(time.Since(ss.Created).Seconds())})
		slog.Info("session ended", "session", id, "device", ss.DeviceID, "reason", reason)
	})
}

func (s *Service) endDeviceSessions(deviceID int64, reason string) {
	s.mu.Lock()
	var ids []string
	for id, ss := range s.sessions {
		if ss.DeviceID == deviceID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.endSession(id, 0, reason)
	}
}

// signal is the part of a session frame the server looks at.
type signal struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Transport string `json:"transport"`
}

// join attaches one side's socket to the session and forwards its frames until either side leaves.
func (s *Service) join(ss *session, side int, ws *websocket.Conn) {
	p := &peer{conn: ws, out: make(chan frame, 256)}
	ss.mu.Lock()
	if ss.peers[side] != nil {
		ss.mu.Unlock()
		ws.Close(websocket.StatusPolicyViolation, "already connected")
		return
	}
	select {
	case <-ss.done:
		ss.mu.Unlock()
		ws.Close(websocket.StatusNormalClosure, "session ended")
		return
	default:
	}
	ss.peers[side] = p
	both := ss.peers[0] != nil && ss.peers[1] != nil
	if both {
		ss.paired = true
	}
	peers := ss.peers
	ss.mu.Unlock()
	ws.SetReadLimit(16 << 20)

	// The read context is not tied to ss.done: cancelling a read closes the socket at once,
	// and endSession still has to send the "end" frame. endSession closes the socket instead.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Writer for this side.
	go func() {
		for {
			select {
			case f := <-p.out:
				wctx, wc := context.WithTimeout(ctx, 15*time.Second)
				err := ws.Write(wctx, f.typ, f.data)
				wc()
				if err != nil {
					s.endSession(ss.ID, 0, sideName[side]+"_write_failed")
					return
				}
			case <-ss.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	if both {
		for i, q := range peers {
			q.out <- frame{websocket.MessageText, mustJSON(map[string]any{"type": "ready", "peer": sideName[1-i], "session_id": ss.ID})}
		}
	}

	other := 1 - side
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			s.endSession(ss.ID, 0, sideName[side]+"_left")
			return
		}
		if typ == websocket.MessageText {
			var sg signal
			if json.Unmarshal(data, &sg) != nil {
				continue
			}
			switch sg.Type {
			case "end":
				s.endSession(ss.ID, 0, sideName[side]+":"+clean(sg.Reason, 40))
				return
			case "transport":
				// The viewer reports which path connected: p2p, turn or websocket.
				if side == sideViewer && (sg.Transport == "p2p" || sg.Transport == "turn" || sg.Transport == "websocket") {
					ss.mu.Lock()
					changed := ss.transport != sg.Transport
					ss.transport = sg.Transport
					ss.mu.Unlock()
					if changed {
						s.Sessions.SetTransport(context.Background(), ss.LogID, sg.Transport)
					}
				}
				continue
			}
		}
		ss.mu.Lock()
		q := ss.peers[other]
		ss.mu.Unlock()
		if q == nil {
			continue // not paired yet; both sides wait for "ready" before sending
		}
		f := frame{typ, data}
		if typ == websocket.MessageBinary && len(data) > 0 && data[0]&1 == 0 {
			// Relay video: drop delta frames when the receiver falls behind. The viewer asks for a key frame.
			select {
			case q.out <- f:
			default:
			}
			continue
		}
		select {
		case q.out <- f:
		case <-ss.done:
			return
		case <-time.After(10 * time.Second):
			s.endSession(ss.ID, 0, sideName[other]+"_stalled")
			return
		}
	}
}

// SessionInfo is what the viewer page shows about a session.
type SessionInfo struct {
	SessionID  string      `json:"session_id"`
	DeviceID   int64       `json:"device_id"`
	Hostname   string      `json:"hostname"`
	DeviceCode string      `json:"device_code"`
	Kind       string      `json:"kind"`
	Recording  bool        `json:"recording"`
	LogID      int64       `json:"log_id"`
	ICEServers []ICEServer `json:"ice_servers"`
}
