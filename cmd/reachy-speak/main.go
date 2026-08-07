// reachy-speak converts text to speech and plays it through the
// Reachy Mini's speaker. The robot's head and antennas move in
// sync with the speech.
//
// Uses the Python audio bridge for speaker playback.
// Supports two TTS backends: openrouter (Kokoro) and elevenlabs.
//
// Usage:
//
//	reachy-speak -text "Hello, I am a robot"
//	reachy-speak -text "Bonjour" -tts openrouter -voice af_heart
//	echo "Hello world" | reachy-speak -tts openrouter -voice am_onyx
package main

import (
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nkapila6/reachy-utils/pkg/bridge"
	"github.com/nkapila6/reachy-utils/pkg/daemon"
	"github.com/nkapila6/reachy-utils/pkg/env"
	"github.com/nkapila6/reachy-utils/pkg/motion"
	"github.com/nkapila6/reachy-utils/pkg/tts"
)

func main() {
	env.Load()

	text := flag.String("text", "", "text to speak (reads from stdin if empty)")
	ttsBackend := flag.String("tts", "openrouter", "TTS backend: 'openrouter' or 'elevenlabs'")
	voice := flag.String("voice", "am_onyx", "voice name (Kokoro) or voice ID (ElevenLabs)")
	robot := flag.String("robot", "localhost:8000", "reachy_mini daemon host:port")
	noMotors := flag.Bool("no-motors", false, "don't drive the robot's motors")
	flag.Parse()

	// Read text from flag or stdin.
	input := strings.TrimSpace(*text)
	if input == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("read stdin: %v", err)
		}
		input = strings.TrimSpace(string(data))
	}
	if input == "" {
		log.Fatal("no text provided (use -text or pipe to stdin)")
	}

	// Start the daemon.
	if !*noMotors {
		log.Printf("starting daemon...")
		if err := daemon.EnsureReady(*robot); err != nil {
			log.Fatalf("daemon not ready: %v (use -no-motors to skip)", err)
		}
	}

	// Start the audio bridge.
	host, _, _ := splitHost(*robot)
	br, err := bridge.Start(host)
	if err != nil {
		log.Fatalf("audio bridge: %v", err)
	}
	defer br.Close()

	// Connect motors.
	var d *daemon.Daemon
	var mot *motion.Model
	if !*noMotors {
		d, err = daemon.Dial(*robot)
		if err != nil {
			log.Fatalf("daemon: %v (use -no-motors to skip)", err)
		}
		defer d.Close()
		mot = motion.New()
	}

	// Initialize TTS.
	var orTTS *tts.OpenRouterSynthesizer
	var elTTS *tts.Synthesizer
	switch *ttsBackend {
	case "elevenlabs":
		elTTS, err = tts.NewSynthesizer(tts.SynthesizerOpts{VoiceID: *voice})
		if err != nil {
			log.Fatalf("elevenlabs tts: %v", err)
		}
	default:
		orTTS, err = tts.NewOpenRouterSynthesizer(tts.OpenRouterTTSOpts{
			Model: "hexgrad/kokoro-82m",
			Voice: *voice,
		})
		if err != nil {
			log.Fatalf("openrouter tts: %v", err)
		}
	}

	// Synthesize.
	log.Printf("synthesizing: %q", input)
	var pcm []byte
	var sampleRate int
	if orTTS != nil {
		pcm, err = orTTS.Synthesize(input)
		sampleRate = 24000
	} else {
		pcm, err = elTTS.SynthesizePCM(input, 16000)
		sampleRate = 16000
	}
	if err != nil {
		log.Fatalf("tts: %v", err)
	}

	// Send to bridge for playback.
	if err := br.SpeakPCM(pcm, sampleRate); err != nil {
		log.Fatalf("speak: %v", err)
	}

	// Wait for playback to start, then run motion.
	if mot != nil && d != nil {
		select {
		case <-br.Playing():
			mot.SetLevel(0.7)
		case <-time.After(10 * time.Second):
			log.Printf("motion: timed out waiting for playback")
			return
		}

		done := make(chan struct{})
		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					p := mot.Tick()
					d.SetFullTarget(p.Head, p.Antennas, p.BodyYaw)
				case <-done:
					return
				}
			}
		}()

		// Estimate audio duration.
		audioDuration := time.Duration(float64(len(pcm))/float64(sampleRate*2)*float64(time.Second)) + 500*time.Millisecond
		if audioDuration < 2*time.Second {
			audioDuration = 2 * time.Second
		}

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-time.After(audioDuration):
		case <-sig:
		}
		close(done)
		mot.SetLevel(0)
		time.Sleep(500 * time.Millisecond)
	} else {
		// No motors -- just wait for playback.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}

	log.Println("done")
}

func splitHost(addr string) (string, string, string) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], ""
		}
	}
	return addr, "", ""
}
