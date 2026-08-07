// Package stt provides speech-to-text via ElevenLabs Scribe v2 Realtime.
//
// Uses the ElevenLabs WebSocket API at wss://api.elevenlabs.io/v1/speech-to-text/realtime.
// Accepts PCM int16 audio and streams transcription results.
//
// API reference: https://elevenlabs.io/docs/api-reference/speech-to-text/v-1-speech-to-text-realtime
// Event reference: https://elevenlabs.io/docs/eleven-api/guides/how-to/speech-to-text/realtime/event-reference
package stt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ElevenLabsTranscriber streams PCM audio to ElevenLabs and returns transcriptions.
type ElevenLabsTranscriber struct {
	apiKey  string
	results chan Result
	errs    chan error
	ws      *websocket.Conn
	mu      sync.Mutex
	done    chan struct{}
	ready   chan struct{} // closed when session_started is received
}

// ElevenLabsSTTOpts configures the ElevenLabs STT transcriber.
type ElevenLabsSTTOpts struct {
	APIKey     string // If empty, reads ELEVENLABS_API_KEY from env.
	Language   string // ISO 639-1 language code (default "en").
	Model      string // STT model (default "scribe_v2_realtime").
	SampleRate int    // Audio sample rate in Hz (default 16000).
}

// NewElevenLabsTranscriber creates a new ElevenLabs streaming STT transcriber.
// Per the API reference, all config is passed as query parameters.
// The server sends a session_started event to confirm.
func NewElevenLabsTranscriber(opts ElevenLabsSTTOpts) (*ElevenLabsTranscriber, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("ELEVENLABS_API_KEY not set")
	}

	model := opts.Model
	if model == "" {
		model = "scribe_v2_realtime"
	}
	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	sampleRate := opts.SampleRate
	if sampleRate == 0 {
		sampleRate = 16000
	}

	// Config via query params per the API reference.
	u := url.URL{
		Scheme: "wss",
		Host:   "api.elevenlabs.io",
		Path:   "/v1/speech-to-text/realtime",
		RawQuery: fmt.Sprintf(
			"model_id=%s&audio_format=pcm_%d&language_code=%s&commit_strategy=vad",
			model, sampleRate, lang,
		),
	}

	header := http.Header{"xi-api-key": []string{apiKey}}
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs stt dial: %w", err)
	}

	t := &ElevenLabsTranscriber{
		apiKey:  apiKey,
		results: make(chan Result, 64),
		errs:    make(chan error, 1),
		ws:      ws,
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
	}

	go t.readResults()
	return t, nil
}

// readResults reads events from the ElevenLabs WebSocket.
// Event reference: session_started, partial_transcript, committed_transcript, error.
func (t *ElevenLabsTranscriber) readResults() {
	defer close(t.done)
	for {
		_, msg, err := t.ws.ReadMessage()
		if err != nil {
			select {
			case t.errs <- fmt.Errorf("elevenlabs stt read: %w", err):
			default:
			}
			return
		}

		var event struct {
			MessageType string `json:"message_type"`
			Type        string `json:"type"`
			Text        string `json:"text"`
			Error       string `json:"error"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(msg, &event); err != nil {
			log.Printf("stt: parse error: %v (raw: %s)", err, string(msg))
			continue
		}

		// Normalize: the API uses "message_type" in some events.
		eventType := event.MessageType
		if eventType == "" {
			eventType = event.Type
		}

		switch eventType {
		case "session_started":
			// Server confirms the session is ready. We can now send audio.
			log.Printf("stt: session started")
			close(t.ready)

		case "partial_transcript":
			if event.Text != "" {
				t.results <- Result{
					Text:       event.Text,
					IsFinal:    false,
					Confidence: 0.9,
					Timestamp:  time.Now(),
				}
			}

		case "committed_transcript":
			if event.Text != "" {
				t.results <- Result{
					Text:       event.Text,
					IsFinal:    true,
					Confidence: 0.9,
					Timestamp:  time.Now(),
				}
			}

		case "error", "auth_error", "quota_exceeded", "transcriber_error",
			"input_error", "rate_limited", "queue_overflow", "resource_exhausted",
			"session_time_limit_exceeded", "chunk_size_exceeded", "insufficient_audio_activity":
			errMsg := event.Error
			if errMsg == "" {
				errMsg = event.Message
			}
			if errMsg == "" {
				errMsg = event.Type
			}
			log.Printf("stt: server error: %s", errMsg)
			select {
			case t.errs <- fmt.Errorf("elevenlabs stt: %s", errMsg):
			default:
			}

		default:
			log.Printf("stt: unknown event: %s", string(msg))
		}
	}
}

// Write sends a PCM int16 audio chunk to ElevenLabs.
// Blocks until the session_started event is received.
func (t *ElevenLabsTranscriber) Write(pcmPayload []byte) error {
	// Wait for session_started before sending audio.
	select {
	case <-t.ready:
	case <-t.done:
		return fmt.Errorf("transcriber closed before session started")
	}

	encoded := base64.StdEncoding.EncodeToString(pcmPayload)
	msg := map[string]string{
		"message_type":  "input_audio_chunk",
		"audio_base_64": encoded,
	}
	return t.sendJSON(msg)
}

func (t *ElevenLabsTranscriber) sendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ws.WriteMessage(websocket.TextMessage, b)
}

// Results returns the channel of transcription results.
func (t *ElevenLabsTranscriber) Results() <-chan Result {
	return t.results
}

// Errors returns the channel of errors.
func (t *ElevenLabsTranscriber) Errors() <-chan error {
	return t.errs
}

// Close closes the WebSocket connection.
// In VAD mode, the server handles commits automatically.
func (t *ElevenLabsTranscriber) Close() error {
	// Just close the WebSocket -- no end_of_stream message needed.
	// The API schema only defines input_audio_chunk as a client message.
	t.mu.Lock()
	t.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	t.mu.Unlock()
	<-t.done
	return t.ws.Close()
}
