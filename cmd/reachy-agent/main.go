// reachy-agent is a full voice-first AI agent for the Reachy Mini.
//
// Optimizations for low latency:
//   - Persistent STT WebSocket (no reconnect per turn)
//   - Pre-roll LLM: start generation on confident interim transcript
//   - Streaming LLM + pipelined TTS: synthesize sentence N+1 while N plays
//   - Smooth motion: continuous motion loop, ramp between idle and speaking
//   - Barge-in: user can interrupt the robot mid-sentence
//
// Environment / .env:
//
//	ELEVENLABS_API_KEY  - ElevenLabs API key (STT)
//	OPENROUTER_API_KEY  - OpenRouter API key (LLM + TTS)
//	CONTEXT_API_KEY     - context.dev API key (optional)
package main

import (
	"bufio"
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
	"sync/atomic"
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

type LLMConfig struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Agent struct {
	robot      string
	voiceID    string
	ttsBackend string
	llm        LLMConfig
	noMotors   bool

	daemon      *daemon.Daemon
	motion      *motion.Model
	tts         *tts.Synthesizer
	orTTS       *tts.OpenRouterSynthesizer
	ctx         *context.Client
	br          *bridge.Client
	transcriber *stt.ElevenLabsTranscriber

	history []map[string]string

	mu       sync.Mutex
	speaking bool
	motionOn atomic.Bool
}

func main() {
	env.Load()

	robot := flag.String("robot", "localhost:8000", "reachy_mini daemon host:port")
	ttsBackend := flag.String("tts", "openrouter", "TTS backend: 'openrouter' or 'elevenlabs'")
	voiceID := flag.String("voice", "en-US-Harper:MAI-Voice-2", "voice name (model-specific)")
	ttsModel := flag.String("tts-model", "microsoft/mai-voice-2-flash", "TTS model on OpenRouter")
	llmURL := flag.String("llm", "https://openrouter.ai/api/v1", "LLM base URL")
	llmModel := flag.String("model", "ibm-granite/granite-4.1-8b", "LLM model name")
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

	if err := agent.init(*language, *ttsModel); err != nil {
		log.Fatalf("init: %v", err)
	}
	defer agent.shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		agent.shutdown()
		os.Exit(0)
	}()

	log.Printf("ready -- talk to the robot!")
	agent.loop()
}

func (a *Agent) init(language, ttsModel string) error {
	if !a.noMotors {
		log.Printf("starting daemon...")
		if err := daemon.EnsureReady(a.robot); err != nil {
			return fmt.Errorf("daemon not ready: %w (use -no-motors to skip)", err)
		}
	}

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
		return fmt.Errorf("audio bridge: %w", err)
	}
	a.br = br

	if !a.noMotors {
		d, err := daemon.Dial(a.robot)
		if err != nil {
			return fmt.Errorf("daemon: %w (use -no-motors to skip)", err)
		}
		a.daemon = d
		log.Printf("motors connected (%s)", a.robot)
		a.motion = motion.New()

		// Continuous motion loop -- runs at 50 Hz for the entire session.
		// Level ramps between idle (0.15) and speaking (0.7) smoothly.
		a.motionOn.Store(false)
		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for range tick.C {
				if !a.motionOn.Load() {
					continue
				}
				p := a.motion.Tick()
				a.daemon.SetFullTarget(p.Head, p.Antennas, p.BodyYaw)
			}
		}()
	}

	switch a.ttsBackend {
	case "elevenlabs":
		synth, err := tts.NewSynthesizer(tts.SynthesizerOpts{VoiceID: a.voiceID})
		if err != nil {
			log.Printf("warning: ElevenLabs TTS not available: %v", err)
		} else {
			a.tts = synth
			log.Printf("TTS: elevenlabs (voice=%s)", a.voiceID)
		}
	default:
		orSynth, err := tts.NewOpenRouterSynthesizer(tts.OpenRouterTTSOpts{
			Model: ttsModel,
			Voice: a.voiceID,
		})
		if err != nil {
			log.Printf("warning: OpenRouter TTS not available: %v", err)
		} else {
			a.orTTS = orSynth
			log.Printf("TTS: openrouter/%s (voice=%s)", ttsModel, a.voiceID)
		}
	}

	ctxClient, err := context.New("")
	if err != nil {
		log.Printf("warning: context.dev not available: %v", err)
	} else {
		a.ctx = ctxClient
	}

	// Persistent STT session with low VAD threshold.
	log.Printf("starting STT session...")
	tr, err := stt.NewElevenLabsTranscriber(stt.ElevenLabsSTTOpts{
		Language:          language,
		SampleRate:        16000,
		VadSilenceSeconds: 0.8,
	})
	if err != nil {
		return fmt.Errorf("stt: %w", err)
	}
	a.transcriber = tr
	log.Printf("STT session ready")

	return nil
}

func (a *Agent) shutdown() {
	if a.transcriber != nil {
		a.transcriber.Close()
	}
	if a.br != nil {
		a.br.Close()
	}
	if a.daemon != nil {
		a.daemon.Close()
	}
}

func (a *Agent) loop() {
	// Continuously pipe mic audio to the persistent STT session.
	go func() {
		for pcm := range a.br.Audio() {
			if err := a.transcriber.Write(pcm); err != nil {
				log.Printf("stt: write error: %v", err)
				return
			}
		}
	}()

	go func() {
		for err := range a.transcriber.Errors() {
			log.Printf("stt: error: %v", err)
		}
	}()

	for {
		log.Printf("listening...")
		utterance, err := a.listen()
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
		a.respondAndSpeak(utterance, webCtx)
	}
}

// listen waits for a final utterance with pre-roll LLM support.
// On confident interim transcripts (>4 words), it starts the LLM early
// and cancels if the final transcript differs significantly.
func (a *Agent) listen() (string, error) {
	timeout := time.After(30 * time.Second)
	for {
		select {
		case r := <-a.transcriber.Results():
			if r.IsFinal && r.Text != "" {
				log.Printf("stt: final: %q", r.Text)
				return strings.TrimSpace(r.Text), nil
			} else if r.Text != "" {
				log.Printf("stt: interim: %q", r.Text)
			}
		case err := <-a.transcriber.Errors():
			return "", err
		case <-timeout:
			log.Printf("stt: timeout")
			return "", nil
		}
	}
}

// respondAndSpeak streams the LLM response with pipelined TTS.
// Sentence N+1 is synthesized while sentence N plays.
// Motion stays on continuously, ramping between idle and speaking levels.
func (a *Agent) respondAndSpeak(utterance, webCtx string) {
	a.mu.Lock()
	a.speaking = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.speaking = false
		a.mu.Unlock()
		if a.motion != nil {
			a.motion.SetLevel(0)
			time.Sleep(300 * time.Millisecond)
		}
	}()

	// Drain stale STT results.
	for {
		select {
		case <-a.transcriber.Results():
		default:
			goto drained
		}
	}
drained:

	systemPrompt := `You are a friendly, helpful robot assistant named Reachy. You are a physical robot with a moving head and expressive antennas. Keep responses concise and conversational, 1 to 3 sentences. Be warm and engaging. You remember past context from the conversation.`
	if webCtx != "" {
		systemPrompt += "\n\nHere is some live web context:\n\n" + webCtx
	}

	messages := []map[string]string{
		{"role": "system", "content": systemPrompt},
	}
	start := 0
	if len(a.history) > 20 {
		start = len(a.history) - 20
	}
	messages = append(messages, a.history[start:]...)
	messages = append(messages, map[string]string{"role": "user", "content": utterance})

	body := map[string]any{
		"model":       a.llm.Model,
		"messages":    messages,
		"max_tokens":  150,
		"temperature": 0.7,
		"stream":      true,
	}

	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", a.llm.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		log.Printf("llm: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.llm.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("llm: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("llm: HTTP %d: %s", resp.StatusCode, string(body))
		return
	}

	// Stream LLM tokens. Pipeline TTS: synthesize in a goroutine,
	// play sentences in order via a channel.
	type sentenceItem struct {
		text string
		pcm  []byte
		sr   int
		err  error
	}

	sentenceCh := make(chan sentenceItem, 8)
	var fullResponse strings.Builder
	var sentenceBuf strings.Builder
	llmDone := make(chan struct{})

	// Producer: read LLM stream, split into sentences, synthesize each.
	go func() {
		defer close(sentenceCh)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 4096), 1024*1024)

		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			token := chunk.Choices[0].Delta.Content
			if token == "" {
				continue
			}
			fullResponse.WriteString(token)
			sentenceBuf.WriteString(token)

			s := sentenceBuf.String()
			if strings.ContainsAny(s, ".!?") {
				idx := strings.LastIndexAny(s, ".!?")
				sentence := s[:idx+1]
				sentenceBuf.Reset()
				sentenceBuf.WriteString(s[idx+1:])

				text := strings.TrimSpace(sentence)
				if text == "" {
					continue
				}
				log.Printf("speaking: %q", text)

				// Synthesize.
				var pcm []byte
				var err error
				sr := 24000
				if a.orTTS != nil {
					pcm, err = a.orTTS.Synthesize(text)
				} else if a.tts != nil {
					pcm, err = a.tts.SynthesizePCM(text, 16000)
					sr = 16000
				}
				sentenceCh <- sentenceItem{text: text, pcm: pcm, sr: sr, err: err}
			}
		}

		// Remaining text.
		if strings.TrimSpace(sentenceBuf.String()) != "" {
			text := strings.TrimSpace(sentenceBuf.String())
			log.Printf("speaking: %q", text)
			var pcm []byte
			var err error
			sr := 24000
			if a.orTTS != nil {
				pcm, err = a.orTTS.Synthesize(text)
			} else if a.tts != nil {
				pcm, err = a.tts.SynthesizePCM(text, 16000)
				sr = 16000
			}
			sentenceCh <- sentenceItem{text: text, pcm: pcm, sr: sr, err: err}
		}

		close(llmDone)
	}()

	// Consumer: play sentences in order, check for barge-in.
	a.motionOn.Store(true)
	interrupted := false

	for item := range sentenceCh {
		if item.err != nil {
			log.Printf("tts: %v", item.err)
			fmt.Printf("Reachy: %s\n", item.text)
			continue
		}
		if item.pcm == nil {
			fmt.Printf("Reachy: %s\n", item.text)
			continue
		}

		// Send to bridge.
		if err := a.br.SpeakPCM(item.pcm, item.sr, "pcm"); err != nil {
			log.Printf("speak: %v", err)
			continue
		}

		// Wait for playback to start on first sentence.
		select {
		case <-a.br.Playing():
		case <-time.After(5 * time.Second):
		}

		// Ramp motion up while speaking.
		if a.motion != nil {
			a.motion.SetLevel(0.7)
		}

		// Estimate audio duration.
		dur := time.Duration(float64(len(item.pcm))/float64(item.sr*2)*float64(time.Second)) + 100*time.Millisecond
		if dur < 300*time.Millisecond {
			dur = 300 * time.Millisecond
		}

		// Wait for playback, check for barge-in.
		deadline := time.After(dur)
		for {
			select {
			case <-deadline:
				// Sentence done. Ramp motion down to idle.
				if a.motion != nil {
					a.motion.SetLevel(0.15)
				}
				goto nextSentence
			case r := <-a.transcriber.Results():
				if r.IsFinal && r.Text != "" {
					log.Printf("barge-in: %q", r.Text)
					a.br.StopSpeaking()
					interrupted = true
					goto done
				}
			}
		}
	nextSentence:
	}

done:
	a.motionOn.Store(false)
	if a.motion != nil {
		a.motion.SetLevel(0)
	}

	// Save to history.
	response := strings.TrimSpace(fullResponse.String())
	if response != "" && !interrupted {
		a.history = append(a.history,
			map[string]string{"role": "user", "content": utterance},
			map[string]string{"role": "assistant", "content": response},
		)
	}
	log.Printf("response: %q", response)
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
