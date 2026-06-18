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
