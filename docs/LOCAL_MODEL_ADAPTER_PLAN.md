# EutherPunk local coding adapter plan

## Objective

Improve execution fidelity in the local coding worker, especially cases where
the model can describe a compiler or reviewer diagnostic but returns unchanged
or still-invalid source.

The target is not a more talkative model. The target is a model that:

1. changes the diagnosed file;
2. preserves unrelated files and working APIs;
3. produces complete structured file content;
4. passes the same executable verifier that produced the diagnostic.

Devstral Small 2 24B is published under Apache 2.0 and can be modified. The
official weights are available from:

<https://huggingface.co/mistralai/Devstral-Small-2-24B-Instruct-2512>

The current Ollama package is useful for inference and evaluation. Adapter
training should start from training-compatible official weights rather than the
quantized Ollama blob.

## Phase 1: collect private traces

Use `eutherpunk worker --output ...` for the original job and retain its full
`drafts` history. After a separate verifier and parent agent have corrected the
workspace, finalize the example:

```bash
eutherpunk trace finalize \
  --result /tmp/worker-result.json \
  --workspace /tmp/verified-workspace \
  --diagnostics /tmp/verification.txt \
  --verdict accepted \
  --output training/traces/example.json
```

Each accepted trace contains:

- bounded task and worker role;
- model and job provenance;
- every structured model draft;
- independent review issues and activities;
- actual verifier diagnostics;
- corrected complete target files;
- hashes binding the trace to its source result.

Trace files contain source code. They remain local, use private file
permissions, are ignored by Git, and must be inspected for secrets and licensing
before any dataset export.

Rejected examples can also be retained, but they are preference/evaluation
data rather than supervised target examples.

## Phase 2: build a frozen evaluation set

Before training, create deterministic tasks covering:

- a one-file compiler repair;
- a cross-file API mismatch;
- preservation of an unrelated file;
- path and secret-safety constraints;
- incomplete placeholder removal;
- a small HTML/JavaScript runtime repair;
- a Go, Rust, Lua and JavaScript implementation task.

Record baseline results for the unmodified model:

- first-pass acceptance rate;
- executable test pass rate;
- diagnosed-file change rate;
- unchanged-file preservation rate;
- repair rounds and wall time;
- invalid or withheld proposal rate.

Keep evaluation tasks out of training data.

## Phase 3: adapter pilot

Start with LoRA or QLoRA rather than a full 24B fine-tune. A useful pilot needs
more than one anecdote: collect a small clean corpus first, reserve a holdout,
then audit the available GPU memory before selecting rank, quantization,
sequence length and batch strategy.

Training examples should emphasize the failing transition:

```text
task + current complete files + verified diagnostic
    -> corrected complete diagnosed files
```

Do not train private chain-of-thought. Use observable tasks, drafts,
diagnostics, patches and verifier outcomes.

## Phase 4: package and A/B test

Keep the base model unchanged and version the adapter independently. Evaluate
base and adapter through the same EutherPunk harness and frozen tasks. Promote
the adapter only when executable acceptance improves without worse path safety,
secret handling or unrelated-file preservation.

If accepted:

1. save the adapter and training manifest;
2. optionally merge it into a compatible checkpoint;
3. quantize an inference artifact;
4. create a separately named Ollama model, for example
   `eutherpunk-devstral-repair:v1`;
5. retain the original Devstral model as an immediate rollback and A/B control.

## Current first trace

The PunkScout experiment produced the first useful example: Devstral repeated
six concrete Go compiler diagnostics over several revisions without correcting
the corresponding source. The parent agent corrected the file and verified
`go test`, race tests, `go vet`, a build and a practical privacy smoke test.

The local trace was finalized as schema version 1. It is intentionally not
committed because it contains generated and corrected source.

## First frozen baseline

The versioned `evaluation/v1/suite.json` starts with three small Go cases:

- repair of an undefined production helper;
- repair of a cross-file public API mismatch;
- removal of a functional placeholder with repeated-separator behavior.

Tests and selected module files are marked as preserved inputs. The evaluator
runs each job in an isolated workspace, invokes `go test ./...` without a shell,
checks the protected hashes and finalizes a private trace automatically.

The first live run against `devstral-small-2:24b` on 2026-07-29 produced:

- executable pass rate: 3/3;
- protected-file preservation: 3/3;
- harness completion: 2/3;
- mean wall time: 18.374 seconds.

The third workspace passed its executable tests but the model reviewer still
withheld it after inventing Unicode, extra separator and additional-test
requirements. This is evidence of reviewer overreach rather than a coding
failure. The review contract now explicitly rejects invented requirements.

A follow-up calibration run showed the inverse failure: the calibrated reviewer
accepted a slug implementation that left leading and trailing hyphens, while
the executable test rejected it. The evaluator therefore now performs up to two
bounded diagnostic-driven repair rounds and reruns the same verifier after each
draft. Both the original failure and later verification outcomes remain in the
training trace.

Future baselines must use a new named output directory and record the exact
harness commit so results remain comparable.

## Multilanguage v2 baseline

The immutable `evaluation/v2/suite.json` retained the three v1 Go cases and
added:

- numeric JavaScript median behavior under `node --test`;
- a Lua FIFO module under `lua test.lua`;
- Rust sorting and deduplication under `cargo test --quiet`;
- a two-production-file Go repair with protected tests and module metadata.

The first v2 run used harness `4fe4a63`, suite SHA-256
`189c15abcd0f9b2a305867e1cc35f428e9f8ee0aac67b184ff56ff8a833fe63b`
and `devstral-small-2:24b`. Results:

- executable pass rate: 7/7;
- protected-file preservation: 7/7;
- harness completion: 7/7;
- mean wall time: 15.024 seconds;
- external verifier repair rounds: one, for the slug case.

The Lua case also exercised an internal reviewer-driven draft repair before its
external verifier passed. All seven traces are private mode `0600` artifacts
under the ignored training output tree.

## First repair-dataset pilot

The private dataset builder accepts repeatable trace files/directories and
exports only accepted, diagnosed transitions where an earlier draft differs
from the verified final files. It rejects symlinked input and strong secret
patterns before creating the output directory, then deduplicates examples and
uses deterministic holdout assignment once at least five examples exist.

The first pilot inspected 42 JSON artifacts from the PunkScout, v1, v2 and live
repair output trees:

- accepted traces: 12;
- usable repair transitions: 4;
- duplicates: 0;
- train examples: 4;
- holdout examples: 0 because the pilot is below five examples.

The four transitions are three independently generated slug failures and the
PunkScout Go compiler repair. An automated scan found no strong secret,
copyright or license markers, and the three-message JSONL structure validated.
The manifest deliberately retains:

```json
{
  "manual_license_and_secret_review_required": true,
  "training_authorized": false
}
```

No adapter training should begin from this pilot. Continue collecting varied
diagnosed repairs until there is a meaningful holdout and enough language/task
diversity to measure generalization.

## Verified initial-state provenance

The worker result and trace now retain bounded `initial_files`. Eval cases run
their declared verifier before the model starts and reject a seed that already
passes. Go build cache is kept outside the model workspace; generated Rust
`target/` state remains excluded by the existing workspace rules.

This makes a first-pass model success usable without manufacturing a failure:

```text
verified failing initial files + failing diagnostics + task
    -> externally verified corrected production files
```

A new v2 run produced six accepted initial-to-final transitions and one rejected
slug attempt after both external repair rounds failed. Combining these with the
older diagnosed traces yielded the second private pilot:

- JSON artifacts inspected: 69;
- accepted traces: 18;
- usable repair transitions: 10;
- duplicates: 0;
- train examples: 7;
- deterministic holdout examples: 3.

The rejected slug trace remains useful negative/preference evidence but is not
included in supervised targets. The corpus is now structurally trainable, but
ten transitions still do not justify an adapter run; the next milestone remains
at least 30 diverse repairs.

## Diverse v3 and the 30-transition milestone

The immutable `evaluation/v3/suite.json` contains twenty new dependency-free
repairs: five each for Go, JavaScript, Lua and Rust. Every seed was executed
before model access and confirmed failing. Suite SHA-256:

```text
8cf02649985ed37b10bf210ee76c487ffee0a20e14f96cffaef57a2891ffa529
```

The first run used harness `5406758` and `devstral-small-2:24b`:

- accepted by executable verifier: 19/20;
- protected-file preservation: 20/20;
- harness completion: 16/20;
- mean wall time: 16.124 seconds.

The JavaScript range case was the sole executable rejection. Six model drafts
repeated the same signed-step error. A parent correction changed the loop from
subtracting an already negative step to adding the signed step, after which
`node --test` passed. That accepted parent-verified trace became transition 30.

The final private corpus inspected 156 JSON artifacts and contains:

- repair transitions: 30;
- train examples and groups: 24;
- holdout examples and groups: 6;
- duplicate transitions: 0;
- train/holdout group overlap: 0.

Holdout selection is exact and deterministic by a hash of task, diagnostics and
source files. Multiple valid targets for one identical repair input therefore
cannot leak across the split. Automated structure and strong-secret/license
marker scans passed, but the manifest still has
`manual_license_and_secret_review_required: true` and
`training_authorized: false`.
