// Package bridge communicates with the Python audio bridge process.
//
// The bridge uses the reachy_mini SDK to handle mic capture and speaker
// playback. It communicates via JSON lines on stdin/stdout.
//
// This replaces the raw WebRTC consumer (pkg/capture) and ffmpeg decoder
// (pkg/audio/opus_decoder.go) with a single child process that uses the
// official SDK -- same approach as the conversation app.
package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Client manages the audio bridge child process.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	audioCh   chan []byte   // PCM int16 audio from mic
	playingCh chan struct{} // notified when TTS audio starts playing
	errCh     chan error

	mu     sync.Mutex
	ready  bool
	closed bool
	cancel context.CancelFunc
}

// BridgeMsg is a JSON message to/from the bridge.
type BridgeMsg struct {
	Type    string `json:"type"`
	PCM     string `json:"pcm,omitempty"`     // base64 PCM int16
	Text    string `json:"text,omitempty"`    // text for local TTS
	Message string `json:"message,omitempty"` // error message
}

// Start launches the audio bridge Python process.
// robotHost is the Reachy hostname (without port).
// Looks for Python in: .venv/bin/python3, then system python3.
func Start(robotHost string) (*Client, error) {
	python := findPython()
	script := findScript("audio_bridge.py")

	log.Printf("bridge: starting %s %s --robot %s", python, script, robotHost)
	cmd := exec.Command(python, script, "--robot", robotHost)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bridge start: %w", err)
	}

	c := &Client{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewScanner(stdout),
		audioCh:   make(chan []byte, 256),
		playingCh: make(chan struct{}, 8),
		errCh:     make(chan error, 1),
	}

	// Wait for the "ready" message.
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		cmd.Process.Kill()
		return nil, fmt.Errorf("bridge: no ready message (did it crash?)")
	}
	line := scanner.Text()
	var msg BridgeMsg
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("bridge: bad JSON: %s", line)
	}
	if msg.Type == "error" {
		cmd.Process.Kill()
		return nil, fmt.Errorf("bridge error: %s", msg.Message)
	}
	if msg.Type != "ready" {
		cmd.Process.Kill()
		return nil, fmt.Errorf("bridge: expected ready, got %q", msg.Type)
	}

	c.ready = true
	log.Printf("bridge: audio bridge ready")

	// Read audio messages in background.
	go c.readLoop()

	return c, nil
}

// readLoop reads audio messages from the bridge's stdout.
func (c *Client) readLoop() {
	for c.stdout.Scan() {
		line := c.stdout.Text()
		var msg BridgeMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "audio":
			if msg.PCM != "" {
				pcm, err := base64.StdEncoding.DecodeString(msg.PCM)
				if err == nil && len(pcm) > 0 {
					select {
					case c.audioCh <- pcm:
					default:
						// Drop if channel is full (backpressure).
					}
				}
			}
		case "playing":
			select {
			case c.playingCh <- struct{}{}:
			default:
			}
		case "error":
			select {
			case c.errCh <- fmt.Errorf("bridge: %s", msg.Message):
			default:
			}
		}
	}
	// Scanner stopped -- bridge exited.
	select {
	case c.errCh <- fmt.Errorf("bridge exited"):
	default:
	}
}

// Audio returns a channel of PCM int16 audio from the microphone.
func (c *Client) Audio() <-chan []byte {
	return c.audioCh
}

// Playing returns a channel that fires when TTS audio starts playing.
func (c *Client) Playing() <-chan struct{} {
	return c.playingCh
}

// Errors returns a channel of bridge errors.
func (c *Client) Errors() <-chan error {
	return c.errCh
}

// SpeakPCM sends audio to the robot's speaker (for cloud TTS).
// sampleRate is the PCM's source sample rate (e.g. 24000 for Kokoro).
// format is "pcm" or "mp3".
func (c *Client) SpeakPCM(audio []byte, sampleRate int, format string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("bridge closed")
	}
	msg := map[string]any{
		"type":        "speak_pcm",
		"pcm":         base64.StdEncoding.EncodeToString(audio),
		"sample_rate": sampleRate,
		"format":      format,
	}
	b, _ := json.Marshal(msg)
	_, err := fmt.Fprintln(c.stdin, string(b))
	return err
}

// SpeakText sends text to the bridge for local TTS synthesis (Kokoro).
func (c *Client) SpeakText(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("bridge closed")
	}
	msg := BridgeMsg{Type: "speak", Text: text}
	b, _ := json.Marshal(msg)
	_, err := fmt.Fprintln(c.stdin, string(b))
	return err
}

// StopSpeaking clears the speaker queue (barge-in).
func (c *Client) StopSpeaking() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	msg := BridgeMsg{Type: "stop_speaking"}
	b, _ := json.Marshal(msg)
	_, err := fmt.Fprintln(c.stdin, string(b))
	return err
}

// Close shuts down the bridge process.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	fmt.Fprintln(c.stdin, `{"type":"quit"}`)
	c.stdin.Close()
	c.cmd.Wait()
}

// findPython looks for a Python with the reachy_mini SDK available.
// Order: .venv/bin/python3 (uv-managed), then system python3.
func findPython() string {
	// Check for a .venv next to the binary first.
	if exe, err := os.Executable(); err == nil {
		venv := filepath.Join(filepath.Dir(exe), ".venv", "bin", "python3")
		if _, err := os.Stat(venv); err == nil {
			return venv
		}
	}
	// Check current directory.
	if _, err := os.Stat(".venv/bin/python3"); err == nil {
		return ".venv/bin/python3"
	}
	return "python3"
}

// findScript looks for a Python script next to the binary, then in cwd.
func findScript(name string) string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return name
}
