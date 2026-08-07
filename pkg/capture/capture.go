// Package capture grabs microphone audio from the Reachy Mini daemon's
// webrtcsink. It acts as a WebRTC consumer: connects to the daemon's
// signaling endpoint, negotiates a peer connection, and receives the
// Opus audio track published by the robot's microphone.
//
// This is the reverse of the audio playback path in daemon_audio.go.
// Instead of publishing TO the daemon, we consume FROM it.
package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/nkapila6/reachy-utils/pkg/daemon"
	"github.com/pion/webrtc/v4"
)

const (
	// The producer name the daemon registers for the microphone.
	micProducerName = "reachymini"
)

// MicStream delivers captured Opus audio packets.
// Each call to Read returns one RTP packet's worth of Opus data.
type MicStream struct {
	pc        *webrtc.PeerConnection
	sig       *daemon.SignalingClient
	sessionID string

	trackCh chan []byte // buffered channel of Opus payloads
	errCh   chan error

	closeOnce sync.Once
	cancel    context.CancelFunc
}

// OpenMic connects to the daemon's webrtcsink and opens the microphone
// audio stream. robotAddr is "host:8000" (the daemon's HTTP port).
// Returns a MicStream that can be read from.
func OpenMic(robotAddr string) (*MicStream, error) {
	sig, err := daemon.DialSignaling(robotAddr)
	if err != nil {
		return nil, fmt.Errorf("mic signaling: %w", err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		sig.Close()
		return nil, fmt.Errorf("mic peerconn: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &MicStream{
		pc:      pc,
		sig:     sig,
		trackCh: make(chan []byte, 256),
		errCh:   make(chan error, 1),
		cancel:  cancel,
	}

	// Handle incoming audio tracks.
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		log.Printf("mic: received track %s (%s)", track.ID(), track.Codec().MimeType)
		go m.readTrack(ctx, track)
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			select {
			case m.errCh <- fmt.Errorf("mic peer connection %s", s):
			default:
			}
		}
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ji := c.ToJSON()
		if m.sessionID == "" {
			return
		}
		sig.Send(daemon.SignalingMsg{
			Type:      "peer",
			SessionID: m.sessionID,
			ICE: &daemon.ICEInfo{
				Candidate:     ji.Candidate,
				SDPMLineIndex: ji.SDPMLineIndex,
				SDPMid:        ji.SDPMid,
			},
		})
	})

	// Run the signaling handshake.
	if err := m.handshake(); err != nil {
		m.Close()
		return nil, err
	}

	return m, nil
}

// handshake runs the gstwebrtc-api signaling handshake as a consumer.
func (m *MicStream) handshake() error {
	// 1. Wait for welcome, then register as listener.
	msg, err := m.sig.Read()
	if err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if msg.Type != "welcome" {
		return fmt.Errorf("expected welcome, got %q", msg.Type)
	}
	if err := m.sig.Send(daemon.SignalingMsg{Type: "setPeerStatus", Roles: []string{"listener"}}); err != nil {
		return fmt.Errorf("setPeerStatus: %w", err)
	}

	// 2. Wait for peerStatusChanged, then list producers.
	msg, err = m.sig.Read()
	if err != nil {
		return fmt.Errorf("peerStatusChanged: %w", err)
	}
	if err := m.sig.Send(daemon.SignalingMsg{Type: "list"}); err != nil {
		return fmt.Errorf("list: %w", err)
	}

	// 3. Get producer list, pick the mic producer, start session.
	msg, err = m.sig.Read()
	if err != nil {
		return fmt.Errorf("list response: %w", err)
	}
	log.Printf("mic: available producers:")
	for _, p := range msg.Producers {
		var meta struct{ Name string }
		json.Unmarshal(p.Meta, &meta)
		log.Printf("mic:   - id=%s name=%q", p.ID, meta.Name)
	}
	producerID := daemon.PickProducer(msg.Producers, micProducerName)
	if producerID == "" {
		return fmt.Errorf("no producer named %q found among %d producers", micProducerName, len(msg.Producers))
	}
	log.Printf("mic: picked producer %s", producerID)
	if err := m.sig.Send(daemon.SignalingMsg{Type: "startSession", PeerID: producerID}); err != nil {
		return fmt.Errorf("startSession: %w", err)
	}

	// 4. Get sessionStarted, then handle SDP offer.
	msg, err = m.sig.Read()
	if err != nil {
		return fmt.Errorf("sessionStarted: %w", err)
	}
	if msg.Type != "sessionStarted" {
		return fmt.Errorf("expected sessionStarted, got %q", msg.Type)
	}
	m.sessionID = msg.SessionID

	// 5. Handle the SDP offer from the daemon.
	msg, err = m.sig.Read()
	if err != nil {
		return fmt.Errorf("offer: %w", err)
	}
	if msg.SDP == nil || msg.SDP.Type != "offer" {
		return fmt.Errorf("expected SDP offer, got type=%q", msg.Type)
	}
	if err := m.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: msg.SDP.SDP,
	}); err != nil {
		return fmt.Errorf("set remote: %w", err)
	}
	answer, err := m.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := m.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local: %w", err)
	}
	if err := m.sig.Send(daemon.SignalingMsg{
		Type:      "peer",
		SessionID: m.sessionID,
		SDP:       &daemon.SDPInfo{Type: "answer", SDP: answer.SDP},
	}); err != nil {
		return fmt.Errorf("send answer: %w", err)
	}

	// 6. Handle ICE candidates in background.
	go m.readSignaling()

	return nil
}

// readSignaling handles ongoing signaling messages (ICE candidates, etc.).
func (m *MicStream) readSignaling() {
	for {
		msg, err := m.sig.Read()
		if err != nil {
			return
		}
		if msg.ICE != nil && msg.ICE.Candidate != "" {
			init := webrtc.ICECandidateInit{Candidate: msg.ICE.Candidate}
			if msg.ICE.SDPMLineIndex != nil {
				init.SDPMLineIndex = msg.ICE.SDPMLineIndex
			}
			if msg.ICE.SDPMid != nil {
				init.SDPMid = msg.ICE.SDPMid
			}
			if err := m.pc.AddICECandidate(init); err != nil {
				log.Printf("mic: add ice: %v", err)
			}
		}
		if msg.Type == "error" {
			log.Printf("mic: signaling error: %s", msg.Error)
		}
	}
}

// readTrack reads RTP packets from a remote track and pushes Opus payloads
// onto the channel.
func (m *MicStream) readTrack(ctx context.Context, track *webrtc.TrackRemote) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("mic: read rtp: %v", err)
			}
			return
		}
		// Copy the payload since the RTP packet buffer is reused.
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)
		select {
		case m.trackCh <- payload:
		case <-ctx.Done():
			return
		}
	}
}

// ReadOpus returns the next Opus audio payload from the microphone.
// Blocks until data is available or the stream is closed.
func (m *MicStream) ReadOpus() ([]byte, error) {
	select {
	case pkt, ok := <-m.trackCh:
		if !ok {
			return nil, io.EOF
		}
		return pkt, nil
	case err := <-m.errCh:
		return nil, err
	}
}

// OpusPackets returns a channel of Opus payloads. Useful for piping
// directly into an STT stream.
func (m *MicStream) OpusPackets() <-chan []byte {
	return m.trackCh
}

// Errors returns a channel that receives stream errors.
func (m *MicStream) Errors() <-chan error {
	return m.errCh
}

// Close shuts down the microphone stream.
func (m *MicStream) Close() {
	m.closeOnce.Do(func() {
		m.cancel()
		if m.sessionID != "" {
			m.sig.Send(daemon.SignalingMsg{Type: "endSession", SessionID: m.sessionID})
		}
		m.pc.Close()
		m.sig.Close()
	})
}

// Wait blocks until the stream encounters an error or is closed.
func (m *MicStream) Wait() error {
	return <-m.errCh
}
