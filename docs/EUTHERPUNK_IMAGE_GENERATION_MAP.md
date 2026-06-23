# EutherPunk Image Generation Map

This is the recovery map for EutherPunk chat image generation.

## Runtime Path

1. Browser chat sends `/bild <prompt>` to EutherPunk.
2. EutherPunk server receives `POST /api/eutherpunk/images/generate`.
3. EutherPunk returns a short-lived in-memory image job id to the browser.
4. The browser polls `/api/eutherpunk/images/jobs/<job_id>` instead of holding a long fetch open.
5. The background EutherPunk job posts a temporary image workflow to ComfyUI at `http://192.168.32.88:8188/prompt`.
6. ComfyUI generates the image and exposes it through its `/history/{prompt_id}` and `/view` endpoints.
7. EutherPunk downloads the PNG and stores it under `/home/nichlas/EutherPunk/var/images/<user>/`.
8. Browser displays the stored server URL: `/api/eutherpunk/images/<user>/<file>.png`.

The permanent copy is owned by EutherPunk, not ComfyUI. ComfyUI is only the generator.

When the user attaches an image and asks for `/bild` or the chat emits an
`EUTHERPUNK_IMAGE_PROMPT`, the browser sends the first attached image as
`source_image`. For SenseNova models, EutherPunk uploads that source image to
ComfyUI `/upload/image`, adds a `LoadImage` node, and passes it into
`SenseNova_SM_Sampler` as the optional `image` input. Z-Image remains
text-to-image only.

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

## SenseNova U1 Assets

SenseNova is optional and selectable per user in the EutherPunk settings dialog. Z-Image remains the default image model.

Available EutherPunk SenseNova profiles:

- `sensenova-u1-8b-fast`: uses ComfyUI `interleave` mode without LoRA. For `1:1`
  this targets the node's 1536 x 1536 bucket and is the preferred chat-speed
  SenseNova profile while Dots TTS stays warm.
- `sensenova-u1-8b`: uses ComfyUI T2I/edit path and allows the SenseNova LoRA.
  For `1:1` this targets 2048 x 2048 and can take several minutes on the shared
  workstation.

ComfyUI custom node:

```sh
/home/nichlas/ai/ComfyUI/custom_nodes/ComfyUI_SenseNova_U1
```

Required model files:

- `/home/nichlas/ai/ComfyUI/models/gguf/SenseNova-U1-8B-MoT-8step-Q4_K_S.gguf`
- `/home/nichlas/ai/ComfyUI/models/loras/SenseNova-U1-8B-MoT-LoRA-8step-V1.0.safetensors`

The EutherPunk workflow uses these ComfyUI node classes:

- `SenseNova_SM_Model`
- `SenseNova_SM_Sampler`
- `PreviewImage`

The SenseNova sampler is configured conservatively for the shared workstation:

- `batch_size = 1`
- `prefetch_count = 1`
- `max_new_tokens = 256`
- `steps = 4` unless overridden by config/request
- output target is selected from `1:1`, `16:9`, or `9:16` based on requested dimensions

Before a SenseNova job is queued, EutherPunk asks EutherLink to release voice
resources through:

- `POST /v1/resources/heavy-tts/suspend`
- `POST /v1/resources/voxcpm2/unload`

GrapheneOS Matcha stays available for lightweight server voice, but heavy local
TTS models are temporarily suspended so SenseNova can use the GPU instead of
competing with loaded speech models. During the suspension window, new Dots TTS
requests can fall back to `grapheneos-matcha-en` through EutherLink instead of
failing hard or immediately rewarming Dots.

If Dots is already rendering an active job, EutherLink returns busy instead of
terminating it. EutherPunk keeps the SenseNova image job in `waiting_tts` and
retries the suspend request until Dots finishes, then starts the ComfyUI image
generation.

The workstation also has a Btrfs-compatible persistent swap file:

- `/swapfile-eutherpunk`, 32 GiB
- `/etc/fstab`: `/swapfile-eutherpunk none swap defaults,pri=10 0 0`

If the node rejects the workflow, verify node registration after a ComfyUI restart:

```sh
curl -fsS http://192.168.32.88:8188/object_info/SenseNova_SM_Model
curl -fsS http://192.168.32.88:8188/object_info/SenseNova_SM_Sampler
```

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

Per-user model settings are stored as TOML under the configured settings directory. By default this lives next to the image directory:

```text
/home/nichlas/EutherPunk/var/settings/<user>.toml
```

Currently supported keys:

```toml
chat_model = "qwen3-coder:30b"
vision_model = "moondream:latest"
image_model = "z-image-turbo"
image_lora = "none"
voice_backend = "grapheneos-matcha-en"
tts_enabled = false
server_voice_enabled = false
```

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

Expected result: JSON with `job_id` and `status`. Poll `/api/eutherpunk/images/jobs/<job_id>` until it returns `status = done` and an `image` object with `url`, `user`, `filename`, and `prompt_id`. The file should exist under `/home/nichlas/EutherPunk/var/images/<user>/` on the server.

Async image jobs wait up to at least 15 minutes server-side. This keeps browser
fetch/proxy drops from cancelling ComfyUI jobs and allows a short Z-Image job to
survive sitting behind a slower SenseNova job in the ComfyUI queue.

## Common Failures

- `connection refused` to `127.0.0.1:11434` on server: local Ollama reverse tunnel is down.
- `connection refused` to `192.168.32.88:8188`: ComfyUI is down or bound to the wrong address.
- `connection refused` after a long SenseNova wait: check `journalctl --user -u comfyui.service`; OOM-kill means RAM/swap/resource release failed before or during the job.
- Browser `NetworkError when attempting to fetch resource` during image generation: verify that the browser is using the async image job API and polling `/api/eutherpunk/images/jobs/<job_id>`. Long direct fetches can be dropped by the browser, LAN, or public reverse proxy while ComfyUI continues running.
- `EnvironmentNameNotFound: comfyui`: restore the `.venv` based ComfyUI service.
- ComfyUI `node_errors` for Z-Image: verify `/object_info` still lists `UNETLoader`, `CLIPLoader`, `VAELoader`, `EmptySD3LatentImage`, `KSampler`, and `PreviewImage`.
- ComfyUI `node_errors` for SenseNova: verify `/object_info` still lists `SenseNova_SM_Model` and `SenseNova_SM_Sampler`, and that the GGUF and LoRA files exist in the paths above.
- Browser still runs old JS: verify `index.html` cache-bust query changed and hard-refresh the page.
- Deploy fails with `Permission denied (publickey)`: unlock `/home/nichlas/.ssh/euther_server` in an `ssh-agent` before running `scripts/deploy-server.sh`.
