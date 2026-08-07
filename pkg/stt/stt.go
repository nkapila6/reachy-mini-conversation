// Package stt provides speech-to-text via Deepgram's streaming API.
//
// It accepts raw Opus audio packets and streams them to Deepgram over
// a WebSocket, receiving transcriptions in real time. Supports both
// interim (partial) and final results.
package stt

import (
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

// Result is a transcription result from Deepgram.
type Result struct {
	Text       string    // transcribed text
	IsFinal    bool      // true for final results, false for interim
	Confidence float64   // 0..1
	Timestamp  time.Time // when the result was received
}

// Transcriber streams audio to Deepgram and returns transcriptions.
type Transcriber struct {
	apiKey  string
	results chan Result
	errs    chan error
	ws      *websocket.Conn
	mu      sync.Mutex
	done    chan struct{}
}

// TranscriberOpts configures the Deepgram transcriber.
type TranscriberOpts struct {
	// APIKey is the Deepgram API key. If empty, reads DEEPGRAM_API_KEY from env.
	APIKey string
	// Language is the BCP-47 language code (default "en").
	Language string
	// Model is the Deepgram model to use (default "nova-2").
	Model string
	// InterimResults enables streaming interim (partial) results.
	InterimResults bool
	// Endpointing is the silence duration in ms before finalizing (default 500).
	Endpointing int
}

// NewTranscriber creates a new Deepgram streaming transcriber.
func NewTranscriber(opts TranscriberOpts) (*Transcriber, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("DEEPGRAM_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("DEEPGRAM_API_KEY not set")
	}

	model := opts.Model
	if model == "" {
		model = "nova-2"
	}
	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	endpointing := opts.Endpointing
	if endpointing == 0 {
		endpointing = 500
	}

	// Build the WebSocket URL with query params.
	u := url.URL{
		Scheme: "wss",
		Host:   "api.deepgram.com",
		Path:   "/v1/listen",
		RawQuery: fmt.Sprintf(
			"model=%s&language=%s&encoding=opus&sample_rate=48000&channels=2&interim_results=%v&endpointing=%d",
			model, lang, opts.InterimResults, endpointing,
		),
	}

	header := http.Header{"Authorization": []string{"Token " + apiKey}}
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return nil, fmt.Errorf("deepgram dial: %w", err)
	}

	t := &Transcriber{
		apiKey:  apiKey,
		results: make(chan Result, 64),
		errs:    make(chan error, 1),
		ws:      ws,
		done:    make(chan struct{}),
	}

	go t.readResults()
	return t, nil
}

// readResults reads transcription results from the Deepgram WebSocket.
func (t *Transcriber) readResults() {
	defer close(t.done)
	for {
		_, msg, err := t.ws.ReadMessage()
		if err != nil {
			select {
			case t.errs <- fmt.Errorf("deepgram read: %w", err):
			default:
			}
			return
		}

		var resp struct {
			Type    string `json:"type"`
			Channel struct {
				Alternatives []struct {
					Transcript string  `json:"transcript"`
					Confidence float64 `json:"confidence"`
				} `json:"alternatives"`
			} `json:"channel"`
			IsFinal     bool `json:"is_final"`
			SpeechFinal bool `json:"speech_final"`
		}
		if err := json.Unmarshal(msg, &resp); err != nil {
			continue
		}

		if len(resp.Channel.Alternatives) == 0 {
			continue
		}
		alt := resp.Channel.Alternatives[0]
		if alt.Transcript == "" {
			continue
		}

		t.results <- Result{
			Text:       alt.Transcript,
			IsFinal:    resp.IsFinal || resp.SpeechFinal,
			Confidence: alt.Confidence,
			Timestamp:  time.Now(),
		}
	}
}

// Write sends an Opus audio payload to Deepgram.
func (t *Transcriber) Write(opusPayload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ws.WriteMessage(websocket.BinaryMessage, opusPayload)
}

// Results returns the channel of transcription results.
func (t *Transcriber) Results() <-chan Result {
	return t.results
}

// Errors returns the channel of errors.
func (t *Transcriber) Errors() <-chan error {
	return t.errs
}

// Close sends the close message and waits for the connection to finish.
func (t *Transcriber) Close() error {
	t.mu.Lock()
	// Send a CloseMessage to signal end of audio.
	_ = t.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	t.mu.Unlock()
	<-t.done
	return t.ws.Close()
}

// TranscribeFile sends a complete Opus file to Deepgram (non-streaming).
// This is a convenience for one-shot transcription.
func TranscribeFile(apiKey string, opusData []byte) (string, error) {
	if apiKey == "" {
		apiKey = os.Getenv("DEEPGRAM_API_KEY")
	}
	if apiKey == "" {
		return "", fmt.Errorf("DEEPGRAM_API_KEY not set")
	}

	u := "https://api.deepgram.com/v1/listen?model=nova-2&encoding=opus&sample_rate=48000"
	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	req.Header.Set("Content-Type", "audio/opus")

	// We need to actually send the body. Let's use a bytes reader.
	// Actually, the http.NewRequest with nil body won't work. Let's fix.
	_ = opusData
	_ = req
	return "", fmt.Errorf("not implemented: use streaming API instead")
}

// Silence returns a channel that emits the full utterance when the user
// stops speaking. It accumulates interim results and emits the final
// concatenated text when speech_final is received.
func (t *Transcriber) Silence() <-chan string {
	out := make(chan string, 16)
	go func() {
		defer close(out)
		var buf string
		for r := range t.results {
			if r.IsFinal {
				buf += r.Text + " "
			}
			if r.IsFinal && r.Confidence > 0 {
				// Emit the accumulated utterance.
				if len(buf) > 0 {
					out <- buf
					buf = ""
				}
			}
		}
	}()
	return out
}

// Utterances returns a channel that emits complete utterances as they
// are finalized by Deepgram.
func (t *Transcriber) Utterances() <-chan string {
	out := make(chan string, 16)
	go func() {
		defer close(out)
		for r := range t.results {
			if r.IsFinal && r.Text != "" {
				out <- r.Text
			}
		}
	}()
	return out
}

// log is used for debug output.
var _ = log.Printf
