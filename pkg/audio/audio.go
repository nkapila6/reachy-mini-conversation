// Package audio provides audio playback utilities for the Reachy Mini.
//
// Two output paths:
//  1. Exec a decoder child (ffplay/ffmpeg/mpv) and pipe Opus to stdin.
//  2. Open a WebRTC peer to the daemon's webrtcsink and forward Opus RTP.
//
// The daemon path is the default on a vanilla Reachy Mini since the
// daemon owns the USB speaker.
package audio

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"time"

	"github.com/nkapila6/reachy-utils/pkg/daemon"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// Sink is the common interface for an audio output path.
type Sink interface {
	WriteRTP(*rtp.Packet) error
	Close()
}

// Player feeds Opus RTP packets into a decoder child process (ffplay/ffmpeg/mpv).
type Player struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	ogg *oggwriter.OggWriter
}

var playerCandidates = [][]string{
	{"ffplay", "-hide_banner", "-loglevel", "error", "-nodisp", "-fflags", "nobuffer", "-i", "pipe:0"},
	{"ffmpeg", "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-f", "alsa", "default"},
	{"mpv", "--really-quiet", "--no-video", "-"},
}

// NewPlayer creates a player that pipes Opus to a decoder child.
func NewPlayer(cmd []string, label string) (*Player, error) {
	c := exec.Command(cmd[0], cmd[1:]...)
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	ogg, err := oggwriter.NewWith(stdin, 48000, 2)
	if err != nil {
		_ = c.Process.Kill()
		return nil, err
	}
	log.Printf("audio: playing %s via %s", label, cmd[0])
	return &Player{cmd: c, in: stdin, ogg: ogg}, nil
}

func (p *Player) WriteRTP(pkt *rtp.Packet) error { return p.ogg.WriteRTP(pkt) }

func (p *Player) Close() {
	_ = p.ogg.Close()
	_ = p.in.Close()
	_ = p.cmd.Wait()
}

// DaemonSink publishes Opus RTP into the daemon's speaker via WebRTC.
type DaemonSink struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP
	sig   *daemon.SignalingClient

	sessionID string
}

// NewDaemonSink opens a WebRTC peer to the daemon's webrtcsink for audio playback.
func NewDaemonSink(robotAddr, label string) (*DaemonSink, error) {
	sig, err := daemon.DialSignaling(robotAddr)
	if err != nil {
		return nil, fmt.Errorf("audio signaling: %w", err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		sig.Close()
		return nil, fmt.Errorf("audio peerconn: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "reachy-utils",
	)
	if err != nil {
		pc.Close()
		sig.Close()
		return nil, fmt.Errorf("audio track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		sig.Close()
		return nil, fmt.Errorf("audio add track: %w", err)
	}

	d := &DaemonSink{pc: pc, track: track, sig: sig}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ji := c.ToJSON()
		if d.sessionID == "" {
			return
		}
		sig.Send(daemon.SignalingMsg{
			Type:      "peer",
			SessionID: d.sessionID,
			ICE: &daemon.ICEInfo{
				Candidate:     ji.Candidate,
				SDPMLineIndex: ji.SDPMLineIndex,
				SDPMid:        ji.SDPMid,
			},
		})
	})

	// Handshake: welcome -> setPeerStatus(listener) -> list -> startSession -> handle offer.
	msg, err := sig.Read()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("welcome: %w", err)
	}
	if msg.Type != "welcome" {
		d.Close()
		return nil, fmt.Errorf("expected welcome, got %q", msg.Type)
	}
	if err := sig.Send(daemon.SignalingMsg{Type: "setPeerStatus", Roles: []string{"listener"}}); err != nil {
		d.Close()
		return nil, fmt.Errorf("setPeerStatus: %w", err)
	}

	msg, err = sig.Read()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("peerStatusChanged: %w", err)
	}
	if err := sig.Send(daemon.SignalingMsg{Type: "list"}); err != nil {
		d.Close()
		return nil, fmt.Errorf("list: %w", err)
	}

	msg, err = sig.Read()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("list response: %w", err)
	}
	producerID := daemon.PickProducer(msg.Producers, "reachymini")
	if producerID == "" {
		d.Close()
		return nil, fmt.Errorf("no producer found")
	}
	if err := sig.Send(daemon.SignalingMsg{Type: "startSession", PeerID: producerID}); err != nil {
		d.Close()
		return nil, fmt.Errorf("startSession: %w", err)
	}

	msg, err = sig.Read()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("sessionStarted: %w", err)
	}
	d.sessionID = msg.SessionID

	msg, err = sig.Read()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("offer: %w", err)
	}
	if msg.SDP == nil || msg.SDP.Type != "offer" {
		d.Close()
		return nil, fmt.Errorf("expected SDP offer")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: msg.SDP.SDP,
	}); err != nil {
		d.Close()
		return nil, fmt.Errorf("set remote: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		d.Close()
		return nil, fmt.Errorf("set local: %w", err)
	}
	if err := sig.Send(daemon.SignalingMsg{
		Type:      "peer",
		SessionID: d.sessionID,
		SDP:       &daemon.SDPInfo{Type: "answer", SDP: answer.SDP},
	}); err != nil {
		d.Close()
		return nil, fmt.Errorf("send answer: %w", err)
	}

	// Handle ICE in background.
	go func() {
		for {
			msg, err := sig.Read()
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
				pc.AddICECandidate(init)
			}
		}
	}()

	log.Printf("audio: playing %s via daemon webrtc", label)
	return d, nil
}

func (d *DaemonSink) WriteRTP(pkt *rtp.Packet) error {
	return d.track.WriteRTP(pkt)
}

func (d *DaemonSink) Close() {
	if d.sessionID != "" {
		d.sig.Send(daemon.SignalingMsg{Type: "endSession", SessionID: d.sessionID})
	}
	d.pc.Close()
	d.sig.Close()
}

// FindSink picks the right audio output path. Order:
//   - explicit -player override -> exec'd shell command
//   - ffplay/ffmpeg/mpv on PATH -> exec'd child
//   - reachy_mini daemon signaling reachable -> daemon WebRTC peer
//   - otherwise error
func FindSink(override, robotAddr string) (Sink, string, error) {
	if override != "" {
		cmd := []string{"sh", "-c", override + " <&0"}
		p, err := NewPlayer(cmd, "custom")
		return p, "custom -player command", err
	}
	for _, c := range playerCandidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			p, err := NewPlayer(c, c[0])
			return p, c[0], err
		}
	}
	if daemon.SignalingReachable(robotAddr, 500*time.Millisecond) {
		p, err := NewDaemonSink(robotAddr, "reachy-utils")
		return p, "reachy_mini daemon (webrtc)", err
	}
	return nil, "", fmt.Errorf("no audio path found: install ffplay/ffmpeg/mpv, run a reachy_mini daemon at %s, or pass -player", robotAddr)
}
