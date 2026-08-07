// Package audio provides Opus-to-PCM decoding via ffmpeg.
//
// ElevenLabs STT requires PCM int16 audio, but the Reachy daemon produces
// Opus. This package pipes Opus RTP packets through ffmpeg to get PCM.
//
// The Reachy Mini ships with ffmpeg, so this works without extra deps.
package audio

import (
	"fmt"
	"io"
	"log"
	"os/exec"
)

// OpusDecoder decodes Opus audio to PCM int16 via ffmpeg.
type OpusDecoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// NewOpusDecoder starts an ffmpeg process that decodes Opus to PCM int16.
// sampleRate is the output sample rate (16000 for ElevenLabs STT).
// channels is the output channel count (1 for mono).
func NewOpusDecoder(sampleRate int, channels int) (*OpusDecoder, error) {
	// ffmpeg reads Opus from stdin, outputs raw PCM s16le to stdout.
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "opus",
		"-i", "pipe:0",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-ac", fmt.Sprintf("%d", channels),
		"pipe:1",
	}

	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	// Suppress stderr since we use -loglevel error.
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	log.Printf("audio: ffmpeg opus->pcm decoder started (%d Hz, %d ch)", sampleRate, channels)
	return &OpusDecoder{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// Write feeds raw Opus data to the decoder.
func (d *OpusDecoder) Write(opusData []byte) error {
	_, err := d.stdin.Write(opusData)
	return err
}

// Read reads decoded PCM int16 data from the decoder.
func (d *OpusDecoder) Read(pcm []byte) (int, error) {
	return d.stdout.Read(pcm)
}

// Close shuts down the decoder.
func (d *OpusDecoder) Close() {
	d.stdin.Close()
	d.cmd.Wait()
}
