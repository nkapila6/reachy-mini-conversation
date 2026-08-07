#!/usr/bin/env python3
"""Audio bridge for reachy-agent.

Uses the reachy_mini SDK to capture mic audio and play speaker audio.
Communicates with the Go agent via JSON lines on stdin/stdout.
"""

import sys
import json
import base64
import time
import argparse
import logging
import asyncio
import threading
import numpy as np

logging.basicConfig(level=logging.INFO, format="bridge: %(message)s", stream=sys.stderr)
log = logging.getLogger(__name__)

AUDIO_STARTUP_CONFIG = (
    ("PP_AGCMAXGAIN", (10.0,)),
    ("PP_MIN_NS", (0.8,)),
    ("PP_MIN_NN", (0.8,)),
    ("PP_GAMMA_E", (0.5,)),
    ("PP_GAMMA_ETAIL", (0.5,)),
    ("PP_NLATTENONOFF", (0,)),
    ("PP_MGSCALE", (4.0, 1.0, 1.0)),
)

send_lock = threading.Lock()


def send(msg):
    with send_lock:
        sys.stdout.write(json.dumps(msg) + "\n")
        sys.stdout.flush()


def apply_audio_config(robot):
    audio = getattr(getattr(robot, "media", None), "audio", None)
    if audio is None:
        log.warning("robot.media.audio not available")
        return False
    apply_config = getattr(audio, "apply_audio_config", None)
    if not callable(apply_config):
        log.warning("audio.apply_audio_config not available")
        return False
    try:
        ok = apply_config(AUDIO_STARTUP_CONFIG, verify=True, write_settle_seconds=0.1)
        if ok:
            log.info("audio DSP config applied")
        return ok
    except Exception as e:
        log.warning("audio DSP config error: %s", e)
        return False


async def run_loops(robot, tts_pipeline, voice_name):
    """Run mic capture and command loops concurrently (matches conversation app)."""
    stop = asyncio.Event()
    logged_format = False
    chunk_count = 0

    async def record_loop():
        nonlocal logged_format, chunk_count
        while not stop.is_set():
            try:
                frame = robot.media.get_audio_sample()
            except Exception:
                await asyncio.sleep(0.01)
                continue
            if frame is not None and frame.size > 0:
                if not logged_format:
                    log.info("audio frame: shape=%s dtype=%s", frame.shape, frame.dtype)
                    logged_format = True

                # Convert to mono int16 (matches conversation app's audio_to_int16).
                if frame.ndim > 1:
                    frame = frame.mean(axis=1)
                if frame.dtype == np.float32 or frame.dtype == np.float64:
                    frame = (frame * 32767.0).clip(-32768, 32767).astype(np.int16)
                elif frame.dtype != np.int16:
                    frame = frame.astype(np.int16)

                chunk_count += 1
                if chunk_count % 50 == 1:
                    rms = np.sqrt(np.mean(frame.astype(np.float32) ** 2))
                    log.info(
                        "audio level: rms=%.1f chunk=%d samples=%d",
                        rms,
                        chunk_count,
                        len(frame),
                    )

                pcm = frame.tobytes()
                send({"type": "audio", "pcm": base64.b64encode(pcm).decode()})
            await asyncio.sleep(0)

    async def command_loop():
        loop = asyncio.get_running_loop()
        while not stop.is_set():
            line = await loop.run_in_executor(None, sys.stdin.readline)
            line = line.strip()
            if not line:
                continue
            try:
                cmd = json.loads(line)
            except json.JSONDecodeError:
                continue

            if cmd.get("type") == "speak":
                # Text for local Kokoro TTS.
                text = cmd.get("text", "")
                if text and tts_pipeline is not None:
                    try:

                        def synth():
                            audio_chunks = []
                            for gs, ps, audio in tts_pipeline(
                                text, voice=voice_name, speed=1
                            ):
                                audio_chunks.append(audio)
                            if audio_chunks:
                                return np.concatenate(audio_chunks)
                            return None

                        audio = await loop.run_in_executor(None, synth)
                        if audio is not None:
                            # Kokoro outputs 24kHz float32. Downsample to 16kHz.
                            indices = np.arange(0, len(audio), 1.5).astype(int)
                            audio = audio[indices]
                            robot.media.push_audio_sample(audio)
                            log.info("spoke (kokoro): %s", text[:80])
                    except Exception as e:
                        log.error("speak error: %s", e)
                elif text:
                    log.info("no TTS, would say: %s", text)

            elif cmd.get("type") == "speak_pcm":
                # PCM int16 from cloud TTS (OpenRouter or ElevenLabs).
                audio_b64 = cmd.get("pcm", "")
                sample_rate = cmd.get("sample_rate", 24000)
                if audio_b64:
                    try:
                        audio_bytes = base64.b64decode(audio_b64)
                        pcm = (
                            np.frombuffer(audio_bytes, dtype=np.int16).astype(
                                np.float32
                            )
                            / 32768.0
                        )
                        # Resample from source rate to 16kHz if needed.
                        if sample_rate != 16000 and sample_rate > 0:
                            ratio = float(sample_rate) / 16000.0
                            indices = np.arange(0, len(pcm), ratio).astype(int)
                            pcm = pcm[indices]
                        # Boost volume.
                        pcm = np.clip(pcm * 3.0, -1.0, 1.0)
                        send({"type": "playing"})
                        robot.media.push_audio_sample(pcm)
                        log.info(
                            "spoke (pcm): %d bytes -> %d samples (peak=%.3f)",
                            len(audio_bytes),
                            len(pcm),
                            float(np.max(np.abs(pcm))),
                        )
                    except Exception as e:
                        log.error("speak error: %s", e)

            elif cmd.get("type") == "stop_speaking":
                try:
                    robot.media.audio.clear_player()
                except Exception:
                    pass

            elif cmd.get("type") == "quit":
                stop.set()
                return

    tasks = [
        asyncio.create_task(record_loop(), name="record-loop"),
        asyncio.create_task(command_loop(), name="command-loop"),
    ]
    try:
        await asyncio.gather(*tasks)
    except asyncio.CancelledError:
        pass
    finally:
        stop.set()
        try:
            robot.media.stop_recording()
            robot.media.stop_playing()
        except Exception:
            pass
        log.info("bridge stopped")


def main():
    parser = argparse.ArgumentParser(description="Reachy audio bridge")
    parser.add_argument("--robot", default="localhost", help="Reachy hostname or IP")
    parser.add_argument("--voice", default="af_heart", help="Kokoro voice name")
    args = parser.parse_args()

    from reachy_mini import ReachyMini

    # Create the robot -- auto-detect (matches conversation app).
    log.info("connecting to Reachy (auto-detect)...")
    try:
        robot = ReachyMini()
    except Exception as e:
        send({"type": "error", "message": f"could not connect: {e}"})
        sys.exit(1)

    # Wake up if sleeping (matches conversation app's app_lifecycle.wake_up_if_sleeping).
    try:
        robot.enable_motors()
        robot.wake_up()
        log.info("robot woken up")
    except Exception as e:
        log.warning("wake_up failed: %s", e)

    # Start media pipelines first, then apply DSP config (matches conversation app).
    log.info("starting media pipelines...")
    robot.media.start_recording()
    robot.media.start_playing()
    time.sleep(1)

    apply_audio_config(robot)

    try:
        input_rate = robot.media.get_input_audio_samplerate()
        log.info("input sample rate: %d Hz", input_rate)
    except Exception:
        input_rate = 16000
        log.info("could not get sample rate, assuming %d Hz", input_rate)

    # Initialize Kokoro TTS pipeline.
    tts_pipeline = None
    try:
        from kokoro import KPipeline

        tts_pipeline = KPipeline(lang_code="a")  # American English
        log.info("kokoro TTS ready (voice=%s)", args.voice)
    except Exception as e:
        log.warning("kokoro TTS not available: %s (will print responses instead)", e)

    send({"type": "ready"})
    log.info("bridge ready")

    asyncio.run(run_loops(robot, tts_pipeline, args.voice))


if __name__ == "__main__":
    main()
