package context

import (
	"os"
	"strings"
	"testing"
)

func TestNewNoKey(t *testing.T) {
	os.Unsetenv("CONTEXT_API_KEY")
	_, err := New("")
	if err == nil {
		t.Error("expected error when CONTEXT_API_KEY is not set")
	}
}

func TestNewWithKey(t *testing.T) {
	os.Setenv("CONTEXT_API_KEY", "test-key")
	defer os.Unsetenv("CONTEXT_API_KEY")
	c, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil")
	}
}

func TestNewExplicitKey(t *testing.T) {
	os.Unsetenv("CONTEXT_API_KEY")
	c, err := New("explicit-key")
	if err != nil {
		t.Fatalf("New with explicit key: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil")
	}
}

func TestFetchURLIntegration(t *testing.T) {
	key := os.Getenv("CONTEXT_API_KEY")
	if key == "" {
		t.Skip("CONTEXT_API_KEY not set -- skipping integration test")
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := c.FetchURL("https://example.com")
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if result.Title == "" {
		t.Error("FetchURL returned empty title")
	}
	t.Logf("title: %s", result.Title)
	t.Logf("content length: %d", len(result.Content))
}

func TestSearchIntegration(t *testing.T) {
	key := os.Getenv("CONTEXT_API_KEY")
	if key == "" {
		t.Skip("CONTEXT_API_KEY not set -- skipping integration test")
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	results, err := c.Search("latest AI news", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Error("Search returned no results")
	}
	for i, r := range results {
		t.Logf("  %d. %s (%s)", i+1, r.Title, r.URL)
	}
}

func TestFetchAndSummarizeIntegration(t *testing.T) {
	key := os.Getenv("CONTEXT_API_KEY")
	if key == "" {
		t.Skip("CONTEXT_API_KEY not set -- skipping integration test")
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	summary, err := c.FetchAndSummarize("https://example.com", 500)
	if err != nil {
		t.Fatalf("FetchAndSummarize: %v", err)
	}
	if summary == "" {
		t.Error("FetchAndSummarize returned empty summary")
	}
	t.Logf("summary: %s", summary)
}

func TestLLMContext(t *testing.T) {
	key := os.Getenv("CONTEXT_API_KEY")
	if key == "" {
		t.Skip("CONTEXT_API_KEY not set -- skipping integration test")
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// URL input.
	ctx, err := c.LLMContext("https://example.com", 500)
	if err != nil {
		t.Fatalf("LLMContext URL: %v", err)
	}
	if !strings.Contains(ctx, "Example") {
		t.Logf("LLMContext URL output: %s", ctx)
	}

	// Search query input.
	ctx, err = c.LLMContext("weather today", 500)
	if err != nil {
		t.Fatalf("LLMContext search: %v", err)
	}
	if ctx == "" {
		t.Error("LLMContext search returned empty")
	}
	t.Logf("LLMContext search: %s", ctx)
}
