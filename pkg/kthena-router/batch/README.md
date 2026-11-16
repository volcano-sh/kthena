# Batch API

This package is the OpenAI-compatible batch API of kthena-router. The design is in
[the proposal](../../../docs/proposal/kthena-router-batch-api.md).

Only the files endpoints exist so far. Batches and the worker that runs them come next.

## How a request gets here

The router sends everything under `/v1` to one catch-all handler. That handler asks an
`APIExtension` first (see `pkg/kthena-router/router`), and `Service` in this package is that
extension: `Handles` claims the paths under `/v1/files` and `Serve` answers them. Every other
path stays a model request. The access log and authentication run before it, as for any `/v1`
request.

`cmd/kthena-router/app/batch.go` builds the `Service` when `BATCH_ENABLED` is true. The Helm
chart sets that from `kthenaRouter.batch.enabled`.

## Files

| File | What it does |
| --- | --- |
| `api_files.go` | HTTP handlers for `/v1/files`: upload, list, get, download and delete, and the routing between them. |
| `tenant.go` | Works out who owns a request from the verified token's issuer and subject. Without authentication every caller is `anonymous`. |
| `errors.go` | Writes errors in the OpenAI error shape. |
| `types.go` | The file record and the constants for purposes and statuses. |
| `store.go` | `Store`, the interface for the records that router replicas share. |
| `redis_store.go` | `RedisStore`, the Redis implementation of `Store`. |
| `scripts/` | Lua scripts for the Redis steps that must happen at once: creating a file record and deleting one. |
| `files.go` | `FileStore`, the interface for file bytes, and `FSStore`, which keeps them on a mounted volume. |

## Where the data lives

- Records are in Redis under the prefix `kthena:batch:{b}:`. Each tenant has an index used for listing.
- Bytes are on the volume at `<storage path>/<tenant>/<file id>`. The directory `.kthena` is
  reserved for the store's own files.

Another tenant's file answers 404, the same as a missing one. Redis or the volume being down
answers 503.
