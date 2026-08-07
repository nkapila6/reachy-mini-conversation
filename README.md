# reachy-mini-conversation

Voice-first AI agent for the Reachy Mini robot. It listens, thinks, speaks, and moves, all on-robot.

## What it does

The robot listens to you, transcribes your speech with ElevenLabs Scribe, generates a response via an OpenRouter LLM, synthesizes speech with Kokoro TTS, and plays it through the speaker while moving its head and antennas in sync.

## Requirements

- Reachy Mini (Wireless) with daemon running
- SSH access to the robot
- OpenRouter API key (for LLM + TTS)
- ElevenLabs API key (for STT)

## Setup

```bash
cp .env.example .env
# Fill in: ELEVENLABS_API_KEY, OPENROUTER_API_KEY

./deploy.sh
```

## Run

```bash
ssh pollen@reachy-mini.local
cd ~/reachy
./reachy-agent -robot localhost:8000 -tts openrouter -voice af_heart
```

Talk to the robot. Press Ctrl-C to stop (the robot goes to sleep).

## Binaries

- `reachy-agent` - full conversation loop (listen, think, speak, move)
- `reachy-mic` - just listen and print transcriptions
- `reachy-speak` - just speak text (supports piping)
- `reachy-fetch` - fetch web data from context.dev

## Options

| Flag | Default | Description |
|---|---|---|
| `-tts` | `openrouter` | TTS backend: `openrouter` or `elevenlabs` |
| `-voice` | `am_onyx` | Kokoro voice (af_heart, am_onyx, bf_emma, hf_alpha, etc) |
| `-model` | `openai/gpt-4o-mini` | LLM model on OpenRouter |
| `-language` | `en` | STT language |
| `-no-motors` | `false` | Skip motor connection |

Full voice list: [huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md](https://huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md)

## How it works

A Go binary handles all logic (STT, LLM, TTS, motion). A small Python bridge (`audio_bridge.py`) handles mic capture and speaker playback via the official `reachy_mini` SDK. They communicate over JSON lines on stdin/stdout.