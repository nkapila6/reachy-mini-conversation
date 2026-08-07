#!/bin/bash
# test.sh -- quick pre-hackathon smoke test for reachy-utils
#
# Runs unit tests (always) and integration tests (only if API keys are set).
# Reports what's working and what needs attention.
#
# API keys go in your shell environment. Set them before running:
#   export DEEPGRAM_API_KEY=sk-...
#   export OPENROUTER_API_KEY=sk-or-...
#   export ELEVENLABS_API_KEY=...      # optional
#   export CONTEXT_API_KEY=...         # optional
#
# Then run:
#   ./test.sh
#
# On the robot, put these in ~/.bashrc or a .env file you source before
# starting the agent.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

PASS=0
FAIL=0
SKIP=0

check() {
	local name="$1"
	local status="$2"
	local detail="${3:-}"
	case "$status" in
	PASS)
		echo -e "  ${GREEN}PASS${NC} $name"
		((PASS++))
		;;
	FAIL)
		echo -e "  ${RED}FAIL${NC} $name -- $detail"
		((FAIL++))
		;;
	SKIP)
		echo -e "  ${YELLOW}SKIP${NC} $name -- $detail"
		((SKIP++))
		;;
	esac
}

echo ""
echo -e "${CYAN}=== reachy-utils pre-hackathon smoke test ===${NC}"
echo ""

# --- Go build ---
echo -e "${CYAN}[build]${NC} compiling all packages..."
if go build ./... 2>&1; then
	check "go build ./..." PASS
else
	check "go build ./..." FAIL "compilation errors above"
fi

# --- Unit tests (always run) ---
echo ""
echo -e "${CYAN}[unit tests]${NC} running unit tests..."

# Motion tests (pure math, no deps)
if go test -short -run 'Test(New|SetLevel|Tick|SE3|Clamp|Spring|Concurrent)' ./pkg/motion/ -v 2>&1 | tail -5; then
	check "motion model" PASS
else
	check "motion model" FAIL "motion tests failed"
fi

# TTS unit tests (no API needed)
if go test -short -run 'Test(SplitText|NewSynthesizer)' ./pkg/tts/ -v 2>&1 | tail -5; then
	check "tts (unit)" PASS
else
	check "tts (unit)" FAIL "tts unit tests failed"
fi

# STT unit tests (no API needed)
if go test -short -run 'TestNewTranscriber' ./pkg/stt/ -v 2>&1 | tail -5; then
	check "stt (unit)" PASS
else
	check "stt (unit)" FAIL "stt unit tests failed"
fi

# Context unit tests (no API needed)
if go test -short -run 'TestNew' ./pkg/context/ -v 2>&1 | tail -5; then
	check "context (unit)" PASS
else
	check "context (unit)" FAIL "context unit tests failed"
fi

# --- API key checks ---
echo ""
echo -e "${CYAN}[api keys]${NC} checking environment..."

if [ -n "${DEEPGRAM_API_KEY:-}" ]; then
	check "DEEPGRAM_API_KEY" PASS
else
	check "DEEPGRAM_API_KEY" SKIP "not set -- STT integration tests will be skipped"
fi

if [ -n "${ELEVENLABS_API_KEY:-}" ]; then
	check "ELEVENLABS_API_KEY" PASS
else
	check "ELEVENLABS_API_KEY" SKIP "not set -- TTS integration tests will be skipped"
fi

if [ -n "${CONTEXT_API_KEY:-}" ]; then
	check "CONTEXT_API_KEY" PASS
else
	check "CONTEXT_API_KEY" SKIP "not set -- context.dev integration tests will be skipped"
fi

if [ -n "${OPENROUTER_API_KEY:-}" ]; then
	check "OPENROUTER_API_KEY" PASS
else
	check "OPENROUTER_API_KEY" SKIP "not set -- LLM integration tests will be skipped"
fi

# --- Integration tests (only if keys are set) ---
echo ""
echo -e "${CYAN}[integration tests]${NC} running API integration tests..."

# Agent end-to-end (LLM response test -- no robot needed)
if [ -n "${OPENROUTER_API_KEY:-}" ]; then
	if go test -short -run 'TestAgentLLMResponse' ./cmd/reachy-agent/ -v -timeout 30s 2>&1 | tail -10; then
		check "agent (LLM response)" PASS
	else
		check "agent (LLM response)" FAIL "LLM call failed"
	fi
else
	check "agent (LLM response)" SKIP "OPENROUTER_API_KEY not set"
fi

# Agent unit tests (no keys needed)
if go test -short -run 'TestAgent(QuestionDetection|ExitCommands|SpeakFallback)' ./cmd/reachy-agent/ -v -timeout 10s 2>&1 | tail -5; then
	check "agent (logic)" PASS
else
	check "agent (logic)" FAIL "agent logic tests failed"
fi

# TTS integration
if [ -n "${ELEVENLABS_API_KEY:-}" ]; then
	if go test -short -run 'TestSynthesizeIntegration' ./pkg/tts/ -v -timeout 30s 2>&1 | tail -5; then
		check "tts (elevenlabs synthesize)" PASS
	else
		check "tts (elevenlabs synthesize)" FAIL "TTS API call failed"
	fi

	if go test -short -run 'TestListVoicesIntegration' ./pkg/tts/ -v -timeout 30s 2>&1 | tail -5; then
		check "tts (elevenlabs list voices)" PASS
	else
		check "tts (elevenlabs list voices)" FAIL "voice list API call failed"
	fi
else
	check "tts (elevenlabs)" SKIP "ELEVENLABS_API_KEY not set"
fi

# Context integration
if [ -n "${CONTEXT_API_KEY:-}" ]; then
	if go test -short -run 'TestFetchURLIntegration' ./pkg/context/ -v -timeout 30s 2>&1 | tail -5; then
		check "context (fetch URL)" PASS
	else
		check "context (fetch URL)" FAIL "context.dev fetch failed"
	fi

	if go test -short -run 'TestSearchIntegration' ./pkg/context/ -v -timeout 30s 2>&1 | tail -5; then
		check "context (search)" PASS
	else
		check "context (search)" FAIL "context.dev search failed"
	fi
else
	check "context (integration)" SKIP "CONTEXT_API_KEY not set"
fi

# --- Binary size check ---
echo ""
echo -e "${CYAN}[binaries]${NC} checking binary sizes..."
for bin in reachy-mic reachy-speak reachy-fetch reachy-agent; do
	if [ -f "$bin" ]; then
		size=$(ls -lh "$bin" | awk '{print $5}')
		check "$bin" PASS "$size"
	else
		check "$bin" SKIP "not built (run: go build -o $bin ./cmd/$bin)"
	fi
done

# --- Summary ---
echo ""
echo -e "${CYAN}=== summary ===${NC}"
echo -e "  ${GREEN}passed:${NC} $PASS"
echo -e "  ${RED}failed:${NC} $FAIL"
echo -e "  ${YELLOW}skipped:${NC} $SKIP"
echo ""

if [ $FAIL -gt 0 ]; then
	echo -e "${RED}Some tests failed. Fix the issues above before the hackathon.${NC}"
	exit 1
fi

if [ $SKIP -gt 0 ]; then
	echo -e "${YELLOW}Some tests were skipped. Set the missing API keys for full coverage.${NC}"
	echo ""
	echo "To set keys:"
	[ -z "${DEEPGRAM_API_KEY:-}" ] && echo "  export DEEPGRAM_API_KEY=your-key-here"
	[ -z "${ELEVENLABS_API_KEY:-}" ] && echo "  export ELEVENLABS_API_KEY=your-key-here"
	[ -z "${CONTEXT_API_KEY:-}" ] && echo "  export CONTEXT_API_KEY=your-key-here"
	[ -z "${OPENROUTER_API_KEY:-}" ] && echo "  export OPENROUTER_API_KEY=your-key-here"
	echo ""
fi

echo -e "${GREEN}Ready for the hackathon.${NC}"
