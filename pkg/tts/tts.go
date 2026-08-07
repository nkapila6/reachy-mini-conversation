// Package tts provides text-to-speech via the ElevenLabs API.
//
// It converts text to Opus-encoded audio using ElevenLabs' voice synthesis.
// Supports streaming (chunked) and non-streaming modes. The output is raw
// Opus audio at 48 kHz mono, ready to pipe into the Reachy daemon's
// webrtcsink or a local audio player.
package tts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Voice configures the ElevenLabs voice.
type Voice struct {
	// ID is the ElevenLabs voice ID (e.g. "21m00Tcm4TlvDq8ikWAM" for Rachel).
	ID string
	// Stability controls voice stability (0..1, default 0.5).
	Stability float64
	// SimilarityBoost controls how closely to match the original voice (0..1, default 0.75).
	SimilarityBoost float64
	// Style controls speaking style exaggeration (0..1, default 0).
	Style float64
	// Speed controls speaking rate (0.7..1.2, default 1.0).
	Speed float64
}

// Synthesizer converts text to speech using ElevenLabs.
type Synthesizer struct {
	apiKey string
	voice  Voice
	client *http.Client
}

// SynthesizerOpts configures the ElevenLabs synthesizer.
type SynthesizerOpts struct {
	// APIKey is the ElevenLabs API key. If empty, reads ELEVENLABS_API_KEY from env.
	APIKey string
	// VoiceID is the ElevenLabs voice ID.
	VoiceID string
	// Stability, SimilarityBoost, Style, Speed are voice settings.
	Stability       float64
	SimilarityBoost float64
	Style           float64
	Speed           float64
}

// NewSynthesizer creates a new ElevenLabs TTS synthesizer.
func NewSynthesizer(opts SynthesizerOpts) (*Synthesizer, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("ELEVENLABS_API_KEY not set")
	}
	if opts.VoiceID == "" {
		return nil, fmt.Errorf("voice ID is required")
	}

	v := Voice{
		ID:              opts.VoiceID,
		Stability:       opts.Stability,
		SimilarityBoost: opts.SimilarityBoost,
		Style:           opts.Style,
		Speed:           opts.Speed,
	}
	// Set defaults.
	if v.Stability == 0 {
		v.Stability = 0.5
	}
	if v.SimilarityBoost == 0 {
		v.SimilarityBoost = 0.75
	}
	if v.Speed == 0 {
		v.Speed = 1.0
	}

	return &Synthesizer{
		apiKey: apiKey,
		voice:  v,
		client: &http.Client{},
	}, nil
}

// ttsRequest is the JSON body for the TTS endpoint.
type ttsRequest struct {
	Text                     string        `json:"text"`
	ModelID                  string        `json:"model_id"`
	VoiceSettings            voiceSettings `json:"voice_settings"`
	OptimizeStreamingLatency int           `json:"optimize_streaming_latency,omitempty"`
}

type voiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style"`
	Speed           float64 `json:"speed"`
}

// Synthesize converts text to Opus audio bytes (non-streaming).
// Returns the complete audio as raw Opus bytes.
func (s *Synthesizer) Synthesize(text string) ([]byte, error) {
	return s.synthesize(text, "opus_48000")
}

// SynthesizePCM converts text to PCM int16 audio bytes at the given sample rate.
// Returns raw PCM s16le bytes suitable for the audio bridge.
func (s *Synthesizer) SynthesizePCM(text string, sampleRate int) ([]byte, error) {
	format := fmt.Sprintf("pcm_%d", sampleRate)
	return s.synthesize(text, format)
}

func (s *Synthesizer) synthesize(text, outputFormat string) ([]byte, error) {
	body := ttsRequest{
		Text:    text,
		ModelID: "eleven_flash_v2_5",
		VoiceSettings: voiceSettings{
			Stability:       s.voice.Stability,
			SimilarityBoost: s.voice.SimilarityBoost,
			Style:           s.voice.Style,
			Speed:           s.voice.Speed,
		},
		OptimizeStreamingLatency: 0,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal tts request: %w", err)
	}

	// output_format is a query parameter per the API reference.
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s?output_format=%s", s.voice.ID, outputFormat)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elevenlabs: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// SynthesizeStream converts text to speech and streams the audio chunks
// as they are generated. Each chunk is raw Opus audio.
func (s *Synthesizer) SynthesizeStream(text string) (<-chan []byte, <-chan error) {
	ch := make(chan []byte, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		body := ttsRequest{
			Text:    text,
			ModelID: "eleven_flash_v2_5",
			VoiceSettings: voiceSettings{
				Stability:       s.voice.Stability,
				SimilarityBoost: s.voice.SimilarityBoost,
				Style:           s.voice.Style,
				Speed:           s.voice.Speed,
			},
			OptimizeStreamingLatency: 3, // Max optimization for streaming.
		}

		b, err := json.Marshal(body)
		if err != nil {
			errCh <- fmt.Errorf("marshal tts request: %w", err)
			return
		}

		url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s/stream?output_format=opus_48000", s.voice.ID)
		req, err := http.NewRequest("POST", url, bytes.NewReader(b))
		if err != nil {
			errCh <- err
			return
		}
		req.Header.Set("xi-api-key", s.apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "audio/opus")

		resp, err := s.client.Do(req)
		if err != nil {
			errCh <- fmt.Errorf("elevenlabs stream: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("elevenlabs stream: HTTP %d: %s", resp.StatusCode, string(body))
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

// SynthesizeToWriter streams TTS audio directly to an io.Writer.
// Useful for piping into ffplay or the daemon's audio sink.
func (s *Synthesizer) SynthesizeToWriter(text string, w io.Writer) error {
	ch, errCh := s.SynthesizeStream(text)
	for chunk := range ch {
		if _, err := w.Write(chunk); err != nil {
			return err
		}
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// ListVoices returns available ElevenLabs voices.
func ListVoices(apiKey string) ([]map[string]any, error) {
	if apiKey == "" {
		apiKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	req, _ := http.NewRequest("GET", "https://api.elevenlabs.io/v1/voices", nil)
	req.Header.Set("xi-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Voices []map[string]any `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Voices, nil
}

// SplitText splits long text into sentence-sized chunks for streaming TTS.
// ElevenLabs has a ~5000 char limit per request, but smaller chunks give
// lower latency for a conversational agent.
// Splits on sentence boundaries first, then falls back to maxChars if a
// single sentence exceeds the limit.
func SplitText(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = 500
	}
	var chunks []string
	sentences := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})

	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(buf.String()))
			buf.Reset()
		}
	}
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if buf.Len()+len(s)+1 > maxChars && buf.Len() > 0 {
			flush()
		}
		// If a single sentence exceeds maxChars, split it by words.
		if len(s) > maxChars && buf.Len() == 0 {
			words := strings.Fields(s)
			var wbuf strings.Builder
			for _, w := range words {
				if wbuf.Len()+len(w)+1 > maxChars && wbuf.Len() > 0 {
					chunks = append(chunks, strings.TrimSpace(wbuf.String()))
					wbuf.Reset()
				}
				if wbuf.Len() > 0 {
					wbuf.WriteString(" ")
				}
				wbuf.WriteString(w)
			}
			if wbuf.Len() > 0 {
				chunks = append(chunks, strings.TrimSpace(wbuf.String()))
			}
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(s)
	}
	flush()
	return chunks
}
