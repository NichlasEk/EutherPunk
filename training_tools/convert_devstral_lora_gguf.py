#!/usr/bin/env python3
"""Convert a Devstral Small 2 PEFT adapter for Ollama's Mistral3 base."""

from __future__ import annotations

import argparse
import runpy
import sys
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--llama-cpp", type=Path, required=True)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--adapter", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    llama_cpp = args.llama_cpp.resolve()
    converter = llama_cpp / "convert_lora_to_gguf.py"
    if not converter.is_file():
        parser.error(f"missing llama.cpp converter: {converter}")

    sys.path[:0] = [str(llama_cpp), str(llama_cpp / "gguf-py")]
    import gguf

    # Upstream llama.cpp maps Mistral3ForConditionalGeneration through its
    # Llama tensor converter. Ollama's packaged Devstral GGUF correctly uses
    # the distinct `mistral3` architecture tag and rejects a `llama` adapter
    # before applying any tensors. Keep the established tensor mapping and
    # change only the adapter metadata tag to match that base.
    gguf.MODEL_ARCH_NAMES[gguf.MODEL_ARCH.LLAMA] = "mistral3"
    sys.argv = [
        str(converter),
        str(args.adapter.resolve()),
        "--base",
        str(args.base.resolve()),
        "--outtype",
        "f16",
        "--outfile",
        str(args.output.resolve()),
    ]
    if args.dry_run:
        sys.argv.append("--dry-run")
    runpy.run_path(str(converter), run_name="__main__")


if __name__ == "__main__":
    main()
