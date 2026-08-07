package stt

import (
	"os"
	"testing"
)

func TestNewTranscriberNoKey(t *testing.T) {
	os.Unsetenv("DEEPGRAM_API_KEY")
	_, err := NewTranscriber(TranscriberOpts{})
	if err == nil {
		t.Error("expected error when DEEPGRAM_API_KEY is not set")
	}
}

func TestNewTranscriberWithKey(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "test-key")
	defer os.Unsetenv("DEEPGRAM_API_KEY")
	// With a fake key, the WebSocket dial will fail. That's expected --
	// we're testing config validation, not connectivity.
	_, err := NewTranscriber(TranscriberOpts{})
	if err == nil {
		t.Error("expected dial error with fake API key")
	}
	t.Logf("got expected error: %v", err)
}

func TestNewTranscriberCustomOpts(t *testing.T) {
	os.Setenv("DEEPGRAM_API_KEY", "test-key")
	defer os.Unsetenv("DEEPGRAM_API_KEY")
	_, err := NewTranscriber(TranscriberOpts{
		Language:       "fr",
		Model:          "nova-2",
		InterimResults: true,
		Endpointing:    300,
	})
	if err == nil {
		t.Error("expected dial error with fake API key")
	}
}

func TestNewTranscriberAPIKeyParam(t *testing.T) {
	os.Unsetenv("DEEPGRAM_API_KEY")
	_, err := NewTranscriber(TranscriberOpts{APIKey: "explicit-fake-key"})
	if err == nil {
		t.Error("expected dial error with fake API key")
	}
}
