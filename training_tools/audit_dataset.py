#!/usr/bin/env python3
"""Audit and authorize a private EutherPunk repair dataset."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SECRET_PATTERNS = {
    "private_key": re.compile(r"-----BEGIN (?:OPENSSH |RSA |EC )?PRIVATE KEY-----"),
    "github_token": re.compile(r"\bgh[opusr]_[A-Za-z0-9]{30,}\b"),
    "aws_access_key": re.compile(r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b"),
    "generic_secret": re.compile(
        r"(?i)\b(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)"
        r"\s*[:=]\s*[\"']?[A-Za-z0-9_./+=-]{16,}"
    ),
}

LICENSE_PATTERNS = {
    "copyright": re.compile(r"(?i)\bcopyright\b|\(c\)\s*\d{4}"),
    "license": re.compile(
        r"(?i)\b(?:SPDX-License-Identifier|GNU General Public License|"
        r"Apache License|MIT License|Mozilla Public License)\b"
    ),
}


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    examples: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            try:
                value = json.loads(line)
            except json.JSONDecodeError as error:
                raise ValueError(f"{path.name}:{line_number}: invalid JSON: {error}") from error
            if not isinstance(value, dict):
                raise ValueError(f"{path.name}:{line_number}: example must be an object")
            examples.append(value)
    return examples


def inspect_example(example: dict[str, Any], source: str) -> dict[str, Any]:
    messages = example.get("messages")
    if (
        example.get("schema_version") != 1
        or not isinstance(example.get("id"), str)
        or not isinstance(example.get("group_id"), str)
        or not isinstance(messages, list)
        or len(messages) != 3
        or [message.get("role") for message in messages]
        != ["system", "user", "assistant"]
    ):
        raise ValueError(f"{source}: unsupported example structure")

    try:
        request = json.loads(messages[1]["content"])
        answer = json.loads(messages[2]["content"])
    except (KeyError, TypeError, json.JSONDecodeError) as error:
        raise ValueError(f"{source}: invalid structured message content: {error}") from error

    files = answer.get("files")
    current_files = request.get("current_files")
    if not isinstance(request.get("task"), str) or not isinstance(files, list):
        raise ValueError(f"{source}: task and assistant files are required")
    if not isinstance(current_files, list) or not files:
        raise ValueError(f"{source}: current files and corrected files are required")

    current_paths = {
        item.get("path") for item in current_files if isinstance(item, dict)
    }
    corrected_paths: list[str] = []
    for item in files:
        if not isinstance(item, dict) or not isinstance(item.get("path"), str):
            raise ValueError(f"{source}: corrected file lacks a path")
        if item["path"] not in current_paths:
            raise ValueError(f"{source}: corrected path {item['path']!r} is absent from input")
        if not isinstance(item.get("content"), str):
            raise ValueError(f"{source}: corrected file {item['path']!r} lacks content")
        corrected_paths.append(item["path"])

    return {
        "id": example["id"],
        "group_id": example["group_id"],
        "task": request["task"],
        "corrected_paths": corrected_paths,
        "source_model": example.get("source_model", ""),
    }


def audit(dataset_dir: Path) -> dict[str, Any]:
    dataset_dir = dataset_dir.resolve()
    paths = {
        "train": dataset_dir / "train.jsonl",
        "holdout": dataset_dir / "holdout.jsonl",
        "manifest": dataset_dir / "manifest.json",
    }
    for label, path in paths.items():
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"{label} must be a regular non-symlink file: {path}")
        if path.stat().st_mode & 0o077:
            raise ValueError(f"{label} is not private mode 0600: {path}")

    manifest = json.loads(paths["manifest"].read_text(encoding="utf-8"))
    train = load_jsonl(paths["train"])
    holdout = load_jsonl(paths["holdout"])
    inspected = [
        inspect_example(example, f"train:{index}")
        for index, example in enumerate(train, 1)
    ]
    inspected.extend(
        inspect_example(example, f"holdout:{index}")
        for index, example in enumerate(holdout, 1)
    )

    ids = [item["id"] for item in inspected]
    train_groups = {example["group_id"] for example in train}
    holdout_groups = {example["group_id"] for example in holdout}
    if len(ids) != len(set(ids)):
        raise ValueError("duplicate example ids")
    if overlap := train_groups & holdout_groups:
        raise ValueError(f"train/holdout group overlap: {sorted(overlap)}")
    if manifest.get("train_examples") != len(train):
        raise ValueError("manifest train count does not match train.jsonl")
    if manifest.get("holdout_examples") != len(holdout):
        raise ValueError("manifest holdout count does not match holdout.jsonl")

    combined = "\n".join(
        paths[label].read_text(encoding="utf-8") for label in ("train", "holdout")
    )
    secret_hits = [name for name, pattern in SECRET_PATTERNS.items() if pattern.search(combined)]
    license_hits = [name for name, pattern in LICENSE_PATTERNS.items() if pattern.search(combined)]
    if secret_hits:
        raise ValueError(f"possible secrets found: {', '.join(secret_hits)}")
    if license_hits:
        raise ValueError(f"license/copyright markers require review: {', '.join(license_hits)}")

    languages: dict[str, int] = {}
    for item in inspected:
        suffixes = {Path(path).suffix or "(none)" for path in item["corrected_paths"]}
        for suffix in suffixes:
            languages[suffix] = languages.get(suffix, 0) + 1

    return {
        "schema_version": 1,
        "dataset_dir": str(dataset_dir),
        "files": {
            label: {
                "name": path.name,
                "sha256": sha256(path),
                "bytes": path.stat().st_size,
            }
            for label, path in paths.items()
        },
        "examples": {
            "train": len(train),
            "holdout": len(holdout),
            "total": len(inspected),
            "train_groups": len(train_groups),
            "holdout_groups": len(holdout_groups),
            "group_overlap": 0,
        },
        "corrected_file_suffixes": dict(sorted(languages.items())),
        "source_models": sorted({item["source_model"] for item in inspected}),
        "automated_review": {
            "schema_valid": True,
            "private_permissions": True,
            "unique_examples": True,
            "secret_pattern_hits": [],
            "license_marker_hits": [],
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("dataset", type=Path)
    parser.add_argument("--authorize", action="store_true")
    parser.add_argument("--reviewer", default="")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()

    try:
        report = audit(args.dataset)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"audit failed: {error}", file=sys.stderr)
        return 1

    if args.authorize:
        reviewer = args.reviewer.strip()
        if not reviewer:
            print("--reviewer is required with --authorize", file=sys.stderr)
            return 2
        report["authorization"] = {
            "training_authorized": True,
            "reviewer": reviewer,
            "reviewed_at": datetime.now(timezone.utc).isoformat(),
            "scope": "private local QLoRA pilot only; no upload or publication",
            "manual_review": {
                "tasks_and_targets_reviewed": True,
                "generated_or_project_owned_content_only": True,
                "secrets_reviewed": True,
                "license_reviewed": True,
            },
        }

    output = args.output or args.dataset / "authorization.json"
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        print(f"refusing to overwrite existing audit: {output}", file=sys.stderr)
        return 2
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
