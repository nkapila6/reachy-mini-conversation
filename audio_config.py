#!/usr/bin/env python3
"""Apply the Reachy Mini audio DSP config for voice capture.

This mirrors what the conversation app does in audio/startup_config.py.
Run once before starting reachy-agent, or let the agent call it automatically.

Usage:
    python3 audio_config.py [--robot localhost]
"""

import sys
import time
import argparse

# Same config as reachy_mini_conversation_app/audio/startup_config.py
AUDIO_STARTUP_CONFIG = (
    ("PP_AGCMAXGAIN", (10.0,)),  # Max auto gain
    ("PP_MIN_NS", (0.8,)),  # Min noise suppression
    ("PP_MIN_NN", (0.8,)),  # Min neural noise reduction
    ("PP_GAMMA_E", (0.5,)),  # Echo cancellation
    ("PP_GAMMA_ETAIL", (0.5,)),  # Echo tail cancellation
    ("PP_NLATTENONOFF", (0,)),  # Non-linear attenuation off
    ("PP_MGSCALE", (4.0, 1.0, 1.0)),  # Mic gain scale
)


def main():
    parser = argparse.ArgumentParser(description="Apply Reachy Mini audio DSP config")
    parser.add_argument("--robot", default="localhost", help="Reachy hostname or IP")
    args = parser.parse_args()

    try:
        from reachy_mini.media.media_manager import MediaBackend
    except ImportError:
        print(
            "ERROR: reachy_mini SDK not installed. Install with: pip install reachy-mini",
            file=sys.stderr,
        )
        sys.exit(1)

    print(f"Connecting to Reachy at {args.robot}...")
    try:
        from reachy_mini import ReachyMini

        robot = ReachyMini(host=args.robot)
    except Exception as e:
        print(f"ERROR: Could not connect to Reachy: {e}", file=sys.stderr)
        sys.exit(1)

    audio = getattr(getattr(robot, "media", None), "audio", None)
    if audio is None:
        print("ERROR: robot.media.audio is not available", file=sys.stderr)
        sys.exit(1)

    apply_config = getattr(audio, "apply_audio_config", None)
    if not callable(apply_config):
        print("ERROR: audio.apply_audio_config is not available", file=sys.stderr)
        sys.exit(1)

    print("Applying audio DSP config...")
    try:
        ok = apply_config(AUDIO_STARTUP_CONFIG, verify=True, write_settle_seconds=0.1)
    except Exception as e:
        print(f"ERROR: audio config failed: {e}", file=sys.stderr)
        sys.exit(1)

    if ok:
        print("Audio config applied successfully.")
        # Give the DSP a moment to settle.
        time.sleep(0.5)
    else:
        print("WARNING: Audio config was not applied (verify failed).", file=sys.stderr)

    robot.close()
    print("Done. The microphone should now produce audible audio.")


if __name__ == "__main__":
    main()
