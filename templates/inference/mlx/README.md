# MLX Templates

`qwen-final-content.jinja` is a conservative Qwen chat template for MLX OpenAI-compatible servers when the model's bundled template starts assistant generation inside `<think>`.

Use it for ContextLattice Dream Mode or `/v1/inference/chat` conformance when the runtime returns `message.reasoning` without final `message.content`.

```bash
scripts/inference_mlx_server.sh --model /path/to/mlx/model --template-profile qwen-final-content
scripts/inference_template_conformance.sh --provider mlx --model /path/to/mlx/model
```
