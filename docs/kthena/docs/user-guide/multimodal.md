# Multimodal Requests

The Kthena Router serves multimodal models, such as vision-language models, through the same OpenAI-compatible endpoints as text models. Clients send images, audio or video as content parts of a chat request, and the router forwards them to the model server unchanged. No router configuration is needed.

The router recognizes the media in a request so that scheduling can take it into account: the [`prefix-cache`](./prefix-cache) plugin only matches requests that share the same media, and the [`kvcache-aware`](./kvcache-aware) plugin leaves multimodal requests to the other score plugins.

## Supported Inputs

The router recognizes the following content parts. Other parts are forwarded as well, but the router does not treat them as media.

| Content part type | API                                              | Modality |
| ----------------- | ------------------------------------------------ | -------- |
| `image_url`       | OpenAI Chat Completions (`/v1/chat/completions`) | image    |
| `input_audio`     | OpenAI Chat Completions                          | audio    |
| `audio_url`       | vLLM and SGLang extension of Chat Completions    | audio    |
| `video_url`       | vLLM and SGLang extension of Chat Completions    | video    |
| `input_image`     | OpenAI Responses (`/v1/responses`)               | image    |
| `image`           | Anthropic Messages (`/v1/messages`)              | image    |

Media can be passed by reference (an `http(s)://` URL, or a `file_id`) or inline (a base64 `data:` URI, or base64 data). Which of them work depends on the model server: for example, vLLM fetches URLs itself, so the model pods need network access to them.

## Deploy a Vision-Language Model

The example below serves `Qwen/Qwen2.5-VL-3B-Instruct` with vLLM on two GPU pods, and exposes it through the router as `qwen-vl`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: qwen-vl
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: qwen-vl
  template:
    metadata:
      labels:
        app: qwen-vl
    spec:
      containers:
        - name: vllm
          image: vllm/vllm-openai:latest
          command: ["vllm", "serve"]
          args:
            - Qwen/Qwen2.5-VL-3B-Instruct
            - --port=8000
            - --max-model-len=8192
            - --limit-mm-per-prompt={"image":4}
          ports:
            - containerPort: 8000
          resources:
            limits:
              nvidia.com/gpu: 1
          readinessProbe:
            httpGet:
              path: /health
              port: 8000
            periodSeconds: 5
          volumeMounts:
            - name: shm
              mountPath: /dev/shm
      volumes:
        - name: shm
          emptyDir:
            medium: Memory
            sizeLimit: 2Gi
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: qwen-vl
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      app: qwen-vl
  workloadPort:
    port: 8000
  model: Qwen/Qwen2.5-VL-3B-Instruct
  inferenceEngine: vLLM
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: qwen-vl
  namespace: default
spec:
  modelName: qwen-vl
  rules:
    - name: default
      targetModels:
        - modelServerName: qwen-vl
```

`spec.model` of the ModelServer is the model name vLLM serves. The router replaces the `model` of each request (`qwen-vl`) with it before forwarding.

## Send Multimodal Requests

Get the address of the router:

```bash
ROUTER_IP=$(kubectl get service kthena-router -n kthena-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
# Or, without a LoadBalancer:
# kubectl port-forward -n kthena-system service/kthena-router 8080:80
# ROUTER_IP=localhost:8080
```

### Image URL

Replace `<IMAGE_URL>` with the URL of an image that the model pods can download:

```bash
curl http://$ROUTER_IP/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-vl",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "image_url", "image_url": {"url": "<IMAGE_URL>"}},
        {"type": "text", "text": "What is in this image?"}
      ]
    }],
    "max_tokens": 128
  }'
```

### Inline Image

Embed a local image as a base64 `data:` URI. The example builds the request body with `jq` and passes it to `curl` on standard input, because a large image does not fit in a single command-line argument:

```bash
base64 -w0 cat.jpg > cat.b64
jq -n --rawfile img cat.b64 '{
  model: "qwen-vl",
  messages: [{
    role: "user",
    content: [
      {type: "image_url", image_url: {url: ("data:image/jpeg;base64," + $img)}},
      {type: "text", text: "What is in this image?"}
    ]
  }],
  max_tokens: 128
}' | curl http://$ROUTER_IP/v1/chat/completions -H "Content-Type: application/json" -d @-
```

Streaming (`"stream": true`), several images per request, and multi-turn conversations with images work the same way as for text requests.

## How Scheduling Treats Media

The router never downloads or decodes media. For each media part it records the modality and a hash of what the client sent: the URL for media passed by reference, or the base64 data for inline media. The hash identifies the media item across requests and router replicas.

### Prefix Cache

The [`prefix-cache`](./prefix-cache) plugin, enabled by default in the Helm chart, renders each media item as a placeholder that holds its hash, at the position of the media part in its message:

```text
<|im_start|>user
<|image:9f86d081884c7d65|>What is in this image?<|im_end|>
```

As a result:

- Requests with the same media and the same conversation before it are routed to the pod that recently served them, where vLLM can reuse the KV cache and the encoder output of the media.
- Requests that ask the same question about different media no longer match each other.

Because the hash is taken from what the client sent, the same image sent once as a URL and once as base64 counts as two different items, and a URL whose content changes counts as the same item. To get the most out of the cache, send repeated media in the same form.

### KV Cache Aware

vLLM includes the hash of each media item in the hashes of the KV blocks that hold its tokens, and SGLang replaces the tokens of each media item with pad token IDs derived from its hash. The router cannot compute these cache keys from text, so the [`kvcache-aware`](./kvcache-aware) plugin does not score requests that contain media. They are scheduled by the other score plugins, which is why `kvcache-aware` must always be enabled together with another score plugin such as `least-request`.

### Other Plugins

Load-based plugins (`least-request`, `least-latency`, `gpu-usage`), [session sticky](./session-sticky), [fairness scheduling](./fairness-scheduling) and [Prefill-Decode Disaggregation](./prefill-decode-disaggregation/) handle multimodal requests like text requests. With Prefill-Decode Disaggregation, the request is forwarded with its media to the prefill and the decode pods; whether this works depends on the multimodal support of the engine's KV connector.

## Verify Routing

When the router runs with `--v=4`, the scheduler logs every score plugin's result for each request. If `kvcache-aware` is enabled, a multimodal request also logs:

```text
KVCacheAware.Score: early return — multimodal request is not scored (model="Qwen/Qwen2.5-VL-3B-Instruct")
```

To check `prefix-cache` affinity, send the same image request several times and compare the selected pod in the router access log; then send the same question with a different image. You can also watch `kthena_router_prefix_cache_match_ratio` on the router metrics endpoint, as described in [Verifying the Plugin is Active](./prefix-cache#verifying-the-plugin-is-active).

## Limitations

- **Media does not count toward input tokens.** The router estimates input tokens from the text of the request only. [Rate limits](./rate-limit) on input tokens and the input token metrics therefore undercount multimodal requests. Output tokens, and the token usage that fairness scheduling records from the engine response, are not affected.
- **No encoder-cache-aware routing.** Requests that share media but not the conversation before it are not routed together.
- **No encode disaggregation.** Media is encoded on the pod that serves the request.
- **No media validation.** The router forwards media as it is. Unsupported media, unreachable URLs and requests over the model's media limit (`--limit-mm-per-prompt` in vLLM) are rejected by the model server.
