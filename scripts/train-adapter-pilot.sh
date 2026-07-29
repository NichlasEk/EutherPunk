#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
RUNTIME="${EUTHERPUNK_TRAINING_RUNTIME:-$ROOT/.training-runtime}"
DATASET="${EUTHERPUNK_TRAINING_DATASET:-$ROOT/training/outputs/repair-dataset-diverse-v3-final}"
OUTPUT="${EUTHERPUNK_TRAINING_OUTPUT:-$ROOT/training/models/devstral-repair-pilot-v1}"

export UV_CACHE_DIR="${UV_CACHE_DIR:-$RUNTIME/uv-cache}"
export UV_PYTHON_INSTALL_DIR="${UV_PYTHON_INSTALL_DIR:-$RUNTIME/python}"
export HF_HOME="${HF_HOME:-$RUNTIME/huggingface}"
export HF_HUB_DISABLE_TELEMETRY=1
export TRANSFORMERS_NO_ADVISORY_WARNINGS=1
export PYTHONUNBUFFERED=1
# Avoid large unusable CUDA memory islands while the 24B model, LoRA
# parameters and checkpointed activations share a 24 GB card.
export PYTORCH_ALLOC_CONF="${PYTORCH_ALLOC_CONF:-expandable_segments:True}"
export PYTORCH_CUDA_ALLOC_CONF="${PYTORCH_CUDA_ALLOC_CONF:-expandable_segments:True}"
export VIRTUAL_ENV="$RUNTIME/venv"

mkdir -p "$RUNTIME"
chmod 700 "$RUNTIME"

uv python install 3.12
if [[ ! -x "$RUNTIME/venv/bin/python" ]]; then
  uv venv --python 3.12 "$RUNTIME/venv"
fi
uv sync \
  --project "$ROOT/training_tools" \
  --locked \
  --active \
  --no-install-project

exec "$RUNTIME/venv/bin/python" "$ROOT/training_tools/train_qlora.py" \
  --dataset "$DATASET" \
  --output "$OUTPUT" \
  "$@"
