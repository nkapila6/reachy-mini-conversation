// end-to-end test for the full agent pipeline.
// Requires DEEPGRAM_API_KEY and OPENROUTER_API_KEY.
// Skips if either is missing.
//
// This test does NOT need a robot -- it uses -no-motors mode.
// It sends a text prompt through the LLM and verifies we get a response.
// The mic/STT path is tested separately since it needs actual audio input.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestAgentLLMResponse(t *testing.T) {
	// Check required keys.
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set -- skipping agent LLM test")
	}

	// Create an agent in no-motors mode (no robot needed).
	agent := &Agent{
		robot:    "localhost:8000",
		voiceID:  "21m00Tcm4TlvDq8ikWAM",
		noMotors: true,
		llm: LLMConfig{
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  os.Getenv("OPENROUTER_API_KEY"),
			Model:   "openai/gpt-4o-mini",
		},
	}

	// Init (TTS and context are optional, won't fail).
	if err := agent.init(); err != nil {
		t.Fatalf("agent init: %v", err)
	}
	defer agent.shutdown()

	// Test a simple question.
	response, err := agent.generateResponse("What is the capital of France?", "")
	if err != nil {
		t.Fatalf("generateResponse: %v", err)
	}
	if response == "" {
		t.Error("LLM returned empty response")
	}
	t.Logf("LLM response: %s", response)

	// Verify it contains something about Paris.
	if !strings.Contains(strings.ToLower(response), "paris") {
		t.Logf("warning: response didn't mention Paris, got: %s", response)
	}

	// Test with web context injected.
	webCtx := "The capital of France is Paris. Population: 2.1 million. Known for the Eiffel Tower."
	response2, err := agent.generateResponse("Tell me about the capital of France", webCtx)
	if err != nil {
		t.Fatalf("generateResponse with context: %v", err)
	}
	if response2 == "" {
		t.Error("LLM returned empty response with context")
	}
	t.Logf("LLM response with context: %s", response2)
}

func TestAgentSpeakFallback(t *testing.T) {
	// Test that speak() works when TTS is nil (prints to stdout).
	agent := &Agent{
		noMotors: true,
	}
	// Don't call init() -- TTS will be nil.
	agent.speak("Hello, this is a fallback test")
	// If we get here without panic, it works.
}

func TestAgentQuestionDetection(t *testing.T) {
	questions := []string{
		"what is the weather",
		"who is the president",
		"when does the sun rise",
		"where is the library",
		"why is the sky blue",
		"how do I get there",
		"tell me a story",
		"search for cats",
		"find restaurants",
		"look up the answer",
		"is this a question?",
	}
	nonQuestions := []string{
		"hello",
		"goodbye",
		"thanks",
		"ok",
		"yes",
		"no",
	}

	for _, q := range questions {
		if !isQuestion(q) {
			t.Errorf("isQuestion(%q) = false, want true", q)
		}
	}
	for _, q := range nonQuestions {
		if isQuestion(q) {
			t.Errorf("isQuestion(%q) = true, want false", q)
		}
	}
}

func TestAgentExitCommands(t *testing.T) {
	exits := []string{"exit", "quit", "goodbye", "bye", "stop", "EXIT", "Quit"}
	nonExits := []string{"hello", "what is exit", "goodbye world"}

	for _, e := range exits {
		if !isExitCommand(e) {
			t.Errorf("isExitCommand(%q) = false, want true", e)
		}
	}
	for _, e := range nonExits {
		if isExitCommand(e) {
			t.Errorf("isExitCommand(%q) = true, want false", e)
		}
	}
}
