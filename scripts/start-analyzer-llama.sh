#!/usr/bin/env bash
set -euo pipefail

MODEL_FILE="${LLM_MODEL_FILE:-Qwen3.5-0.8B-Q4_K_M.gguf}"
MODEL_PATH="/models/${MODEL_FILE}"
LLAMA_HOST="${LLAMA_HOST:-0.0.0.0}"
LLAMA_PORT="${LLAMA_PORT:-8080}"
LLAMA_CTX="${LLAMA_CTX:-2048}"
LLAMA_NGL="${LLAMA_NGL:-0}"
# New name: LLM_PARALLEL_THREADS. Keep LLAMA_THREADS as legacy fallback.
LLAMA_THREADS="${LLM_PARALLEL_THREADS:-${LLAMA_THREADS:--1}}"
LLAMA_EXTRA_ARGS="${LLAMA_EXTRA_ARGS:-}"

if [[ ! -f "${MODEL_PATH}" ]]; then
  echo "model not found: ${MODEL_PATH}" >&2
  exit 1
fi

read -r -a EXTRA_ARGS <<< "${LLAMA_EXTRA_ARGS}"

export MQTT_CLIENT_ID="job-analyzer-${HOSTNAME}"
export LLM_ENDPOINT="http://127.0.0.1:${LLAMA_PORT}"

/app/llama-server \
  -m "${MODEL_PATH}" \
  --host "${LLAMA_HOST}" \
  --port "${LLAMA_PORT}" \
  -c "${LLAMA_CTX}" \
  -ngl "${LLAMA_NGL}" \
  -t "${LLAMA_THREADS}" \
  "${EXTRA_ARGS[@]}" &
LLAMA_PID=$!

/app/service &
ANALYZER_PID=$!

graceful_shutdown() {
  kill -TERM "${ANALYZER_PID}" "${LLAMA_PID}" 2>/dev/null || true
}
trap graceful_shutdown TERM INT

set +e
wait -n "${ANALYZER_PID}" "${LLAMA_PID}"
EXIT_CODE=$?
set -e

graceful_shutdown
wait "${ANALYZER_PID}" 2>/dev/null || true
wait "${LLAMA_PID}" 2>/dev/null || true
exit "${EXIT_CODE}"
