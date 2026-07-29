# EutherPunk

EutherPunk is a local-first AI agent layer for chat, coding help, configuration work, and later mobile voice interaction through EutherOxide.

The first runtime target is the existing Ollama setup under:

```text
/home/nichlas/ai/llm
```

Default model:

```text
qwen3-coder:30b
```

EutherPunk should be treated as the agent and product layer above one or more local models. The model can change later without changing the user-facing identity.

## Current Components

- `cmd/eutherpunkd`: local API service that proxies chat to Ollama.
- `cmd/eutherpunk`: CLI client for status checks and prompts.
- `cmd/eutherpunkd/web`: thin browser client for text chat, TTS, and voice input.
- `config/eutherpunk.example.toml`: TOML config shape.
- `docs/EUTHERPUNK_PLAN.md`: full project plan.

## Config

Persistent config is TOML. JSON is still used for HTTP request/response bodies where it fits naturally.

Default config path:

```text
~/.config/eutherpunk/config.toml
```

Override it with:

```bash
EUTHERPUNK_CONFIG=/path/to/eutherpunk.toml
```

Copy the example to get started:

```bash
mkdir -p ~/.config/eutherpunk
cp config/eutherpunk.example.toml ~/.config/eutherpunk/config.toml
```

Users are configured in TOML and mapped to EutherOxide identities:

```toml
[users.nichlas]
eutheroxide_id = "nichlas"
eutheroxide_username = "nichlas"
model = "qwen3-coder:30b"
safe_mode = true
```

The current local user list is exposed through:

```text
GET /api/eutherpunk/users
```

## Run Locally

Start Ollama first, for example:

```bash
/home/nichlas/ai/llm/bin/start-ollama-agent.sh
```

Start EutherPunk API:

```bash
go run ./cmd/eutherpunkd
```

Open the web client:

```text
http://127.0.0.1:8787/eutherpunk
```

Check status:

```bash
go run ./cmd/eutherpunk status
```

List configured users:

```bash
go run ./cmd/eutherpunk users
```

Ask the model:

```bash
go run ./cmd/eutherpunk ask "sammanfatta vad EutherPunk ska bli"
```

Stream an answer in the terminal:

```bash
go run ./cmd/eutherpunk chat "vad kan du hjälpa mig med?"
```

Start the interactive assistant:

```bash
go run ./cmd/eutherpunk
# Same mode, with an explicit name:
go run ./cmd/eutherpunk assist
```

The portable CLI remains deliberately limited. It does not inspect files
outside an explicitly selected workspace, execute local commands, install
software, or request administrator access. The system tool can collect a small
report after approval; it does not collect IP addresses, serial numbers, or
machine IDs.

Interactive preview commands:

```text
/permissions
/permissions system off|ask|session
/permissions files off|ask|session|auto
/workspace
/workspace init <directory>
/workspace use <directory>
/memory
/memory on|off
/memory show|path|reload
/settings
/settings init|show|path|reload|save
/auth
/auth login
/logout
/system
/system share
/system share full
/clear
/status
/exit
```

While typing a slash command in an interactive terminal, the closest available
completion is shown as dim gift-green text. Press Up Arrow or Tab to accept it.
When no completion is visible, Up Arrow recalls command history.

`/system` keeps the complete report in the terminal. `/system share` sends a
privacy-filtered report to the selected model as chat context; hostname,
username, and working directory are masked by default. `/system share full`
shows the exact full report and requires an extra confirmation before sending
those identifying fields. A `session` permission grant resets when the process
exits. `auto` is the default for workspace files: structured checkpoints are
written automatically, but only inside the explicitly selected workspace. It
does not grant shell or administrator access.

### Local coding workspace

When `eutherpunk` starts in a project directory, it offers to initialize the
current directory as the workspace for that session. Answering no keeps the CLI
in chat-only mode. If `.eutherpunk` already exists, it instead offers to resume
the known workspace and defaults to yes.
When `luac` or Node.js is installed, proposed Lua, JavaScript, and inline HTML
scripts are syntax-checked before the approval prompt; invalid proposals are
rejected without writing anything.

`/workspace init <directory>` creates and selects a new project directory;
`/workspace use <directory>` selects an existing one. The home directory and
filesystem root cannot be selected as a workspace. EutherPunk may snapshot at
most 32 UTF-8 text files (48 KiB total) after an `ask`, `session`, or `auto`
grant.
It does not follow symlinks and skips `.git`, dependency/build directories,
binary files, `.env`, credentials, private keys and other likely secret files.

The model proposes complete file contents using a strictly parsed local
protocol. The CLI validates every relative path. In `ask` mode it shows a
preview and asks before writing; in `session` mode approved files are written
for the rest of that process; in `auto` mode validated draft checkpoints are
written immediately so later validation and repair passes iterate on real
files. Writes are atomic and the original overwritten file is preserved as
`<name>.eutherpunk.previous`. Deletion and arbitrary shell execution are not
available in this version. The user still runs or builds projects separately.

In `auto` mode the harness maintains durable project memory under
`.eutherpunk/`: `project.md` holds the long-lived goal and rules, `state.json`
records the latest task, model, checkpoint, files and validation state, and
`journal.jsonl` keeps a bounded event history. This context is read before
source files on every workspace request, including after restarting the CLI.
The directory is harness-owned: model proposals cannot overwrite it, symlinks
are rejected, files are private, and status updates are atomic.

Workspace generation uses an authenticated asynchronous job protocol. Starting
a job returns the `du>` prompt immediately so normal conversation can continue
while one coding job works in the background. `/job` polls its activity,
`/job wait` follows it in the foreground, `/job open` reviews a completed
proposal, and `/job cancel` stops it. Escape twice while waiting also sends a
server-side cancellation. Completed, failed, cancelled, and expired jobs
therefore have explicit outcomes instead of depending on one long proxy request.
While a coding job exists, conversational turns reuse its already-loaded model
to avoid a long Ollama model swap. Normal chat returns to the configured
conversational model after the proposal is opened or the job is cancelled.

### Non-interactive local worker

`eutherpunk worker` exposes the bounded workspace harness to parent agents and
scripts without an interactive terminal. The first worker role is
`implementer`. It requires an existing EutherID login and never opens a browser,
asks for permissions, runs shell commands, merges Git branches, or pushes.

Proposal-only mode is the safe default:

```bash
eutherpunk worker \
  --workspace /tmp/eutherpunk-task \
  --task "Implement the parser described in TASK.md"
```

Progress is written to stderr. Stdout contains one JSON object with schema
version, status, task, role, workspace, job/model provenance, activities,
issues, checkpoint revision, and complete proposed files with sizes and
SHA-256 hashes. An optional durable copy can be requested with
`--output /path/to/result.json`.

Writing requires the explicit `--apply` flag:

```bash
eutherpunk worker \
  --workspace /tmp/eutherpunk-task \
  --task "Implement the parser described in TASK.md" \
  --apply \
  --output /tmp/eutherpunk-task-result.json
```

With `--apply`, structured draft checkpoints are written only inside the
selected workspace, originals are preserved as `.eutherpunk.previous`, and the
project memory is updated. Without it, neither project files nor project memory
are modified. Result statuses are `completed`, `needs_review`, `no_change`,
`failed`, or `cancelled`. A timeout can be selected with `--timeout`, up to
30 minutes.

Worker JSON includes the complete bounded revision history under `drafts`.
After an external verifier has checked and, when necessary, corrected the
workspace, the result can be converted into a private adapter-training trace:

```bash
eutherpunk trace finalize \
  --result /tmp/worker-result.json \
  --workspace /tmp/eutherpunk-task \
  --diagnostics /tmp/verification.txt \
  --verdict accepted \
  --output /tmp/training-trace.json
```

The trace binds the original task, model/job provenance, every model draft,
review issues, verifier diagnostics, and the current corrected target files.
It omits the absolute workspace path, uses an atomic private output file, never
uploads anything, and rejects symlinked inputs or a result from another
workspace. Training traces contain source code and must still be treated as
sensitive local data; inspect them before adding them to any dataset or Git
repository.

### Frozen local-model evaluation

The repository contains immutable versioned suites. V1 is the original
three-case Go baseline; V2 adds JavaScript, Lua, Rust and a two-file Go repair.
Run the current multilang suite from a clean output path:

```bash
eutherpunk eval run \
  --suite evaluation/v2/suite.json \
  --output training/outputs/devstral-multilang-v2
```

Use `--case go-compiler-repair` for one case. Every case gets an isolated
workspace, the full worker result, bounded verifier diagnostics and an automatic
accepted/rejected training trace. `summary.json` reports executable pass rate,
protected-file preservation, harness completion and timing. It also records the
suite SHA-256 and CLI version so later A/B runs remain attributable.

If the executable verifier fails an otherwise completed proposal, the evaluator
sends the bounded real diagnostics back to the existing server job, applies the
new draft and verifies it again. This loop is limited to two rounds. The worker
result, trace and diagnostics retain all revisions and verification rounds
instead of replacing the failed evidence.

Evaluation suites are trusted developer inputs. Verifiers are invoked directly
without a shell and are restricted to `go`, `node`, `cargo`, `lua` and `luac`.
The bundled suites use only dependency-free local projects. Results and traces
remain private under ignored `training/` directories. Never edit a suite after
recording a baseline; add a new version instead.

### Private repair dataset

Build a deduplicated JSONL pilot from one or more trace files/directories:

```bash
eutherpunk dataset build \
  --input training/traces \
  --input training/outputs/devstral-multilang-v2 \
  --output training/outputs/repair-dataset-pilot
```

Only accepted traces with a differing earlier draft and concrete repair
evidence are exported. Directly successful generations are intentionally
excluded. The builder rejects symlinks and strong private-key/token/assigned
secret patterns, removes duplicate transitions and writes private
`train.jsonl`, `holdout.jsonl` and `manifest.json` files.

Small pilots with fewer than five examples remain entirely in `train.jsonl`;
larger datasets use a deterministic ID-based holdout split. Every manifest sets
`training_authorized` to false and requires manual license and secret review.
Building a dataset is evidence preparation, not permission to train or upload
its source.

The status response also contains a bounded activity log. The CLI shows real
pipeline events such as context preparation, model generation volume, structured
format validation, and local proposal validation. It deliberately does not
expose private model reasoning or stream unvalidated partial source code.
Before a proposal becomes available, a separate adversarial review pass checks
its behavior against the request. Concrete defects are fed back into up to two
repair rounds. Repairs target the files named by the diagnostics directly,
without asking the model to create a new file plan, while unaffected files are
preserved. A proposal that still fails review is withheld instead of being
presented as usable code.
The server may use a dedicated coding model through
`EUTHERPUNK_WORKSPACE_MODEL`; it defaults to the server agent model while normal
chat keeps the user's selected conversational model.

```text
POST   /api/eutherpunk/workspace/jobs
GET    /api/eutherpunk/workspace/jobs/{id}
DELETE /api/eutherpunk/workspace/jobs/{id}
```

Long-term memory is opt-in and stored next to the CLI config as `memory.md`.
`/memory on` creates a human-readable template and a small enable marker, so
the memory is loaded again at the next start. `/memory off` removes only the
marker and preserves the Markdown file. The preview never writes memories on
the model's initiative, caps the file at 32 KiB, and sends it as user-level
background context rather than as a system instruction or tool authorization.
Do not store passwords, tokens, private keys, or other secrets in the file.

`/settings init` creates a human-readable `settings.toml` beside `memory.md`.
It can persist the endpoint, model, safe `off`/`ask` system permission, memory
state, privacy filters, autocomplete keys, history, and ghost-text color.
Environment variables still take precedence over the file. Use `/settings
show` to inspect the active format, edit the file in a text editor, then use
`/settings reload`. A successful save keeps the prior file as
`settings.toml.previous`. The settings file deliberately has no password or
token field.

Example privacy and terminal settings:

```toml
[privacy]
share_hostname = false
share_username = false
share_working_directory = false

[terminal]
autocomplete = true
accept_up_arrow = true
accept_tab = true
history = true
ghost_color = "#5cff5c"
```

## EutherID authentication

The CLI uses a browser/device flow. On first start it creates a short-lived
PKCE-protected request, opens `apothictech.se`, and waits for an explicit
approval from a browser session that was verified with EutherID. No EutherID
password, internal EutherID token, or server key is entered into the CLI.

The issued CLI token is limited to `eutherpunk:chat`: it cannot administer
EutherPunk, change server settings, access stored web conversations, generate
media, read local files, or execute commands. Access tokens last one hour and
the rotating refresh token lasts at most 30 days. `/logout` revokes the whole
token family.

On Windows the credential is stored in Windows Credential Manager. Other
platforms use a separate mode-`0600` credential file beside the config for
development. Credentials are never written to `settings.toml` or `memory.md`;
the server persists only SHA-256 token hashes. See
`docs/CLI_AUTH_DESIGN.md` for the protocol and threat boundaries.

The web app uses its existing EutherOxide browser cookie. Protected browser
requests are accepted only when `/api/app/status` confirms that the session
was created through EutherID; password-only browser sessions do not satisfy
this requirement. Unsafe browser methods also require a same-origin
`Origin`/`Referer`. The status page, CLI download and device-flow bootstrap are
public; chat and all settings, conversation, media and admin APIs are protected.

## Portable CLI Preview

Release builds for both Linux and Windows default to
`https://apothictech.se`, which works from the LAN and remotely. A local
`settings.toml` or `EUTHERPUNK_URL` environment variable can still override
the endpoint.

### Windows

Build only the Windows CLI from Linux:

```bash
GOOS=windows GOARCH=amd64 EUTHERPUNK_CLI_ONLY=1 scripts/build.sh
sha256sum dist/cli/eutherpunk-windows-amd64.exe
```

The resulting `.exe` is portable: it does not need an installer and does not
write configuration on first launch. The release build defaults to the public
HTTPS endpoint and `supergemma4-26b-free:latest`. Either can be overridden in
PowerShell:

```powershell
$env:EUTHERPUNK_URL = "https://apothictech.se"
$env:EUTHERPUNK_MODEL = "supergemma4-26b-free:latest"
.\eutherpunk-windows-amd64.exe
```

## Build CLI Downloads

Build current target:

```bash
scripts/build.sh
```

Build another target:

```bash
GOOS=windows GOARCH=amd64 scripts/build.sh
GOOS=darwin GOARCH=arm64 scripts/build.sh
GOOS=linux GOARCH=arm64 scripts/build.sh
```

The daemon serves CLI binaries from `dist/cli`:

```text
GET /downloads/eutherpunk-cli/linux-amd64
GET /downloads/eutherpunk-cli/windows-amd64
GET /downloads/eutherpunk-cli/darwin-arm64
```

## Useful Environment

```text
EUTHERPUNK_ADDR=:8787
EUTHERPUNK_URL=http://127.0.0.1:8787
EUTHERPUNK_MODEL=qwen3-coder:30b
EUTHERPUNK_WORKSPACE_MODEL=qwen3-coder:30b
EUTHERPUNK_CONFIG=/home/nichlas/.config/eutherpunk/config.toml
OLLAMA_URL=http://127.0.0.1:11434
```

## Server Direction

EutherPunk currently runs as a separate service behind Caddy on the LAN. Ollama remains internal. Public traffic should go through a trusted gateway with auth, not directly to Ollama.

The intended client shape is a thin web/client layer backed by the EutherPunk API:

- Browser text chat through `POST /api/eutherpunk/chat/stream`.
- Browser TTS through `speechSynthesis` first, server-side TTS later.
- Browser voice input through `SpeechRecognition` where supported, server-side STT later.
- CLI downloads through `/downloads/eutherpunk-cli/{platform}`.
- Server voice through `POST /api/eutherpunk/tts`, currently routed to EutherLink `grapheneos-matcha-en`.

Current LAN test URL:

```text
http://192.168.32.186:8080/eutherpunk
```

## Deploy To EutherOxide Host

The deployment target is the LAN server:

```text
192.168.32.186
```

Build and deploy after the SSH key is unlocked:

```bash
scripts/build.sh
scripts/deploy-server.sh
```

The deploy script installs a user-level `eutherpunkd.service` and verifies:

```text
http://127.0.0.1:8787/api/eutherpunk/status
```

Caddy should proxy LAN routes directly to `127.0.0.1:8787`. See [docs/EUTHEROXIDE_INTEGRATION.md](docs/EUTHEROXIDE_INTEGRATION.md).
