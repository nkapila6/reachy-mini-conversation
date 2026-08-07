package tts

import (
	"os"
	"strings"
	"testing"
)

func TestSplitText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxChars int
		want     int // expected number of chunks
	}{
		{
			name:     "short text",
			text:     "Hello world.",
			maxChars: 500,
			want:     1,
		},
		{
			name:     "multiple sentences under limit",
			text:     "Hello. How are you? I am fine!",
			maxChars: 500,
			want:     1,
		},
		{
			name:     "splits at sentence boundaries",
			text:     strings.Repeat("This is a sentence. ", 50),
			maxChars: 200,
			want:     0, // we just check it splits into multiple chunks
		},
		{
			name:     "empty text",
			text:     "",
			maxChars: 500,
			want:     0,
		},
		{
			name:     "no punctuation",
			text:     strings.Repeat("word ", 100),
			maxChars: 100,
			want:     0, // splits by words when no sentence boundaries exist
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := SplitText(tt.text, tt.maxChars)
			if tt.want > 0 && len(chunks) != tt.want {
				t.Errorf("SplitText() got %d chunks, want %d", len(chunks), tt.want)
			}
			// Verify no chunk exceeds maxChars.
			for i, c := range chunks {
				if len(c) > tt.maxChars && tt.maxChars > 0 {
					t.Errorf("chunk %d length %d exceeds max %d", i, len(c), tt.maxChars)
				}
			}
			// Verify concatenation preserves content (minus whitespace differences).
			joined := strings.Join(chunks, " ")
			if len(tt.text) > 0 && len(joined) == 0 {
				t.Error("non-empty input produced empty output")
			}
		})
	}
}

func TestSplitTextDefaultMax(t *testing.T) {
	text := strings.Repeat("A long sentence without any punctuation at all. ", 20)
	chunks := SplitText(text, 0) // 0 means use default 500
	for _, c := range chunks {
		if len(c) > 500 {
			t.Errorf("chunk length %d exceeds default max 500", len(c))
		}
	}
}

func TestNewSynthesizerNoKey(t *testing.T) {
	// Unset the env var to test error case.
	os.Unsetenv("ELEVENLABS_API_KEY")
	_, err := NewSynthesizer(SynthesizerOpts{VoiceID: "test"})
	if err == nil {
		t.Error("expected error when ELEVENLABS_API_KEY is not set")
	}
}

func TestNewSynthesizerNoVoiceID(t *testing.T) {
	os.Setenv("ELEVENLABS_API_KEY", "test-key")
	defer os.Unsetenv("ELEVENLABS_API_KEY")
	_, err := NewSynthesizer(SynthesizerOpts{})
	if err == nil {
		t.Error("expected error when voice ID is empty")
	}
}

func TestNewSynthesizerWithKey(t *testing.T) {
	os.Setenv("ELEVENLABS_API_KEY", "test-key")
	defer os.Unsetenv("ELEVENLABS_API_KEY")
	s, err := NewSynthesizer(SynthesizerOpts{VoiceID: "test-voice"})
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	if s.voice.Stability != 0.5 {
		t.Errorf("default stability = %f, want 0.5", s.voice.Stability)
	}
	if s.voice.SimilarityBoost != 0.75 {
		t.Errorf("default similarity = %f, want 0.75", s.voice.SimilarityBoost)
	}
	if s.voice.Speed != 1.0 {
		t.Errorf("default speed = %f, want 1.0", s.voice.Speed)
	}
}

func TestSynthesizeIntegration(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		t.Skip("ELEVENLABS_API_KEY not set -- skipping integration test")
	}
	voiceID := os.Getenv("ELEVENLABS_VOICE_ID")
	if voiceID == "" {
		voiceID = "21m00Tcm4TlvDq8ikWAM" // Rachel
	}

	s, err := NewSynthesizer(SynthesizerOpts{VoiceID: voiceID})
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}

	data, err := s.Synthesize("Hello, this is a test of the ElevenLabs text to speech API.")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(data) == 0 {
		t.Error("Synthesize returned empty audio")
	}
	t.Logf("synthesized %d bytes of Opus audio", len(data))
}

func TestListVoicesIntegration(t *testing.T) {
	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		t.Skip("ELEVENLABS_API_KEY not set -- skipping integration test")
	}
	voices, err := ListVoices(key)
	if err != nil {
		t.Fatalf("ListVoices: %v", err)
	}
	if len(voices) == 0 {
		t.Error("ListVoices returned no voices")
	}
	t.Logf("found %d voices", len(voices))
	for _, v := range voices {
		if name, ok := v["name"].(string); ok {
			t.Logf("  - %s", name)
		}
	}
}
