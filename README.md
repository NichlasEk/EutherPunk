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
/permissions files off|ask|session
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
exits.

### Local coding workspace

`/workspace init <directory>` creates and selects a new project directory;
`/workspace use <directory>` selects an existing one. The home directory and
filesystem root cannot be selected as a workspace. EutherPunk may snapshot at
most 32 UTF-8 text files (48 KiB total) after an `ask` or `session` approval.
It does not follow symlinks and skips `.git`, dependency/build directories,
binary files, `.env`, credentials, private keys and other likely secret files.

The model can propose complete file contents using a strictly parsed local
protocol. The CLI validates every relative path, shows a preview, and asks
again before writing. Writes are atomic and an overwritten file is preserved
as `<name>.eutherpunk.previous`. Deletion and arbitrary shell execution are not
available in this version. This is enough to create and revise small projects;
the user still runs or builds them separately.

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
