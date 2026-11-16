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

This is a deliberately small first step. It does not fetch or decode media,
estimate visual tokens, route on the encoder cache, or split the encode stage
onto its own pods (E/PD, E/P/D). It defines the data model those features will
build on.

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

Media differs from text in ways that matter to a router (see
[Serving Multimodal at Scale in llm-d](https://llm-d.ai/blog/serving-multimodal-at-scale-in-llm-d)):

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
   looks up block hashes that cannot match the engine's, because vLLM mixes the
   hash of each media item into the hashes of the blocks that hold its tokens.

#### Goals

- Recognize the media content parts of the OpenAI Chat Completions and Responses
  APIs, the vLLM extensions (`audio_url`, `video_url`) and the Anthropic Messages
  API.
- Give every media item a modality and a content identity that is stable across
  requests and router replicas, without fetching or decoding the media.
- Expose the media items to scheduling plugins through the existing
  `framework.Context.Prompt`.
- Make `prefix-cache` affinity respect media identity, and keep `kvcache-aware`
  from scoring on block hashes it cannot compute.
- Forward multimodal requests to the model server unchanged, in aggregated and
  in Prefill/Decode disaggregated mode, with no new configuration.
- Keep text-only behavior, APIs and CRDs unchanged.

#### Non-Goals

- Fetching remote media or decoding inline media in the router.
- Estimating the number of tokens of media items for rate limiting, metrics or
  load-aware scoring.
- Encoder-cache-aware routing.
- Encode disaggregation (E/PD, E/P/D) and EC connectors.
- Exact multimodal KV block hashing in `kvcache-aware`.
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

#### Notes/Constraints/Caveats

- The identity of a media item is derived from what the client sent: the URL for
  media passed by reference, the encoded payload for inline media. The same
  image sent once as a URL and once as base64 gets two identities, and a URL
  whose content changes keeps its identity. This matches how clients usually
  reuse media and needs no network access.
- The router still estimates input tokens from text only. Rate limiting on input
  tokens therefore undercounts multimodal requests. Fairness scheduling is not
  affected, because it records the prompt tokens reported by the engine.

#### Risks and Mitigations

- **Hashing cost of large inline media.** Each inline payload is hashed once per
  request with xxHash, a non-cryptographic hash already used by `prefix-cache`.
  This is a single linear pass over bytes the router has already parsed as JSON.
- **Changed prefix-cache keys for multimodal requests.** Media placeholders change
  the prompt string of multimodal requests only. Existing prefix-cache entries of
  such requests stop matching once after the upgrade; the cache is in-memory and
  is rebuilt anyway when the router restarts.

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
}

type Message struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	MultiModal []MultiModalItem `json:"-"`
}

func (m *ChatMessage) HasMultiModal() bool
```

Items are kept per message and in request order, so later plugins can relate a
media item to its position in the conversation.

#### Parsing

`utils.ParsePrompt` keeps joining text parts into `Content`. A content part whose
type carries media is turned into a `MultiModalItem` through a single table:

| Part type     | API                               | Modality | Source fields             |
| ------------- | --------------------------------- | -------- | ------------------------- |
| `image_url`   | OpenAI Chat Completions           | image    | `image_url` (`.url`)      |
| `input_image` | OpenAI Responses                  | image    | `image_url`, `file_id`    |
| `image`       | Anthropic Messages                | image    | `source` (`.data`/`.url`) |
| `input_audio` | OpenAI Chat Completions           | audio    | `input_audio` (`.data`)   |
| `audio_url`   | vLLM extension                    | audio    | `audio_url` (`.url`)      |
| `video_url`   | vLLM extension                    | video    | `video_url` (`.url`)      |

A source field is either the source string itself or an object holding it in
`url`, `data` or `file_id`. The item hash is `xxhash.Sum64String(source)`. Parts
of other types, and media parts without a source, are ignored as before. A
message is kept if it has text or media. Supporting a new part type is one table
entry.

#### Prompt rendering

A new `utils.GetPromptStringWithMedia` renders the prompt like
`utils.GetPromptString`, plus a placeholder for each media item at the start of
its message:

```text
<|im_start|>user
<|image:9f86d081884c7d65|>What is in this image?<|im_end|>
```

The `prefix-cache` plugin hashes this string instead of the text-only one, so two
requests share a prefix only if they share the media as well as the text up to
that point. For text-only requests both functions return the same string.

`utils.GetPromptString` is unchanged and keeps feeding router-side input token
accounting (access log, metrics, input rate limits): it skips media, and skips
messages that only hold media, exactly as before.

#### Scheduling plugins

- `prefix-cache`: hashes `GetPromptStringWithMedia` instead of `GetPromptString`.
- `kvcache-aware`: returns no scores for a request with media and logs it at
  verbosity 4. The scheduler already gives unscored pods a score of 0, so the
  other score plugins decide.
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
  data, or from the vLLM `/v1/chat/completions/render` endpoint — and add it to
  the input token count. `MultiModalItem` can grow fields such as width and
  height when that component needs them.
- **Encoder-cache affinity.** A score plugin with a `PostSchedule` hook can keep
  a per-pod LRU of item hashes and score pods by the share of the request's
  media they recently served, independent of the text prefix.
- **Exact multimodal KV matching.** With a tokenizer that returns multimodal
  placeholder positions and hashes (for example the vLLM render endpoint, or
  client-provided `multi_modal_uuids`), `kvcache-aware` can compute engine-equal
  block hashes instead of skipping media requests.
- **Encode disaggregation.** `HasMultiModal` is the decision point for routing a
  request through an encode pool, in the same way `PDGroup` selects
  Prefill/Decode scheduling today. Text-only requests keep skipping it.

#### Test Plan

- Unit tests for parsing every supported part type, ignored parts, request
  order, Responses and Anthropic shapes, and prompt rendering.
- Unit tests that `prefix-cache` does not match requests that differ only in
  media, and that `kvcache-aware` neither scores nor tokenizes media requests.
- A router test that a chat request with an image URL and a base64 data URI
  reaches the model server with its content unchanged.
- End-to-end on GPUs: a vision-language model (e.g. `Qwen/Qwen2.5-VL-3B-Instruct`)
  on vLLM behind a `ModelRoute`, with image URL and base64 requests, in
  aggregated mode with several replicas and `prefix-cache` enabled.

### Alternatives

- **Hash the decoded media.** Identity would survive a change of delivery (URL
  versus base64), but the router would have to fetch remote media and decode
  inline media on the request path. Rejected for the first version.
- **Drop media from the prompt, as today.** Keeps the router simplest, but
  leaves plugins blind to media and makes `prefix-cache` build false affinity.
- **Treat any multimodal request as cache-less and skip `prefix-cache` too.**
  Avoids false affinity but loses the main benefit of routing repeated media to
  the pod that already processed it.
