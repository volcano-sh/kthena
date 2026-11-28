# Batch API

This package is the OpenAI-compatible batch API of kthena-router. The design is in
[the proposal](../../../docs/proposal/kthena-router-batch-api.md).

So far a client can upload, list, read and delete files, create a batch, and read it back while every
router replica runs it. Listing and cancelling batches come next.

## How a request gets here

The router sends everything under `/v1` to one catch-all handler. That handler asks an
`APIExtension` first (see `pkg/kthena-router/router`), and `Service` in this package is that
extension: `Handles` claims the paths under `/v1/files` and `/v1/batches`, and `Serve` answers them.
Every other path stays a model request. The access log and authentication run before it, as for any
`/v1` request. Like `/v1/models`, it answers on every listener and host the router serves, before any
route matching, so with authentication off every Gateway shares the one `anonymous` tenant.

`cmd/kthena-router/app/batch.go` builds the `Service` when `BATCH_ENABLED` is true. The Helm
chart sets that from `kthenaRouter.batch.enabled`. `Start` checks the storage in the
background, every 5 seconds while it is not ready and every minute while it is. The router's
listeners never wait for the volume or Redis, the API comes back by itself, and a check that fails
later closes it again until a check passes.

## How a batch runs

1. `POST /v1/batches` checks the input file and holds it, writes the batch and queues it, all in one
   Redis step. The batch remembers who created it and how the request came in: the host and, in
   Gateway API mode, the listener.
2. Every replica runs a worker with two runners. While the storage is ready, a runner takes the oldest
   queued batch and stamps it with a new random fence; every later write of that run checks the fence.
3. The run counts the lines, then sends them in order, eight at a time, through an in-process copy of
   the router's request handling. The batch's owner and listener travel in the Go context, never in a
   header, so a line is routed, rate limited and logged like a request from its owner. A line carries no session key,
   so session stickiness does not apply to it, and it counts in the router's request metrics like any request.
4. Answers go to a new output file and a new error file. When every line is answered, both files are
   made durable, recorded with purpose `batch_output`, and the batch moves to `completed`.
5. On shutdown a run stops, throws its files away and hands the batch back to the queue at its old
   place; the router waits up to 15 seconds for that. The next run starts from the first line.

Not there yet: a batch whose pod dies stays `in_progress` until the takeover comes, a bad line becomes an
error line instead of failing the whole batch, and the 24 hour window is recorded but not enforced.

## Files

| File | What it does |
| --- | --- |
| `api_files.go` | HTTP handlers for `/v1/files`: upload, list, get, download and delete, and the routing between them. An upload that sends nothing for a minute is cut off with 408. |
| `api_batches.go` | HTTP handlers for `/v1/batches`: create and get, with the OpenAI limits on the request and the OpenAI batch object. |
| `tenant.go` | Works out who owns a request from the verified token's issuer and subject. A verified token without a subject is refused, since all such tokens would share one owner. Without authentication every caller is `anonymous`. |
| `errors.go` | Writes errors in the OpenAI error shape. |
| `types.go` | The file and batch records, the batch statuses and the constants for purposes. |
| `store.go` | `Store`, the interface for the records that router replicas share. |
| `redis_store.go` | `RedisStore`, the Redis implementation of `Store`, for files and the volume. |
| `redis_batches.go` | The batch part of `RedisStore`: create, read, claim, progress, the next status, and handing a batch back. |
| `scripts/` | Lua scripts for the Redis steps that must happen at once, one file each. Every one is safe to run twice, because go-redis retries a command whose answer was lost. |
| `files.go` | `FileStore`, the interface for file bytes, and `FSStore`, which keeps them on a mounted volume. A file appears under its ID only once it is on disk. |
| `ready.go` | The readiness gate: every request answers 503, and no batch is taken, while the volume does not work or is not the one the Redis records belong to. Also the volume ID, and `Start`. |
| `worker.go` | The runners that take queued batches, run them, and hand them back when a run stops. |
| `exec.go` | Runs one batch: counts the lines, sends them, writes the answers and publishes the result files. |
| `dispatch.go` | Sends one line through the router's request handling inside the process, with its batch's identity in the Go context. |

## Where the data lives

- Records are in Redis under the prefix `kthena:batch:{b}:`: files under `file:`, batches under `job:`, the
  queue of waiting batches under `queue`. Each tenant has an index used for listing files.
  The records cannot be rebuilt from the volume, so Redis must keep its data, with AOF and
  `maxmemory-policy noeviction` as the proposal says. A Redis that loses them lists no files, while
  the bytes stay on the volume until the cleanup.
- Bytes are on the volume at `<storage path>/<tenant>/<file id>`, result files too; they expire after
  30 days, like uploads. The directory `.kthena` is reserved for the store's own files.
- The volume has an ID in `.kthena/volume-id`, written once and published with a hard link, so the
  volume must support rename and hard links. The first router to start records it
  in Redis under `kthena:batch:{b}:volume`. A router whose volume has another ID answers 503 and
  logs both IDs, because the records point at bytes that are not on its volume. To start over on a
  new volume on purpose, delete the records under the prefix, including that key.

Another tenant's file or batch answers 404, the same as a missing one, and so does a file past its
`expires_at`, which is no longer listed either; removing its record and bytes comes with the
cleanup in a later PR. Redis or the volume being down answers 503.
