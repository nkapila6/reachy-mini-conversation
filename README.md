# reachy-mini-conversation

Voice-first AI agent for the Reachy Mini robot. Listens, thinks, speaks -- all on-robot.

Built for a hackathon. Uses [context.dev](https://context.dev) for live web data, [OpenRouter](https://openrouter.ai) for LLM + TTS, and [ElevenLabs](https://elevenlabs.io) for STT.

## What it does

The robot listens to you speak, transcribes it with ElevenLabs Scribe, fetches live web context from context.dev (if you ask a question or mention a URL), generates a response via an OpenRouter LLM, synthesizes speech with Kokoro TTS, and plays it through the robot's speaker while moving its head and antennas in sync.

## Architecture

```
Go agent (STT, LLM, TTS, motion)
  |  JSON lines (stdin/stdout)
Python audio bridge (reachy_mini SDK)
  |  robot.media.get_audio_sample() / push_audio_sample()
Reachy Mini daemon (motors, mic, speaker)
```

The Go binary handles all logic. A small Python bridge (`audio_bridge.py`) handles mic capture and speaker playback via the official `reachy_mini` SDK -- same approach as the [conversation app](https://github.com/pollen-robotics/reachy_mini_conversation_app).

## Binaries

| Binary | What it does |
|---|---|
| `reachy-agent` | Full conversation loop: listen -> think -> speak -> move |
| `reachy-mic` | Just listen and print transcriptions |
| `reachy-speak` | Just speak text (supports piping) |
| `reachy-fetch` | Fetch web data from context.dev |

## Setup

### Prerequisites

- A Reachy Mini (Wireless) with the daemon running
- SSH access to the robot (`ssh pollen@reachy-mini.local`)
- An OpenRouter API key
- An ElevenLabs API key

### Configure

```bash
cp .env.example .env
# Fill in your API keys:
#   ELEVENLABS_API_KEY=sk-...
#   OPENROUTER_API_KEY=sk-or-...
```

### Deploy

```bash
./deploy.sh
```

This builds the Go binaries for ARM64, compares SHA256 hashes to skip unchanged files, scps only what changed, and syncs the Python venv on the robot.

### Run

On the robot:

```bash
ssh pollen@reachy-mini.local
cd ~/reachy
./reachy-agent -robot localhost:8000 -tts openrouter -voice af_heart
```

Talk to the robot. Say "exit" or press Ctrl-C to stop (the robot goes to sleep).

## Options

```
./reachy-agent -robot localhost:8000 -tts openrouter -voice af_heart
```

| Flag | Default | Description |
|---|---|---|
| `-robot` | `localhost:8000` | Daemon host:port |
| `-tts` | `openrouter` | TTS backend: `openrouter` or `elevenlabs` |
| `-voice` | `am_onyx` | Voice name (see voices below) |
| `-model` | `openai/gpt-4o-mini` | LLM model on OpenRouter |
| `-language` | `en` | STT language |
| `-no-motors` | `false` | Skip motor connection (for testing without robot) |

## Kokoro voices

Available voices for `-tts openrouter`:

- American English: `af_heart` (best), `af_bella`, `am_onyx`, `am_michael`, `am_puck`
- British English: `bf_emma`, `bm_george`
- Hindi: `hf_alpha`, `hm_omega`
- Japanese: `jf_alpha`, `jm_kumo`
- Mandarin: `zf_xiaobei`, `zm_yunxi`
- Spanish: `ef_dora`, `em_alex`
- French: `ff_siwis`
- Italian: `if_sara`, `im_nicola`
- Portuguese: `pf_dora`, `pm_alex`

Full list at [huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md](https://huggingface.co/hexgrad/Kokoro-82M/blob/main/VOICES.md)

## Project structure

```
cmd/
  reachy-agent/   # Full conversation loop
  reachy-mic/     # STT only
  reachy-speak/   # TTS only
  reachy-fetch/   # Web fetch only
pkg/
  bridge/         # Python audio bridge client (JSON lines protocol)
  daemon/         # Reachy daemon client (motion WebSocket, wake/sleep)
  motion/         # Speech-driven head/antenna/body motion model (50 Hz)
  stt/            # ElevenLabs Scribe v2 Realtime STT
  tts/            # OpenRouter Kokoro + ElevenLabs TTS
  context/        # context.dev web fetcher
  env/            # .env file loader
audio_bridge.py   # Python bridge: mic capture + speaker playback via SDK
deploy.sh         # Build + scp + venv sync
```

## How it works

1. Go agent starts the daemon, spawns the Python audio bridge
2. Bridge creates `ReachyMini()`, wakes the robot, starts media pipelines, applies XVF3800 DSP config
3. Bridge captures mic audio as PCM int16 at 16kHz, sends to Go via JSON lines
4. Go streams PCM to ElevenLabs Scribe v2 Realtime over WebSocket
5. On final transcript, Go fetches web context (if question/URL detected) and calls OpenRouter LLM
6. Go synthesizes speech via OpenRouter Kokoro TTS (or ElevenLabs), sends PCM back to bridge
7. Bridge plays audio through the robot's speaker
8. Go drives head/antenna motion in sync with audio playback