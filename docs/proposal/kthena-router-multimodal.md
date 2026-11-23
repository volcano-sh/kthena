---
title: Basic Multimodal Request Support in Kthena Router
authors:
- TBD
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-11-16

---

## Basic Multimodal Request Support in Kthena Router

### Summary

Make the Kthena router aware of the images, audio and video carried by
OpenAI-compatible (and Anthropic) requests, so that vision-language and other
multimodal models can be served behind it as first-class citizens. The router
recognizes media content parts, gives every media item a content identity, keeps
it in the prompt view that scheduling plugins see, and forwards the request to
the model server unchanged. Text-only requests behave exactly as before.

This is a deliberately small first step. The router does not fetch or decode
media itself, estimate visual tokens for accounting, route on the encoder cache,
or split the encode stage onto its own pods (E/PD, E/P/D). It defines the data
model those features will build on, and lets the two cache-affinity plugins
handle media: `prefix-cache` without a tokenizer, and `kvcache-aware` exactly,
by having the engine render the prompt.

### Motivation

Multimodal models such as Qwen-VL, Gemma and LLaVA are served by vLLM and
SGLang through the same `/v1/chat/completions` endpoint as text models. A request
carries media as content parts next to the text:

```json
{
  "model": "qwen-vl",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "image_url", "image_url": {"url": "https://example.com/photo.jpg"}},
      {"type": "text", "text": "What is in this image?"}
    ]
  }]
}
```

Media differs from text in ways that matter to a router:

- **Identity is content-addressed, not prefix-addressed.** An image either
  matches or not as a whole, keyed by a hash of its content. The engine caches
  both the KV blocks and the encoder output of an image, and both are only
  reused if the request lands on the pod that holds them.
- **Size on the wire is unrelated to cost.** A 60-byte URL and a 2 MB base64
  PNG can both expand to hundreds of visual tokens, and the bytes may not be
  in the request at all.
- **There is an extra stage.** A multimodal request is encode, then prefill,
  then decode. The encode stage can later be moved to dedicated pods.

Today the router forwards multimodal requests, but it parses them as if they
were text-only:

1. Media parts are dropped from the parsed prompt, and a message that only
   holds media disappears from it. No scheduling plugin can tell that a request
   carries media.
2. The `prefix-cache` plugin sees the same prompt for two requests that ask the
   same question about different images, and builds affinity on that false
   match. It cannot build affinity for requests that share an image.
3. The `kvcache-aware` plugin tokenizes the text of a multimodal request and
   looks up block hashes that cannot match the engine's, because the engine mixes
   the hash of each media item into the cache keys of its tokens: vLLM into the
   hashes of the blocks that hold them, SGLang into the pad token IDs that replace
   them before the radix cache lookup.

#### Goals

- Recognize the media content parts of the OpenAI Chat Completions and Responses
  APIs, the `audio_url` and `video_url` extensions shared by vLLM and SGLang, and
  the Anthropic Messages API.
- Give every media item a modality and a content identity that is stable across
  requests and router replicas, without fetching or decoding the media.
- Expose the media items to scheduling plugins through the existing
  `framework.Context.Prompt`.
- Make `prefix-cache` affinity respect media identity.
- Make `kvcache-aware` compute the same block hashes as the engine for
  multimodal prompts, so that it matches the KV blocks holding media exactly.
- Forward multimodal requests to the model server unchanged, in aggregated and
  in Prefill/Decode disaggregated mode, with no new configuration.
- Keep text-only behavior, APIs and CRDs unchanged.

#### Non-Goals

- Fetching remote media or decoding inline media in the router.
- Estimating the number of tokens of media items for rate limiting, metrics or
  load-aware scoring.
- Encoder-cache-aware routing.
- Encode disaggregation (E/PD, E/P/D) and EC connectors.
- Exact multimodal KV block hashing for SGLang, which keys media by pad token
  IDs rather than by extra block keys.
- Media-specific metrics or access log fields.

### Proposal

#### User Stories

##### Story 1

A platform team serves `Qwen2.5-VL` with vLLM behind a Kthena `ModelRoute`.
Clients send chat requests with image URLs or base64 data URIs. The router
forwards them to the model pods without any configuration change.

##### Story 2

An e-commerce application asks many different questions about the same product
photo. With `prefix-cache` enabled, requests that share the photo and the
preceding conversation go to the pod that already processed it, while requests
about a different photo are not pulled toward that pod.

##### Story 3

The same team runs vLLM with KV events and the `kvcache-aware` plugin. A
multi-turn conversation about a document scan keeps landing on the pod whose
KV cache holds the scan and the turns before it, because the router computes
the same block keys as the engine, media included.

#### Notes/Constraints/Caveats

- The identity of a media item is derived from what the client sent: the URL for
  media passed by reference, the encoded payload for inline media. The same
  image sent once as a URL and once as base64 gets two identities, and a URL
  whose content changes keeps its identity. This matches how clients usually
  reuse media and needs no network access.
- The router still estimates input tokens from text only. Rate limiting on input
  tokens therefore undercounts multimodal requests. Fairness scheduling is not
  affected, because it records the prompt tokens reported by the engine.
- `kvcache-aware` renders multimodal prompts with the vLLM
  `/v1/chat/completions/render` endpoint, either on the tokenizer service or on
  a vLLM model pod. `vllm serve` registers that endpoint only with
  `--enable-scale-out`; `vllm launch render`, which the tokenizer service runs,
  always registers it. The router asks for `mm_payload: "none"` (vLLM v0.32 and
  later) so that the response carries no processed media; older versions
  ignore the field and also return the processed media, which works but makes
  the response larger.
- The KV events of the model server must report `extra_keys`, which vLLM does
  for the blocks holding media.
- SGLang pods are not rendered on. If a model has no vLLM pod and no tokenizer
  service, `kvcache-aware` does not score its multimodal requests and the other
  score plugins decide, as for any tokenization failure.

#### Risks and Mitigations

- **Hashing cost of large inline media.** Each inline payload is hashed once per
  request with xxHash, a non-cryptographic hash already used by `prefix-cache`.
  This is a single linear pass over bytes the router has already parsed as JSON.
- **Changed prefix-cache keys for multimodal requests.** Media placeholders change
  the prompt string of multimodal requests only. Existing prefix-cache entries of
  such requests stop matching once after the upgrade; the cache is in-memory and
  is rebuilt anyway when the router restarts.
- **Render cost on the scheduling path.** Rendering a multimodal prompt makes the
  engine fetch and preprocess its media, which takes longer than tokenizing
  text. It runs only for multimodal requests and only when `kvcache-aware` is
  enabled, with the same timeout as tokenization, and the model server fetches
  and processes the media again when it serves the request. The prompt is
  rendered once per request, also when Prefill-Decode Disaggregation scores
  several pod groups. Deployments sensitive to this latency can rely on `prefix-cache`
  alone for multimodal affinity, or run the tokenizer service close to the
  router.
- **Hash compatibility between router and sidecar.** The router and the runtime
  sidecar must hash media into block keys identically. Both implement one
  documented byte layout, and shared test vectors pin it in the Go and Python
  tests. Text-only blocks hash exactly as before, so existing KV indexes keep
  matching after the upgrade.

### Design Details

#### Data model

`common.Message` gets a list of media items. The field is excluded from JSON, so
tokenizer requests built from `Message` stay unchanged.

```go
type Modality string // "image", "audio", "video"

type MultiModalItem struct {
	Modality Modality
	// Hash identifies the item by what the client sent: the URL for media
	// passed by reference, or the encoded payload for inline media.
	Hash uint64
	// Offset is the byte offset in Message.Content at which the item appears.
	Offset int
}

type Message struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	MultiModal []MultiModalItem `json:"-"`
	// ContentParts is list content in Chat Completions form, for requests the
	// engine has to render; nil for string content or unrenderable media.
	ContentParts []map[string]interface{} `json:"-"`
}

func (m *ChatMessage) HasMultiModal() bool
```

Items are kept per message and in request order, and each item records its
offset in the joined text of its message, so later plugins can relate a media
item to its position in the conversation.

`ContentParts` keeps the text and media parts of a message in their order, in
the shape of the Chat Completions API, whatever API the request used, so that
the prompt can be rendered by the engine. It is nil for string content and for a
message holding a media part that has no Chat Completions form, such as a
Responses `input_image` given by `file_id`.

#### Parsing

`utils.ParsePrompt` keeps joining text parts into `Content`. A content part whose
type carries media is turned into a `MultiModalItem` through a single table:

| Part type     | API                               | Modality | Source fields             |
| ------------- | --------------------------------- | -------- | ------------------------- |
| `image_url`   | OpenAI Chat Completions           | image    | `image_url` (`.url`)      |
| `input_image` | OpenAI Responses                  | image    | `image_url`, `file_id`    |
| `image`       | Anthropic Messages                | image    | `source` (`.data`/`.url`) |
| `input_audio` | OpenAI Chat Completions           | audio    | `input_audio` (`.data`)   |
| `audio_url`   | vLLM and SGLang extension         | audio    | `audio_url` (`.url`)      |
| `video_url`   | vLLM and SGLang extension         | video    | `video_url` (`.url`)      |

A source field is either the source string itself or an object holding it in
`url`, `data` or `file_id`. The item hash is `xxhash.Sum64String(source)`. Parts
of other types, and media parts without a source, are ignored as before. A
message is kept if it has text or media. Supporting a new part type is one table
entry.

#### Prompt view of `prefix-cache`

`prefix-cache` hashes the prompt in fixed-size byte blocks and has no tokenizer
on purpose: it is the cheap affinity plugin that works for every engine. A new
`utils.GetPromptSegments` returns the prompt that `utils.GetPromptString`
renders, split into text segments and one segment per media item, at its offset
in the text of its message. A media segment is a placeholder holding the
modality and hash of the item:

```text
<|im_start|>user
<|image:9f86d081884c7d65|>What is in this image?<|im_end|>
```

A media segment ends the block it falls in, and the text after it starts a new
block. Two requests therefore share a prefix only if they share the media as
well as the text up to that point, and requests that share a prefix up to and
including some media match that far whatever text follows the media. For
text-only requests the blocks, and so the hashes, are the same as before.

`utils.GetPromptString` is unchanged and keeps feeding router-side input token
accounting (access log, metrics, input rate limits): it skips media, and skips
messages that only hold media, exactly as before.

#### Exact multimodal KV matching in `kvcache-aware`

vLLM expands each media item into placeholder tokens and keys every KV block
that overlaps an item by the item as well: the identifier of the item (the hash
of its processed content, or the UUID the client gave it) and the offset of its
first placeholder token relative to the block. The router can only compute the
same keys if it knows the token IDs, the placeholder positions and the
identifiers, so it asks the engine instead of guessing:

1. For a multimodal prompt, `kvcache-aware` sends a render request with
   `mm_payload: "none"` to the vLLM `/v1/chat/completions/render` endpoint. For
   a Chat Completions request this is the request as sent, with the upstream
   model and without `stream` and `stream_options`, so that fields such as
   `tools`, `chat_template_kwargs` and `mm_processor_kwargs` render the same
   prompt as for inference. For the other APIs it holds the converted messages,
   with `ContentParts` as their content, and `add_generation_prompt: true`. The
   endpoint applies the chat template, processes the media and returns the token
   IDs, plus the identifier and placeholder range of every media item. The
   tokenizer service proxies this endpoint; with `fallbackToEngine`, or without
   the service, the router renders on a vLLM pod of the model. Prompts with
   media passed by URL are always rendered on a vLLM pod, since rendering
   fetches the media and the tokenizer service may run in the router pod,
   where a client-provided URL could reach pod-local endpoints. The router
   checks every content part of the request as sent for a media URL, including
   parts without a `type`, from which vLLM also reads media URLs. A message
   without `ContentParts` holding media makes the prompt unrenderable, and the
   plugin does not score the request. The result is kept for the whole
   scheduling of the request, so with Prefill-Decode Disaggregation the decode
   and prefill pods are scored with a single render.
2. The router splits the token IDs into blocks of the engine block size, as for
   text, and hashes each block together with the media items overlapping it.
3. The runtime sidecar reads the `extra_keys` of the vLLM `BlockStored` events,
   which carry the same `(identifier, offset)` pairs per block, and mixes them
   into the standardized hash of the block in the same way. Other extra keys,
   such as a LoRA name or a cache salt, are ignored as before.

A block key is computed by both sides as SHA-256 over the big-endian `uint32`
token IDs, followed, for each media item overlapping the block in offset order,
by the big-endian `uint32` byte length of its identifier, the identifier and its
offset as a big-endian `int64`; the first 8 bytes, masked to 63 bits, are the
key. Without media the bytes, and so the key, are the same as for text.

Text-only prompts are tokenized as before.

#### Scheduling plugins

- `prefix-cache`: hashes the segments of `GetPromptSegments`.
- `kvcache-aware`: renders multimodal prompts and matches media-aware block
  keys; tokenizes text-only prompts as before.
- Load-based plugins (`least-request`, `least-latency`, `gpu-usage`) and session
  sticky work unchanged.

#### Request forwarding

The router already forwards the parsed request body to the selected pod. It only
rewrites the `model` field, requests usage reporting, and, in Prefill/Decode
disaggregated mode, adds the fields of the KV connector. Media parts are
forwarded as they are, and the engine fetches, decodes and encodes them.

#### Extension points

The design leaves room for the advanced features in the non-goals:

- **Token estimation.** A component can map each `MultiModalItem` to a token
  count — from a fixed per-model value, from the image size read out of inline
  data, or from the render result `kvcache-aware` already obtains — and add it
  to the input token count. `MultiModalItem` can grow fields such as width and
  height when that component needs them. This only changes the input token
  count; prefix-cache keys stay as they are, because a placeholder holds only
  the modality and hash of its item.
- **Encoder-cache affinity.** A score plugin with a `PostSchedule` hook can keep
  a per-pod LRU of item hashes and score pods by the share of the request's
  media they recently served, independent of the text prefix. It pays off mainly
  together with encode disaggregation, where encode pods keep a large encoder
  cache. In aggregated mode, media in a shared prefix is already reused through
  the KV cache that `prefix-cache` routes to, and the engine's encoder cache
  holds only about one scheduling step of embeddings, so the extra gain is
  limited to the same media following a different prefix.
- **SGLang KV matching.** SGLang replaces the placeholder tokens of each item
  with pad token IDs derived from its hash before the radix cache lookup. A
  render step that returns those pad IDs would let the same block hashing serve
  SGLang pods.
- **Encode disaggregation.** `HasMultiModal` is the decision point for routing a
  request through an encode pool, in the same way `PDGroup` selects
  Prefill/Decode scheduling today. Text-only requests keep skipping it.

#### Test Plan

- Unit tests for parsing every supported part type, ignored parts, request
  order, Responses and Anthropic shapes, Chat Completions content parts, and
  prompt segments.
- Unit tests that `prefix-cache` does not match requests that differ only in
  media, that media ends a block, and that requests sharing a prefix up to some
  media match up to it.
- Unit tests of the render request and response handling, the tokenizer service
  proxy, and `kvcache-aware` scoring of multimodal requests against a fake
  render endpoint: the same image matches, a different image does not, and
  render failures leave the request unscored.
- Shared test vectors for the media-aware block key in the Go and Python tests,
  and sidecar tests that `extra_keys` of KV events are decoded and hashed.
- A router test that a chat request with an image URL and a base64 data URI
  reaches the model server with its content unchanged.
- End-to-end on GPUs: a vision-language model (e.g. `Qwen/Qwen2.5-VL-3B-Instruct`)
  on vLLM behind a `ModelRoute`, with image URL and base64 requests, in
  aggregated mode with several replicas, once with `prefix-cache` and once with
  `kvcache-aware` and KV events enabled.

### Alternatives

- **Hash the decoded media.** Identity would survive a change of delivery (URL
  versus base64), but the router would have to fetch remote media and decode
  inline media on the request path. Rejected for the first version.
- **Drop media from the prompt, as today.** Keeps the router simplest, but
  leaves plugins blind to media and makes `prefix-cache` build false affinity.
- **Treat any multimodal request as cache-less and skip `prefix-cache` too.**
  Avoids false affinity but loses the main benefit of routing repeated media to
  the pod that already processed it.
- **Skip multimodal requests in `kvcache-aware`.** Needs no render step, but
  leaves the plugin blind exactly where KV reuse saves the most prefill.
- **Approximate multimodal block keys in the router.** Tokenizing the text and
  estimating the placeholder tokens would avoid the render call, but the
  placeholder count depends on the model's processor and the media size, and
  the engine's media identifier on the processed content, so the keys would not
  match the engine's.
- **Give `prefix-cache` a tokenizer too.** Would make its blocks follow the
  engine's, but `prefix-cache` is the plugin for deployments without a
  tokenizer, and byte blocks that end at media already keep its matches
  correct.
