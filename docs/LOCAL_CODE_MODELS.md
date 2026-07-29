# Local coding model candidates

Last reviewed: 2026-07-29.

EutherPunk should evaluate coding models inside the same iterative agent loop:
generate a complete proposal, perform an independent semantic review, repair
concrete issues, run local syntax/runtime checks, and withhold a proposal that
still fails. A one-shot prompt is not a meaningful coding-agent comparison.

## Recommended order

1. **Devstral Small 2 24B**
   - Official Ollama artifact: `devstral-small-2:24b`, about 15 GB.
   - Apache 2.0.
   - Designed for tool use, repository exploration, multi-file editing, and
     software-engineering agents.
   - Mistral reports 65.8% on SWE-bench Verified for the 24B model.
   - Selected for the live EutherPunk workspace A/B test on 2026-07-29 after
     Qwen again failed the Tetris task behind the revised file harness.

2. **Qwen3-Coder 30B-A3B**
   - Previous EutherPunk workspace model, about 19 GB in Ollama.
   - Open weights with strong official emphasis on agentic coding and tool use.
   - Efficient MoE architecture with 30B total and about 3.3B active parameters.
   - Failed the EutherPunk Tetris acceptance test on 2026-07-28: two repair
     rounds still repeated rotation and persistent-color defects. Keep as a
     baseline, not as the assumed winner.

3. **Mistral Small 4**
   - Apache 2.0 general model combining chat, reasoning, multimodal input, and
     Devstral-derived agentic coding.
   - Interesting later candidate when EutherPunk wants one model for both
     conversation and coding, but Devstral Small 2 is the more focused first
     coding comparison.

4. **Qwen3-Coder-Next**
   - Newer open-weight coding-agent model.
   - The commonly available local variants are substantially larger than the
     current 15–20 GB target, so it is not the first practical test here.

## About unrestricted derivatives

Community "abliterated" versions exist for both Qwen3-Coder 30B and Devstral
Small 2. Their goal is to reduce refusals, but the Qwen derivative's own model
card describes abliteration as a crude proof of concept. Removing refusal
directions can also damage instruction following, tool-call reliability, and
code quality. EutherPunk should therefore benchmark the official permissively
licensed model first, then compare an abliterated derivative using the same
tests if refusal behavior is an actual observed problem.

An unrestricted personality model and a reliable coding worker do not have to
be the same model. EutherPunk can keep an expressive conversational model while
using a more disciplined model for code generation and verification.

## Sources

- Qwen3-Coder overview: https://qwenlm.github.io/blog/qwen3-coder/
- Qwen3-Coder in Ollama: https://ollama.com/library/qwen3-coder
- Qwen Code agent documentation: https://qwenlm.github.io/qwen-code-docs/
- Devstral 2 announcement: https://mistral.ai/news/devstral-2-vibe-cli/
- Devstral Small 2 in Ollama: https://ollama.com/library/devstral-small-2
- Mistral Small 4: https://mistral.ai/news/mistral-small-4/
- Abliterated Qwen3-Coder model card:
  https://huggingface.co/huihui-ai/Huihui-Qwen3-Coder-30B-A3B-Instruct-abliterated
- Abliterated Devstral Small 2 model card:
  https://huggingface.co/wangzhang/Devstral-Small-2-24B-Instruct-abliterated
