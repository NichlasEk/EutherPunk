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

## Useful Environment

```text
EUTHERPUNK_ADDR=:8787
EUTHERPUNK_URL=http://127.0.0.1:8787
EUTHERPUNK_MODEL=qwen3-coder:30b
EUTHERPUNK_CONFIG=/home/nichlas/.config/eutherpunk/config.toml
OLLAMA_URL=http://127.0.0.1:11434
```

## Server Direction

EutherOxide should expose EutherPunk through authenticated routes and downloads, while Ollama remains internal. Public traffic should go through EutherOxide or another trusted gateway, not directly to Ollama.
