// Package tts provides text-to-speech via OpenRouter's audio speech API.
//
// Uses the same OPENROUTER_API_KEY as the LLM. Supports Kokoro-82M and
// Fish Audio S1, plus any other TTS model on OpenRouter.
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
	"time"
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
	APIKey string // If empty, reads OPENROUTER_API_KEY from env.
	Model  string // TTS model (default "fish-audio/s1").
	Voice  string // Voice name (model-specific).
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
		model = "fish-audio/s1"
	}
	voice := opts.Voice

	return &OpenRouterSynthesizer{
		apiKey: apiKey,
		model:  model,
		voice:  voice,
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// orTTSRequest matches the OpenRouter TTS API spec.
// Fish Audio S1 doesn't use voice or response_format -- just input + model.
// Kokoro uses voice + response_format.
type orTTSRequest struct {
	Input          string `json:"input"`
	Model          string `json:"model"`
	Voice          string `json:"voice,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

// Synthesize converts text to audio bytes (MP3 by default).
func (s *OpenRouterSynthesizer) Synthesize(text string) ([]byte, error) {
	body := orTTSRequest{
		Input: text,
		Model: s.model,
	}
	// Kokoro needs voice + response_format. Fish Audio S1 doesn't.
	if s.voice != "" {
		body.Voice = s.voice
		body.ResponseFormat = "pcm"
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
