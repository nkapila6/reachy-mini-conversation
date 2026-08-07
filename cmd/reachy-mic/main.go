// reachy-mic captures microphone audio from the Reachy Mini and
// transcribes it to text using ElevenLabs Scribe v2 Realtime.
// Prints transcriptions to stdout.
//
// Uses the Python audio bridge (same as reachy-agent) for mic capture.
//
// Usage:
//
//	reachy-mic -robot localhost:8000
//	reachy-mic -language fr
//	reachy-mic -no-motors  # audio-only, no robot motion
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nkapila6/reachy-utils/pkg/bridge"
	"github.com/nkapila6/reachy-utils/pkg/daemon"
	"github.com/nkapila6/reachy-utils/pkg/env"
	"github.com/nkapila6/reachy-utils/pkg/motion"
	"github.com/nkapila6/reachy-utils/pkg/stt"
)

func main() {
	env.Load()

	robot := flag.String("robot", "localhost:8000", "reachy_mini daemon host:port")
	language := flag.String("language", "en", "BCP-47 language code for STT")
	interim := flag.Bool("interim", false, "print interim (partial) transcriptions")
	noMotors := flag.Bool("no-motors", false, "don't drive the robot's motors")
	flag.Parse()

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
		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for range tick.C {
				p := mot.Tick()
				d.SetFullTarget(p.Head, p.Antennas, p.BodyYaw)
			}
		}()
	}

	// Create the ElevenLabs STT transcriber.
	transcriber, err := stt.NewElevenLabsTranscriber(stt.ElevenLabsSTTOpts{
		Language:   *language,
		SampleRate: 16000,
	})
	if err != nil {
		log.Fatalf("stt: %v", err)
	}
	defer transcriber.Close()

	// Pipe bridge audio (PCM int16) to ElevenLabs STT.
	go func() {
		for pcm := range br.Audio() {
			if err := transcriber.Write(pcm); err != nil {
				log.Printf("stt write: %v", err)
				return
			}
		}
	}()

	// Print transcriptions.
	go func() {
		for r := range transcriber.Results() {
			if r.IsFinal {
				fmt.Println(r.Text)
			} else if *interim {
				fmt.Printf("\r%s", r.Text)
			}
		}
	}()

	// Handle errors.
	go func() {
		for err := range transcriber.Errors() {
			log.Printf("stt error: %v", err)
		}
	}()

	// Wait for Ctrl-C.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")

	_ = mot
}

func splitHost(addr string) (string, string, string) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], ""
		}
	}
	return addr, "", ""
}
