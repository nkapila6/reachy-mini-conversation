// reachy-agent is a full voice-first AI agent for the Reachy Mini.
//
// It uses a Python audio bridge (audio_bridge.py) that talks to the
// reachy_mini SDK for mic capture and speaker playback -- same approach
// as the official conversation app. All logic (STT, LLM, TTS, motion)
// stays in Go.
//
// Usage:
//
//	reachy-agent -robot localhost:8000
//	reachy-agent -robot 192.168.1.100:8000 -voice 21m00Tcm4TlvDq8ikWAM
//	reachy-agent -robot localhost:8000 -model openai/gpt-4o-mini
//
// Environment / .env:
//
//	ELEVENLABS_API_KEY  - ElevenLabs API key (STT + TTS)
//	OPENROUTER_API_KEY  - OpenRouter API key (LLM)
//	CONTEXT_API_KEY     - context.dev API key (optional, web lookups)
//
// Build:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o reachy-agent ./cmd/reachy-agent
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nkapila6/reachy-utils/pkg/bridge"
	"github.com/nkapila6/reachy-utils/pkg/context"
	"github.com/nkapila6/reachy-utils/pkg/daemon"
	"github.com/nkapila6/reachy-utils/pkg/env"
	"github.com/nkapila6/reachy-utils/pkg/motion"
	"github.com/nkapila6/reachy-utils/pkg/stt"
	"github.com/nkapila6/reachy-utils/pkg/tts"
)

// LLMConfig holds the LLM endpoint configuration.
type LLMConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Agent ties together all the components.
type Agent struct {
	robot      string
	voiceID    string
	ttsBackend string
	llm        LLMConfig
	noMotors   bool

	daemon *daemon.Daemon
	motion *motion.Model
	tts    *tts.Synthesizer           // ElevenLabs
	orTTS  *tts.OpenRouterSynthesizer // OpenRouter
	ctx    *context.Client
	br     *bridge.Client

	mu       sync.Mutex
	speaking bool
}

func main() {
	env.Load()

	robot := flag.String("robot", "localhost:8000", "reachy_mini daemon host:port")
	ttsBackend := flag.String("tts", "openrouter", "TTS backend: 'openrouter' or 'elevenlabs'")
	voiceID := flag.String("voice", "am_onyx", "voice: Kokoro voices (af_heart, af_bella, am_onyx, am_michael, bf_emma, hf_alpha, etc) or ElevenLabs voice ID")
	llmURL := flag.String("llm", "https://openrouter.ai/api/v1", "LLM base URL")
	llmModel := flag.String("model", "openai/gpt-4o-mini", "LLM model name")
	llmKey := flag.String("llm-key", "", "LLM API key (or set OPENROUTER_API_KEY env)")
	language := flag.String("language", "en", "BCP-47 language code for STT")
	noMotors := flag.Bool("no-motors", false, "don't drive the robot's motors")
	flag.Parse()

	apiKey := *llmKey
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}

	agent := &Agent{
		robot:      *robot,
		voiceID:    *voiceID,
		ttsBackend: *ttsBackend,
		noMotors:   *noMotors,
		llm: LLMConfig{
			BaseURL: *llmURL,
			APIKey:  apiKey,
			Model:   *llmModel,
		},
	}

	if err := agent.init(); err != nil {
		log.Fatalf("init: %v", err)
	}
	defer agent.shutdown()

	// Handle Ctrl-C: put robot to sleep before exiting.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		agent.shutdown()
		os.Exit(0)
	}()

	log.Printf("ready -- talk to the robot!")
	agent.loop(*language)
}

func (a *Agent) init() error {
	// Start the daemon backend first -- the bridge needs it running.
	if !a.noMotors {
		log.Printf("starting daemon...")
		if err := daemon.EnsureReady(a.robot); err != nil {
			return fmt.Errorf("daemon not ready: %w (use -no-motors to skip)", err)
		}
	}

	// Start the Python audio bridge -- it creates ReachyMini(), calls
	// enable_motors() + wake_up(), starts media, and applies DSP config.
	host, _, _ := splitHost(a.robot)
	var br *bridge.Client
	var err error
	for i := 0; i < 5; i++ {
		br, err = bridge.Start(host)
		if err == nil {
			break
		}
		log.Printf("bridge: attempt %d/5 failed: %v (retrying in 2s...)", i+1, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("audio bridge: %w (is audio_bridge.py in the same directory?)", err)
	}
	a.br = br

	// Connect motors (the bridge already woke the robot).
	if !a.noMotors {
		d, err := daemon.Dial(a.robot)
		if err != nil {
			return fmt.Errorf("daemon: %w (use -no-motors to skip)", err)
		}
		a.daemon = d
		log.Printf("motors connected (%s)", a.robot)

		// Motion model only runs while speaking -- not during idle/listening.
		// This prevents the antennas from swinging wildly when the robot is
		// just sitting there waiting for input.
		a.motion = motion.New()
	}

	// Initialize TTS (optional). Two backends: openrouter or elevenlabs.
	switch a.ttsBackend {
	case "elevenlabs":
		synth, err := tts.NewSynthesizer(tts.SynthesizerOpts{VoiceID: a.voiceID})
		if err != nil {
			log.Printf("warning: ElevenLabs TTS not available: %v (voice output disabled)", err)
		} else {
			a.tts = synth
			log.Printf("TTS: elevenlabs (voice=%s)", a.voiceID)
		}
	default:
		orSynth, err := tts.NewOpenRouterSynthesizer(tts.OpenRouterTTSOpts{
			Model: "hexgrad/kokoro-82m",
			Voice: a.voiceID,
		})
		if err != nil {
			log.Printf("warning: OpenRouter TTS not available: %v (voice output disabled)", err)
		} else {
			a.orTTS = orSynth
			log.Printf("TTS: openrouter/kokoro (voice=%s)", a.voiceID)
		}
	}

	// Initialize context.dev client (optional).
	ctxClient, err := context.New("")
	if err != nil {
		log.Printf("warning: context.dev not available: %v", err)
	} else {
		a.ctx = ctxClient
	}

	return nil
}

func (a *Agent) shutdown() {
	if a.br != nil {
		a.br.Close()
	}
	if a.daemon != nil {
		a.daemon.Close()
	}
}

func (a *Agent) loop(language string) {
	for {
		log.Printf("listening...")
		utterance, err := a.listen(language)
		if err != nil {
			log.Printf("listen error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if utterance == "" {
			continue
		}
		log.Printf("heard: %q", utterance)

		if isExitCommand(utterance) {
			log.Println("goodbye!")
			return
		}

		webCtx := a.fetchContext(utterance)
		response, err := a.generateResponse(utterance, webCtx)
		if err != nil {
			log.Printf("llm error: %v", err)
			a.speak("Sorry, I had trouble thinking of a response.")
			continue
		}
		log.Printf("response: %q", response)
		a.speak(response)
	}
}

// listen captures audio from the bridge and transcribes via ElevenLabs STT.
func (a *Agent) listen(language string) (string, error) {
	transcriber, err := stt.NewElevenLabsTranscriber(stt.ElevenLabsSTTOpts{
		Language:   language,
		SampleRate: 16000,
	})
	if err != nil {
		return "", fmt.Errorf("stt: %w", err)
	}
	defer transcriber.Close()

	// Pipe bridge audio (PCM int16) to ElevenLabs STT.
	packetCount := 0
	go func() {
		for pcm := range a.br.Audio() {
			packetCount++
			if packetCount == 1 {
				log.Printf("stt: first audio chunk received (%d bytes)", len(pcm))
			}
			if err := transcriber.Write(pcm); err != nil {
				log.Printf("stt: write error: %v", err)
				return
			}
		}
	}()

	// Log errors.
	go func() {
		for err := range transcriber.Errors() {
			log.Printf("stt: error: %v", err)
		}
	}()

	// Wait for a final utterance or timeout.
	// Single reader on Results() -- no separate logging goroutine
	// to avoid stealing results from this select.
	timeout := time.After(15 * time.Second)
	for {
		select {
		case r := <-transcriber.Results():
			if r.IsFinal && r.Text != "" {
				log.Printf("stt: final: %q (after %d chunks)", r.Text, packetCount)
				return strings.TrimSpace(r.Text), nil
			} else if r.Text != "" {
				log.Printf("stt: interim: %q", r.Text)
			}
		case err := <-transcriber.Errors():
			return "", err
		case <-timeout:
			log.Printf("stt: timeout after %d chunks, no final utterance", packetCount)
			return "", nil
		}
	}
}

// speak synthesizes text and plays it through the robot's speaker via the bridge.
func (a *Agent) speak(text string) {
	a.mu.Lock()
	a.speaking = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.speaking = false
		a.mu.Unlock()
		if a.motion != nil {
			a.motion.SetLevel(0)
			time.Sleep(500 * time.Millisecond)
		}
	}()

	// Synthesize via whichever TTS backend is available.
	var pcm []byte
	var err error
	if a.orTTS != nil {
		pcm, err = a.orTTS.Synthesize(text)
		if err != nil {
			log.Printf("tts: %v", err)
			fmt.Printf("Reachy: %s\n", text)
			return
		}
		if err := a.br.SpeakPCM(pcm, 24000); err != nil {
			log.Printf("speak: %v", err)
		}
	} else if a.tts != nil {
		pcm, err = a.tts.SynthesizePCM(text, 16000)
		if err != nil {
			log.Printf("tts: %v", err)
			fmt.Printf("Reachy: %s\n", text)
			return
		}
		if err := a.br.SpeakPCM(pcm, 16000); err != nil {
			log.Printf("speak: %v", err)
		}
	} else {
		fmt.Printf("Reachy: %s\n", text)
		return
	}

	// Wait for the bridge to start playing, then start motion in sync.
	if a.motion != nil && a.daemon != nil {
		// Wait for the "playing" signal (up to 10s for TTS synthesis).
		select {
		case <-a.br.Playing():
			a.motion.SetLevel(0.7)
		case <-time.After(10 * time.Second):
			log.Printf("motion: timed out waiting for audio playback")
			return
		}

		// Run motion ticker while audio plays.
		done := make(chan struct{})
		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					p := a.motion.Tick()
					a.daemon.SetFullTarget(p.Head, p.Antennas, p.BodyYaw)
				case <-done:
					return
				}
			}
		}()

		// Estimate audio duration: PCM int16 mono at 16kHz = 32000 bytes/sec.
		// After resampling from 24kHz, the duration is roughly len(pcm) / 48000.
		// Add 500ms buffer for the speaker to finish.
		audioDuration := time.Duration(float64(len(pcm))/48000.0*float64(time.Second)) + 500*time.Millisecond
		if audioDuration < 2*time.Second {
			audioDuration = 2 * time.Second
		}
		time.Sleep(audioDuration)
		close(done)
	}
}

func (a *Agent) fetchContext(utterance string) string {
	if a.ctx == nil {
		return ""
	}
	words := strings.Fields(utterance)
	for _, w := range words {
		if strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://") {
			summary, err := a.ctx.FetchAndSummarize(w, 2000)
			if err != nil {
				return ""
			}
			return summary
		}
	}
	if isQuestion(utterance) {
		summary, err := a.ctx.SearchAndSummarize(utterance, 3)
		if err != nil {
			return ""
		}
		return summary
	}
	return ""
}

func (a *Agent) generateResponse(utterance, webCtx string) (string, error) {
	systemPrompt := `You are a friendly, helpful robot assistant named Reachy. You are a physical robot with a moving head and expressive antennas. Keep responses concise and conversational -- 1 to 3 sentences. Be warm and engaging.`
	if webCtx != "" {
		systemPrompt += "\n\nHere is some live web context that may help answer the user's question:\n\n" + webCtx
	}

	messages := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": utterance},
	}

	body := map[string]any{
		"model":       a.llm.Model,
		"messages":    messages,
		"max_tokens":  150,
		"temperature": 0.7,
	}

	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", a.llm.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.llm.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.llm.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("llm parse: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func splitHost(addr string) (string, string, string) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], ""
		}
	}
	return addr, "", ""
}

func isQuestion(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "what") ||
		strings.HasPrefix(s, "who") ||
		strings.HasPrefix(s, "when") ||
		strings.HasPrefix(s, "where") ||
		strings.HasPrefix(s, "why") ||
		strings.HasPrefix(s, "how") ||
		strings.HasPrefix(s, "tell me") ||
		strings.HasPrefix(s, "search") ||
		strings.HasPrefix(s, "find") ||
		strings.HasPrefix(s, "look up") ||
		strings.Contains(s, "?")
}

func isExitCommand(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "exit" || s == "quit" || s == "goodbye" || s == "bye" || s == "stop"
}
