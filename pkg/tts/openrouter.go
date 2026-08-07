// Package tts provides text-to-speech via OpenRouter's audio speech API.
//
// Uses the same OPENROUTER_API_KEY as the LLM. Supports the Kokoro-82M model
// and any other TTS model on OpenRouter.
//
// API reference: https://openrouter.ai/docs/agent-sdk/typescript/api-reference/tts
// Endpoint: POST https://openrouter.ai/api/v1/audio/speech
package tts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// OpenRouterSynthesizer converts text to speech via OpenRouter.
type OpenRouterSynthesizer struct {
	apiKey string
	model  string
	voice  string
	client *http.Client
}

// OpenRouterTTSOpts configures the OpenRouter TTS synthesizer.
type OpenRouterTTSOpts struct {
	// APIKey is the OpenRouter API key. If empty, reads OPENROUTER_API_KEY from env.
	APIKey string
	// Model is the TTS model (default "hexgrad/kokoro-82m").
	Model string
	// Voice is the voice name (default "am_onyx").
	Voice string
	// Speed is the speaking rate (default 1.0).
	Speed float64
	// ResponseFormat is the audio format (default "mp3", also "pcm", "wav", "opus").
	ResponseFormat string
}

// NewOpenRouterSynthesizer creates a new OpenRouter TTS synthesizer.
func NewOpenRouterSynthesizer(opts OpenRouterTTSOpts) (*OpenRouterSynthesizer, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY not set")
	}

	model := opts.Model
	if model == "" {
		model = "hexgrad/kokoro-82m"
	}
	voice := opts.Voice
	if voice == "" {
		voice = "am_onyx"
	}
	speed := opts.Speed
	if speed == 0 {
		speed = 1.0
	}
	format := opts.ResponseFormat
	if format == "" {
		format = "pcm"
	}

	return &OpenRouterSynthesizer{
		apiKey: apiKey,
		model:  model,
		voice:  voice,
		client: &http.Client{},
	}, nil
}

// ttsRequest is the JSON body for the OpenRouter TTS endpoint.
type orTTSRequest struct {
	Input          string  `json:"input"`
	Model          string  `json:"model"`
	Voice          string  `json:"voice"`
	Speed          float64 `json:"speed"`
	ResponseFormat string  `json:"response_format"`
}

// Synthesize converts text to audio bytes.
// Returns raw audio in the configured format (default PCM).
func (s *OpenRouterSynthesizer) Synthesize(text string) ([]byte, error) {
	body := orTTSRequest{
		Input:          text,
		Model:          s.model,
		Voice:          s.voice,
		Speed:          1.0,
		ResponseFormat: "pcm",
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal tts request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/audio/speech", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter tts request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openrouter tts: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// SynthesizeStream converts text to speech and streams audio chunks.
func (s *OpenRouterSynthesizer) SynthesizeStream(text string) (<-chan []byte, <-chan error) {
	ch := make(chan []byte, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		body := orTTSRequest{
			Input:          text,
			Model:          s.model,
			Voice:          s.voice,
			Speed:          1.0,
			ResponseFormat: "pcm",
		}

		b, _ := json.Marshal(body)
		req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/audio/speech", bytes.NewReader(b))
		if err != nil {
			errCh <- err
			return
		}
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.client.Do(req)
		if err != nil {
			errCh <- fmt.Errorf("openrouter tts: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("openrouter tts: HTTP %d: %s", resp.StatusCode, string(body))
			return
		}

		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				ch <- chunk
			}
			if err != nil {
				if err != io.EOF {
					errCh <- err
				}
				return
			}
		}
	}()

	return ch, errCh
}
