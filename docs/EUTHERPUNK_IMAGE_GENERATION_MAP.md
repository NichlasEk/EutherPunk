# EutherPunk Image Generation Map

This is the recovery map for EutherPunk chat image generation.

## Runtime Path

1. Browser chat sends `/bild <prompt>` to EutherPunk.
2. EutherPunk server receives `POST /api/eutherpunk/images/generate`.
3. EutherPunk posts a temporary Z-Image workflow to ComfyUI at `http://192.168.32.88:8188/prompt`.
4. ComfyUI generates the image and exposes it through its `/history/{prompt_id}` and `/view` endpoints.
5. EutherPunk downloads the PNG and stores it under `/home/nichlas/EutherPunk/var/images/<user>/`.
6. Browser displays the stored server URL: `/api/eutherpunk/images/<user>/<file>.png`.

The permanent copy is owned by EutherPunk, not ComfyUI. ComfyUI is only the generator.

## Hosts And Ports

- EutherOxide/EutherPunk gateway: `192.168.32.186:8080`
- EutherPunk daemon on server: `127.0.0.1:8787`
- Ollama tunnel endpoint on server: `127.0.0.1:11434`
- Local ComfyUI: `192.168.32.88:8188`
- Local Ollama source behind reverse tunnel: `127.0.0.1:11434`

## Persistent Services

On this machine:

```sh
systemctl --user status comfyui.service
systemctl --user status eutherpunk-ollama-tunnel.service
systemctl --user status ollama.service
```

On the server:

```sh
ssh -F /home/nichlas/.ssh/config euther-server
systemctl --user status eutherpunkd.service
curl -fsS http://127.0.0.1:8787/api/eutherpunk/status
```

## ComfyUI Service

The working ComfyUI service uses the repo-local virtualenv:

```ini
WorkingDirectory=/home/nichlas/ai/ComfyUI
ExecStart=/home/nichlas/ai/ComfyUI/.venv/bin/python main.py --listen 192.168.32.88 --port 8188
```

If ComfyUI crash-loops with `EnvironmentNameNotFound: Could not find conda environment: comfyui`, the service has regressed to the old conda command. Restore the venv-based `ExecStart`, then run:

```sh
systemctl --user daemon-reload
systemctl --user restart comfyui.service
curl -fsS http://192.168.32.88:8188/system_stats
```

## Z-Image Assets

Required local ComfyUI files:

- `/home/nichlas/ai/ComfyUI/models/diffusion_models/z_image_turbo_bf16.safetensors`
- `/home/nichlas/ai/ComfyUI/models/text_encoders/qwen_3_4b.safetensors`
- `/home/nichlas/ai/ComfyUI/models/vae/ZImag-vae.safetensors`

The EutherPunk workflow uses these ComfyUI node classes:

- `UNETLoader`
- `CLIPLoader` with type `lumina2`
- `VAELoader`
- `CLIPTextEncode`
- `EmptySD3LatentImage`
- `KSampler` with `euler`, `simple`, `cfg=0.7`
- `VAEDecode`
- `PreviewImage`

## EutherPunk Config

Server config lives at `/home/nichlas/.config/eutherpunk/config.toml` and is deployed from `deploy/eutherpunk.server.toml`.

Relevant block:

```toml
[image]
comfyui_url = "http://192.168.32.88:8188"
directory = "/home/nichlas/EutherPunk/var/images"
timeout_seconds = 180
default_width = 1024
default_height = 1024
default_steps = 8
```

Images are stored per user. Until EutherOxide forwards a user header, EutherPunk falls back to the single configured user when there is only one `[users.*]` entry.

Supported future identity headers:

- `X-EutherOxide-User`
- `X-Forwarded-User`
- `X-Remote-User`

## Smoke Tests

From this machine:

```sh
curl -fsS http://192.168.32.88:8188/system_stats
curl -fsS http://192.168.32.186:8080/api/eutherpunk/status
curl -fsS -H 'Content-Type: application/json' \
  -d '{"prompt":"a small red robot holding a coffee mug"}' \
  http://192.168.32.186:8080/api/eutherpunk/images/generate
```

Expected result: JSON with `url`, `user`, `filename`, and `prompt_id`. The file should exist under `/home/nichlas/EutherPunk/var/images/<user>/` on the server.

## Common Failures

- `connection refused` to `127.0.0.1:11434` on server: local Ollama reverse tunnel is down.
- `connection refused` to `192.168.32.88:8188`: ComfyUI is down or bound to the wrong address.
- `EnvironmentNameNotFound: comfyui`: restore the `.venv` based ComfyUI service.
- ComfyUI `node_errors`: verify `/object_info` still lists `UNETLoader`, `CLIPLoader`, `VAELoader`, `EmptySD3LatentImage`, `KSampler`, and `PreviewImage`.
- Browser still runs old JS: verify `index.html` cache-bust query changed and hard-refresh the page.
- Deploy fails with `Permission denied (publickey)`: unlock `/home/nichlas/.ssh/euther_server` in an `ssh-agent` before running `scripts/deploy-server.sh`.
