#!/usr/bin/env python3
"""Run the private EutherPunk Devstral QLoRA pilot."""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
import sys
import traceback
from pathlib import Path
from typing import Any


DEFAULT_MODEL = "mistralai/Devstral-Small-2-24B-Instruct-2512"
DEFAULT_REVISION = "55c5b41e98c2dbd21b0c8afffc540dcfc9eb5128"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    with path.open("r", encoding="utf-8") as handle:
        return [json.loads(line) for line in handle if line.strip()]


def verify_authorization(dataset_dir: Path, authorization_path: Path) -> dict[str, Any]:
    authorization = json.loads(authorization_path.read_text(encoding="utf-8"))
    decision = authorization.get("authorization", {})
    if decision.get("training_authorized") is not True:
        raise ValueError("dataset is not explicitly authorized for training")
    if decision.get("scope") != "private local QLoRA pilot only; no upload or publication":
        raise ValueError("authorization scope is missing or unexpected")

    for label, name in (
        ("train", "train.jsonl"),
        ("holdout", "holdout.jsonl"),
        ("manifest", "manifest.json"),
    ):
        path = dataset_dir / name
        expected = authorization.get("files", {}).get(label, {}).get("sha256")
        if not expected or sha256(path) != expected:
            raise ValueError(f"{name} no longer matches its reviewed hash")
    return authorization


def serialized_example(example: dict[str, Any]) -> dict[str, str]:
    messages = example["messages"]
    system = messages[0]["content"]
    user = messages[1]["content"]
    completion = messages[2]["content"]
    prompt = f"<s>[SYSTEM_PROMPT]{system}[/SYSTEM_PROMPT][INST]{user}[/INST]"
    return {"prompt": prompt, "completion": f"{completion}</s>"}


def fit_example(
    example: dict[str, Any],
    processor: Any,
    max_length: int,
    *,
    minimum_prompt_tokens: int = 256,
) -> tuple[dict[str, str], dict[str, int | bool]]:
    """Fit an example without ever truncating the completion away.

    TRL normally truncates the right side of prompt+completion. Repair prompts
    can contain a complete source file, so that behavior can leave a sample
    with no supervised tokens. Preserve a useful head and tail of oversized
    prompts and give the completion the remaining budget. Only the single
    known giant completion is itself clipped in this small-VRAM pilot.
    """

    serialized = serialized_example(example)
    prompt_ids = processor.encode(
        serialized["prompt"],
        add_special_tokens=False,
    )
    completion_ids = processor.encode(
        serialized["completion"],
        add_special_tokens=False,
    )
    # Leave room for tokenizer-added boundary tokens.
    usable = max_length - 2
    prompt_budget = min(len(prompt_ids), max(minimum_prompt_tokens, usable - len(completion_ids)))
    prompt_budget = min(prompt_budget, usable - 1)
    completion_budget = usable - prompt_budget

    prompt_truncated = len(prompt_ids) > prompt_budget
    completion_truncated = len(completion_ids) > completion_budget
    if prompt_truncated:
        head = max(1, (prompt_budget * 3) // 5)
        tail = prompt_budget - head
        fitted_prompt_ids = prompt_ids[:head]
        if tail:
            fitted_prompt_ids += prompt_ids[-tail:]
    else:
        fitted_prompt_ids = prompt_ids
    fitted_completion_ids = completion_ids[:completion_budget]

    fitted = {
        "prompt": processor.decode(fitted_prompt_ids),
        "completion": processor.decode(fitted_completion_ids),
    }
    stats: dict[str, int | bool] = {
        "prompt_tokens_original": len(prompt_ids),
        "completion_tokens_original": len(completion_ids),
        "prompt_tokens_used": len(fitted_prompt_ids),
        "completion_tokens_used": len(fitted_completion_ids),
        "prompt_truncated": prompt_truncated,
        "completion_truncated": completion_truncated,
    }
    return fitted, stats


@contextlib.contextmanager
def fp8_checkpoint_to_bnb_loader():
    """Compose Transformers' FP8 dequantizer with its bnb quantizer.

    The official checkpoint stores `weight`, `weight_scale_inv` and an unused
    static activation scale. Transformers normally selects either the FP8
    loader or bitsandbytes. This scoped adapter makes its existing streaming
    conversion pipeline apply both operations, per layer, without ever
    materializing the whole model in BF16.
    """

    from transformers.core_model_loading import WeightConverter
    from transformers.integrations.finegrained_fp8 import Fp8Dequantize
    from transformers.quantizers.quantizer_bnb_4bit import Bnb4BitHfQuantizer

    original = Bnb4BitHfQuantizer.update_weight_conversions

    def update_weight_conversions(self, conversions):
        updated = []
        for conversion in conversions:
            if not isinstance(conversion, WeightConverter):
                updated.append(conversion)
                continue
            weight_sources = [
                pattern
                for pattern in conversion.source_patterns
                if pattern.endswith(".weight")
            ]
            if not weight_sources:
                updated.append(conversion)
                continue
            anchored_weights = [pattern + "$" for pattern in weight_sources]
            scale_sources = [
                pattern[: -len(".weight")] + ".weight_scale_inv$"
                for pattern in weight_sources
            ]
            other_sources = [
                pattern
                for pattern in conversion.source_patterns
                if not pattern.endswith(".weight")
            ]
            updated.append(
                WeightConverter(
                    source_patterns=anchored_weights + scale_sources + other_sources,
                    target_patterns=conversion._original_target_patterns,
                    operations=[Fp8Dequantize(self)] + list(conversion.operations),
                )
            )
        updated.append(
            WeightConverter(
                source_patterns=[
                    "weight$",
                    "weight_scale_inv",
                    "activation_scale",
                ],
                target_patterns="weight",
                operations=[Fp8Dequantize(self)],
            )
        )
        return updated

    Bnb4BitHfQuantizer.update_weight_conversions = update_weight_conversions
    try:
        yield
    finally:
        Bnb4BitHfQuantizer.update_weight_conversions = original


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dataset", type=Path, required=True)
    parser.add_argument("--authorization", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--revision", default=DEFAULT_REVISION)
    parser.add_argument("--max-length", type=int, default=1536)
    parser.add_argument("--epochs", type=float, default=3.0)
    parser.add_argument("--learning-rate", type=float, default=1e-4)
    parser.add_argument("--rank", type=int, default=8)
    parser.add_argument("--seed", type=int, default=20260729)
    parser.add_argument("--dry-run", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    dataset_dir = args.dataset.resolve()
    authorization_path = (
        args.authorization.resolve()
        if args.authorization
        else dataset_dir / "authorization.json"
    )
    output = args.output.resolve()
    if output.exists():
        print(f"refusing to reuse output directory: {output}", file=sys.stderr)
        return 2

    try:
        authorization = verify_authorization(dataset_dir, authorization_path)
        train_rows = load_jsonl(dataset_dir / "train.jsonl")
        holdout_rows = load_jsonl(dataset_dir / "holdout.jsonl")
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"preflight failed: {error}", file=sys.stderr)
        return 2

    if not train_rows or not holdout_rows:
        print("preflight failed: both train and holdout must be non-empty", file=sys.stderr)
        return 2
    train_groups = {row["group_id"] for row in train_rows}
    holdout_groups = {row["group_id"] for row in holdout_rows}
    if train_groups & holdout_groups:
        print("preflight failed: train/holdout group overlap", file=sys.stderr)
        return 2

    run_manifest = {
        "schema_version": 1,
        "base_model": args.model,
        "base_revision": args.revision,
        "dataset_hashes": authorization["files"],
        "train_examples": len(train_rows),
        "holdout_examples": len(holdout_rows),
        "holdout_used_for_gradient_updates": False,
        "configuration": {
            "method": "QLoRA",
            "quantization": "NF4 double quantization",
            "compute_dtype": "bfloat16",
            "rank": args.rank,
            "lora_alpha": args.rank * 2,
            "lora_dropout": 0.05,
            "max_length": args.max_length,
            "epochs": args.epochs,
            "learning_rate": args.learning_rate,
            "batch_size": 1,
            "gradient_accumulation_steps": 4,
            "seed": args.seed,
        },
    }
    if args.dry_run:
        print(json.dumps(run_manifest, ensure_ascii=False, indent=2))
        return 0

    if os.environ.get("HF_HUB_OFFLINE") == "1":
        print("preflight failed: HF_HUB_OFFLINE=1 but model download may be required", file=sys.stderr)
        return 2

    try:
        import torch
        from datasets import Dataset
        from peft import LoraConfig, TaskType, prepare_model_for_kbit_training
        from transformers import (
            AutoConfig,
            BitsAndBytesConfig,
            Mistral3ForConditionalGeneration,
            MistralCommonBackend,
            set_seed,
        )
        from trl import SFTConfig, SFTTrainer
    except ImportError as error:
        print(f"training dependencies are incomplete: {error}", file=sys.stderr)
        return 2

    if not torch.cuda.is_available():
        print("preflight failed: CUDA is unavailable", file=sys.stderr)
        return 2
    major, _minor = torch.cuda.get_device_capability()
    if major < 8:
        print("preflight failed: the pilot requires a BF16-capable NVIDIA GPU", file=sys.stderr)
        return 2

    set_seed(args.seed)
    output.mkdir(mode=0o700, parents=True)
    manifest_path = output / "run-manifest.json"
    manifest_path.write_text(
        json.dumps(run_manifest, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    os.chmod(manifest_path, 0o600)

    quantization = BitsAndBytesConfig(
        load_in_4bit=True,
        bnb_4bit_quant_type="nf4",
        bnb_4bit_compute_dtype=torch.bfloat16,
        bnb_4bit_use_double_quant=True,
    )
    processor = MistralCommonBackend.from_pretrained(
        args.model,
        revision=args.revision,
    )
    fitted_train = [fit_example(row, processor, args.max_length) for row in train_rows]
    fitted_holdout = [fit_example(row, processor, args.max_length) for row in holdout_rows]
    all_fit_stats = [stats for _example, stats in fitted_train + fitted_holdout]
    run_manifest["preprocessing"] = {
        "strategy": "completion-safe prompt head-tail truncation",
        "prompt_truncated_examples": sum(
            bool(stats["prompt_truncated"]) for stats in all_fit_stats
        ),
        "completion_truncated_examples": sum(
            bool(stats["completion_truncated"]) for stats in all_fit_stats
        ),
        "minimum_prompt_tokens": 256,
    }
    manifest_path.write_text(
        json.dumps(run_manifest, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    # Devstral's official checkpoint stores most tensors as FP8 and declares
    # its inference quantizer in config.json. QLoRA needs a trainable bnb NF4
    # graph instead. Supplying a clean in-memory config makes Transformers
    # stream each published tensor through the bnb quantizer without changing
    # or republishing the source checkpoint.
    model_config = AutoConfig.from_pretrained(
        args.model,
        revision=args.revision,
    )
    model_config.quantization_config = None
    with fp8_checkpoint_to_bnb_loader():
        model = Mistral3ForConditionalGeneration.from_pretrained(
            args.model,
            revision=args.revision,
            config=model_config,
            quantization_config=quantization,
            dtype=torch.bfloat16,
            low_cpu_mem_usage=True,
        )
    model.config.use_cache = False
    model = prepare_model_for_kbit_training(
        model,
        use_gradient_checkpointing=True,
        gradient_checkpointing_kwargs={"use_reentrant": False},
    )

    lora = LoraConfig(
        task_type=TaskType.CAUSAL_LM,
        r=args.rank,
        lora_alpha=args.rank * 2,
        lora_dropout=0.05,
        bias="none",
        target_modules=(
            r".*language_model.*\."
            r"(q_proj|k_proj|v_proj|o_proj|gate_proj|up_proj|down_proj)"
        ),
    )
    train_dataset = Dataset.from_list([example for example, _stats in fitted_train])
    eval_dataset = Dataset.from_list([example for example, _stats in fitted_holdout])
    training_args = SFTConfig(
        output_dir=str(output / "checkpoints"),
        max_length=args.max_length,
        completion_only_loss=True,
        num_train_epochs=args.epochs,
        per_device_train_batch_size=1,
        per_device_eval_batch_size=1,
        gradient_accumulation_steps=4,
        learning_rate=args.learning_rate,
        warmup_steps=2,
        lr_scheduler_type="cosine",
        optim="paged_adamw_8bit",
        bf16=True,
        fp16=False,
        gradient_checkpointing=True,
        gradient_checkpointing_kwargs={"use_reentrant": False},
        eval_strategy="epoch",
        save_strategy="epoch",
        save_total_limit=2,
        load_best_model_at_end=True,
        metric_for_best_model="eval_loss",
        greater_is_better=False,
        logging_steps=1,
        report_to="none",
        seed=args.seed,
        data_seed=args.seed,
        packing=False,
    )
    trainer = SFTTrainer(
        model=model,
        args=training_args,
        train_dataset=train_dataset,
        eval_dataset=eval_dataset,
        processing_class=processor,
        peft_config=lora,
    )
    trainer.model.print_trainable_parameters()
    torch.cuda.empty_cache()
    torch.cuda.reset_peak_memory_stats()
    try:
        result = trainer.train()
    except Exception as error:
        failure_path = output / "failure.json"
        failure = {
            "error_type": type(error).__name__,
            "error": str(error),
            "max_cuda_memory_allocated_bytes": torch.cuda.max_memory_allocated(),
            "traceback": traceback.format_exc(),
        }
        failure_path.write_text(
            json.dumps(failure, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        os.chmod(failure_path, 0o600)
        raise
    adapter_dir = output / "adapter"
    trainer.save_model(str(adapter_dir))
    processor.save_pretrained(str(adapter_dir))
    metrics = dict(result.metrics)
    metrics.update(trainer.evaluate())
    metrics["max_cuda_memory_allocated_bytes"] = torch.cuda.max_memory_allocated()
    metrics_path = output / "metrics.json"
    metrics_path.write_text(
        json.dumps(metrics, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    os.chmod(metrics_path, 0o600)
    print(json.dumps(metrics, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
