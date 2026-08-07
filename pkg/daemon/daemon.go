// Package daemon is a Go client for the Reachy Mini daemon.
//
// It handles waking the robot, connecting to the motion WebSocket
// (ws://host:8000/ws/sdk), and sending set_full_target poses at 50 Hz.
// This is the same protocol the Python SDK's WSClient speaks.
//
// Also provides the WebRTC signaling client for the daemon's webrtcsink
// (ws://host:8443) used by both mic capture and audio playback.
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// EnsureReady starts the daemon's hardware backend and wakes the robot,
// then polls until the backend is alive. Idempotent -- safe to call
// after a reboot or goto_sleep.
func EnsureReady(robotAddr string) error {
	base := "http://" + robotAddr
	client := &http.Client{Timeout: 5 * time.Second}

	log.Printf("daemon: POST %s/api/daemon/start?wake_up=true", base)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/daemon/start?wake_up=true", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon start: %w (is the reachy_mini daemon running at %s?)", err, robotAddr)
	}
	resp.Body.Close()
	log.Printf("daemon: wake_up request sent (HTTP %d)", resp.StatusCode)

	deadline := time.Now().Add(25 * time.Second)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		r, err := client.Get(base + "/api/daemon/status")
		if err == nil {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if !bytes.Contains(body, []byte(`"backend_status":null`)) {
				log.Printf("daemon: backend is up")
				time.Sleep(2 * time.Second)
				return nil
			}
			log.Printf("daemon: waiting for backend... (status still null)")
		} else {
			log.Printf("daemon: status check failed: %v (retrying...)", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon backend did not come up within 25s")
		}
		<-tick.C
	}
}

// Daemon is a connection to the Reachy Mini's motion WebSocket.
type Daemon struct {
	ws  *websocket.Conn
	out chan []byte
}

// FullTarget is the JSON payload for set_full_target.
type FullTarget struct {
	Type     string    `json:"type"` // "set_full_target"
	Head     []float64 `json:"head,omitempty"`
	Antennas []float64 `json:"antennas,omitempty"`
	BodyYaw  *float64  `json:"body_yaw,omitempty"`
}

// Dial connects to the daemon's motion WebSocket at ws://addr/ws/sdk.
func Dial(addr string) (*Daemon, error) {
	url := fmt.Sprintf("ws://%s/ws/sdk", addr)
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("daemon at %s: %w", url, err)
	}
	d := &Daemon{ws: ws, out: make(chan []byte, 64)}

	// Drain server messages (status/joint streams) so the socket stays healthy.
	go func() {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Single writer goroutine -- gorilla allows only one concurrent writer.
	go func() {
		for msg := range d.out {
			if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("daemon write failed: %v", err)
				return
			}
		}
	}()
	return d, nil
}

func (d *Daemon) send(v any) {
	b, _ := json.Marshal(v)
	select {
	case d.out <- b:
	default: // Drop rather than stall the caller.
	}
}

// SetFullTarget sends a head/antenna/body pose to the robot.
func (d *Daemon) SetFullTarget(head [16]float64, antennas [2]float64, bodyYaw float64) {
	yaw := bodyYaw
	d.send(FullTarget{
		Type:     "set_full_target",
		Head:     head[:],
		Antennas: antennas[:],
		BodyYaw:  &yaw,
	})
}

// Command sends a simple command like wake_up or goto_sleep.
func (d *Daemon) Command(typ string) {
	d.send(map[string]string{"type": typ})
}

// Close sends goto_sleep, waits for the robot to settle, then disconnects.
func (d *Daemon) Close() {
	d.Command("goto_sleep")
	time.Sleep(1500 * time.Millisecond)
	close(d.out)
	_ = d.ws.Close()
}

// --- WebRTC signaling client for the daemon's webrtcsink ---

// SignalingClient manages a WebSocket connection to the daemon's
// gstwebrtc-api signaling endpoint (ws://host:8443). Used by both
// mic capture (consumer) and audio playback (producer) paths.
type SignalingClient struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	url string
}

// SignalingMsg is a gstwebrtc-api protocol message.
type SignalingMsg struct {
	Type      string          `json:"type"`
	PeerID    string          `json:"peerId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Roles     []string        `json:"roles,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	Producers []ProducerInfo  `json:"producers,omitempty"`
	SDP       *SDPInfo        `json:"sdp,omitempty"`
	ICE       *ICEInfo        `json:"ice,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ProducerInfo describes a media producer on the daemon.
type ProducerInfo struct {
	ID   string          `json:"id"`
	Meta json.RawMessage `json:"meta"`
}

// SDPInfo carries an SDP offer or answer.
type SDPInfo struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// ICEInfo carries an ICE candidate.
type ICEInfo struct {
	Candidate     string  `json:"candidate"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
	SDPMid        *string `json:"sdpMid,omitempty"`
}

// DialSignaling connects to the daemon's WebRTC signaling endpoint.
func DialSignaling(robotAddr string) (*SignalingClient, error) {
	host, _, err := splitHostPort(robotAddr)
	if err != nil {
		host = robotAddr
	}
	url := fmt.Sprintf("ws://%s:8443", host)
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("signaling dial %s: %w", url, err)
	}
	return &SignalingClient{ws: ws, url: url}, nil
}

// Read reads the next signaling message.
func (s *SignalingClient) Read() (*SignalingMsg, error) {
	_, raw, err := s.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	var m SignalingMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse signaling msg: %w", err)
	}
	return &m, nil
}

// Send writes a signaling message.
func (s *SignalingClient) Send(m SignalingMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws.WriteMessage(websocket.TextMessage, b)
}

// Close closes the signaling connection.
func (s *SignalingClient) Close() error {
	return s.ws.Close()
}

// PickProducer finds a producer by name, falling back to the first available.
func PickProducer(producers []ProducerInfo, name string) string {
	for _, p := range producers {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(p.Meta, &meta); err == nil && meta.Name == name {
			return p.ID
		}
	}
	if len(producers) > 0 {
		return producers[0].ID
	}
	return ""
}

// SignalingReachable checks if the daemon's signaling port is reachable.
func SignalingReachable(robotAddr string, timeout time.Duration) bool {
	host, _, err := splitHostPort(robotAddr)
	if err != nil {
		host = robotAddr
	}
	addr := net.JoinHostPort(host, "8443")
	conn, err := (&net.Dialer{Timeout: timeout}).Dial("tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// splitHostPort splits host:port, returning host, port, nil.
func splitHostPort(addr string) (string, string, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		// If no port, return the whole string as host.
		return addr, "", nil
	}
	return h, p, nil
}
