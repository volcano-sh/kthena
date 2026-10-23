---
title: OpenAI-Compatible Batch API in the Kthena Router
authors:
- "@aeron-gh"
reviewers:
- "@YaoZengzeng"
- "@hzxuzhonghu"
- "@LiZhenCheng9527"
- TBD
approvers:
- TBD

creation-date: 2026-09-22

---

## OpenAI-Compatible Batch API in the Kthena Router

### Summary

Kthena serves interactive traffic well, but offline work — dataset inference, embedding
generation, evaluation runs — has no home. Today a user has to write their own loop over
`/v1/chat/completions`, and own the retries, the progress tracking and the restart
behavior themselves.

This proposal adds an OpenAI-compatible `/v1/files` and `/v1/batches` to the
kthena-router. A user uploads a JSONL file, creates a batch, polls it, and downloads a
result file, exactly as they would against OpenAI. Kthena owns the whole lifecycle:
neither vLLM nor SGLang exposes a batch API on its online server, so the router reads the
input file line by line and sends each line through its own request path, reusing the
routing, rate limiting, fairness and KV-cache aware scheduling that interactive requests
already get.

State lives in Redis and file bytes live behind a storage interface, so any replica can
serve the API, exactly one replica runs a given batch at a time, and a batch survives the
pod that created it. A proof of concept covering all of this is implemented and measured;
the numbers in [Evidence from the proof of concept](#evidence-from-the-proof-of-concept)
come from it.

### Motivation

Large offline inference is a first-class LLM workload and a poor fit for a synchronous
API. It is throughput-oriented rather than latency-sensitive, it runs for hours, and it
needs to survive process restarts. Running it through the interactive path means the
client owns durability, and a client that dies loses its progress.

OpenAI's Batch API is the de facto shape for this, so matching its schema lets existing
clients move to Kthena without code changes.

Putting it in the router rather than in a new component is what makes it cheap: the
router already knows backend load, already owns model-aware routing, and already talks to
Redis for the on-flight counter and the global rate limit. Batch dispatch reuses all of
it, and operators get the feature by flipping a Helm value rather than deploying and
scaling another service.

#### Goals

- OpenAI-compatible `/v1/files` and `/v1/batches` served by the kthena-router, with the
  same object shapes, status values and error envelope.
- Batches of tens of thousands of requests, with bounded router memory regardless of
  batch size.
- Each batch line takes the same path as an interactive request, so route matching,
  rate limiting, fairness and the KV-cache aware plugins all still apply.
- Exactly-once results: every `custom_id` in the input appears exactly once across the
  output and error files, including across a takeover.
- Any replica can serve the API; exactly one replica executes a given batch; a batch
  survives the loss of the pod running it.
- Cancellation, expiry, and garbage collection of finished batches and their files.
- Off by default, configured through Helm values.

#### Non-Goals

- Requiring engine-native batch support. vLLM's online server has no `/v1/files` or
  `/v1/batches` (its batch mode is the offline `vllm run-batch`, which starts its own
  engine), and SGLang's online server has neither. Kthena owns the lifecycle.
- A new deployable. The batch API and worker run inside kthena-router.
- Matching OpenAI's 24-hour discount pricing semantics. `completion_window` is accepted
  and enforced as a deadline; it is not a billing input.
- Cross-cluster or cross-region batch scheduling.
- Object storage in the first phase. The storage seam exists, but the first backend is a
  shared volume — see [File storage](#file-storage).

### Proposal

A user uploads a JSONL input file to `/v1/files` with `purpose: batch`, creates a batch
against it at `/v1/batches`, polls that batch, and when it reaches `completed` downloads
`output_file_id` and `error_file_id`. Every line of the input is one chat completion
request with a `custom_id`; every line of the output is that `custom_id` and the
response.

Inside the router, creating a batch writes a job record to Redis and pushes its ID onto a
queue. Every replica that has the worker enabled runs a claim loop. A claim takes a
renewable lease on the job and stamps it with a monotonically increasing fence token, so
exactly one replica executes it. The executor streams the input file, dispatches lines
through the router's own handler with bounded concurrency, appends results as they
arrive, and checkpoints progress. If the owning pod dies, the lease expires, another
replica requeues and claims the job, and resumes from the last checkpoint.

The shape of it:

```mermaid
flowchart TB
    client([client])

    subgraph replicaA["replica A: API and worker"]
        apiA["batch API"]
        workerA["batch worker"]
        chainA["router handler chain"]
    end

    subgraph replicaB["replica B: API only"]
        apiB["batch API"]
    end

    redis[("Redis")]
    volume[("shared volume")]
    backends["model backends"]

    client --> apiA
    client --> apiB
    apiA --> redis
    apiA --> volume
    apiB --> redis
    apiB --> volume

    redis -- claim --> workerA
    workerA -- one line at a time --> chainA
    chainA --> backends
    workerA -- append results, checkpoint --> volume
    workerA -- progress --> redis
```

| Box | What it holds |
| --- | --- |
| batch API | `/v1/files`, `/v1/batches` |
| batch worker | claim, renew, reap, release |
| router handler chain | route match, rate limit, fairness, KV-cache aware scheduling |
| Redis | jobs, queue, leases, fences, checkpoints |
| shared volume | input and result files |
| model backends | vLLM, SGLang |

Replica B serves the API but runs no batches, which is how an operator keeps batch load
off the replicas carrying interactive traffic. If replica A dies mid-batch, its lease
expires, and any replica with a worker requeues and resumes the job from its last
checkpoint.

#### User Stories

##### Story 1: offline dataset inference

A team has 50,000 prompts to score against a model Kthena already serves. They upload one
JSONL file, create a batch, and poll it every few minutes. The job runs for an hour, and
they download one output file. They never write a retry loop, and a router rollout in the
middle does not lose their progress.

##### Story 2: batch that yields to interactive traffic

A platform team wants to soak up spare GPU capacity overnight without risking daytime
latency. They run two router pools: the interactive one with `worker.enabled: false`, and
a smaller one with the worker on and per-job concurrency capped. Both pools serve the API
and share one Redis and one volume, so a batch created against either is executed only by
the worker pool, and the interactive p99 on the main pool is unaffected.

##### Story 3: a batch that must stop

A user notices a bad prompt template 3,000 requests into a 20,000 request batch and
cancels it. Work stops within seconds, the answers already produced stay in the output
file, and every request that never ran is written to the error file with
`batch_cancelled`, so they know exactly what to resubmit.

#### Notes/Constraints/Caveats

- **Redis becomes required when batch is enabled.** The router already uses Redis for the
  shared on-flight counter and the global rate limit, but both are optional today. With
  batch enabled, Redis holds the job records, the queue and the leases, so it is a hard
  dependency. The router refuses to start with batch enabled and no Redis rather than
  starting in a state where batches silently never run.
- **The shared volume must be ReadWriteMany for more than one replica.** Every replica
  serving the API must be able to read any file, and a takeover must read a file another
  pod wrote. A single-node test cluster gets away with ReadWriteOnce; a real cluster does
  not. The router probes the volume at startup and fails fast if it is not writable.
- **Batch cost lands on the replica running the job.** This is why the worker has a
  separate switch from the API: operators can serve the API from every pool and run
  batches only in the pool they want to carry the load. The switch is per Deployment, so
  a split means a second release of the chart, not a subset of one Deployment's replicas.
- **Streaming is off for batch lines.** A batch result is a complete response body.
- **First phase is `/v1/chat/completions` only.** `/v1/completions` and `/v1/embeddings`
  are the same mechanism and are the obvious next endpoints, but each needs its own
  validation and response shaping.

#### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Batch traffic degrades interactive latency | Worker is opt-in per router pool; per-job and per-replica concurrency caps; adaptive back-off against backend load (phased, see [Flow control](#flow-control)) |
| A partitioned owner keeps writing after takeover | Fence token on every state write; segment files scoped per ownership attempt, so a stale writer cannot corrupt the new owner's output |
| A large file exhausts router memory | Input streamed line by line, results appended as they complete; measured 15–40 MiB at 50,000 requests against the chart's 512Mi limit |
| Redis loses batch state | Require AOF with `appendfsync everysec` or stricter, plus `maxmemory-policy noeviction`, when batch is enabled. RDB alone is not sufficient: it snapshots periodically, so a crash can lose the most recent checkpoints, leases and queue entries |
| Forged batch identity | Owner and gateway context travel in the request context, never in a header; the engine that trusts them binds no listener, and a client cannot write into a Go `context.Context` |
| One tenant starves the queue | Not yet addressed: the queue is FIFO with no per-tenant bound. `maxQueuedBatchesPerTenant` and `maxActiveBatchesPerTenant` are proposed in [Configuration](#configuration) |
| Abandoned files fill the volume | Files carry an expiry; a batch holds a reference on its input so a delete keeps the bytes until the batch is done; reap drops finished jobs |

### Design Details

#### What is added

All new Go code lives in one new package, `pkg/kthena-router/batch`, plus one wiring file
under `cmd`. The Implementation Plan at the end splits these into serial PRs.

| File | Responsibility |
| --- | --- |
| `batch/types.go` | `Job`, `File`, `Lease`, `Checkpoint`, `Status` |
| `batch/store.go` | the `Store` interface, the only writer of job state |
| `batch/redis_store.go` | Redis implementation: keys, indexes, script loading |
| `batch/scripts/*.lua` | `claim`, `renew`, `transition`, `checkpoint`, `cancel`, `reap`, `release` |
| `batch/files.go` | `/v1/files`, the `FileStore` interface and its volume implementation |
| `batch/batches.go` | `/v1/batches`: create, list, get, cancel |
| `batch/validate.go` | input validation and the OpenAI error envelope |
| `batch/api.go` | implements `APIExtension`, dispatches the nine endpoints |
| `batch/worker.go` | claim loop, lease renewal, reaper, graceful release |
| `batch/exec.go` | runs one claimed batch: stream, dispatch, checkpoint, finalize |
| `batch/dispatch.go` | `Identity` on the request context, `HandlerDispatcher`, `batchWriter` |
| `cmd/kthena-router/app/batch.go` | builds the components, the in-process engine and `batchIdentity` |

#### How it plugs into the existing router

Existing files are only added to, never changed. The Go side is 29 added lines across
two files, and there are no deletions anywhere in the four:

| Existing file | Change |
| --- | --- |
| `pkg/kthena-router/router/router.go` | +27: the `APIExtension` interface, an `apiExtension` field on `Router` that is nil unless the feature is on, its setter and getter, and the consult inside `HandlerFunc` |
| `cmd/kthena-router/app/server.go` | +2: build the batch components when the feature is on |
| `charts/kthena/charts/networking/values.yaml` | +27: the `batch` block, defaulted off |
| `charts/kthena/charts/networking/templates/kthena-router/component/deployment.yaml` | +49: the optional volume mount |

Deliberately **not** modified: the scheduler, the datastore, the fairness queue, the rate
limiters, the access log, and `AuthMiddleware`. Batch lines reach all of them by being
dispatched through the ordinary handler chain rather than by being special cased inside
it. That is the one decision the rest of this section follows from.

#### End to end: the steps for one batch

```mermaid
sequenceDiagram
    participant C as client
    participant A as batch API
    participant R as Redis
    participant W as worker
    participant X as executor
    participant H as handler chain
    participant V as volume

    C->>A: POST /v1/files
    A->>V: stream bytes
    A->>R: write file record
    C->>A: POST /v1/batches
    A->>R: write job, push onto queue
    W->>R: claim, taking lease and fence
    W->>X: run this job
    X->>V: open input, open segments
    loop every line
        X->>H: in-process request
        H->>X: response
        X->>V: append result
    end
    X->>R: checkpoint, periodically
    X->>V: concatenate valid prefixes
    X->>R: transition to completed
    C->>A: GET batch, poll
    C->>A: GET output file content
```

1. **Upload**, batch API. `POST /v1/files` with `purpose: batch` streams the JSONL body
   into the file store and writes a `file:<id>` record owned by the caller's tenant.
2. **Create**, batch API. `POST /v1/batches` validates the input file, writes `job:<id>`,
   adds the batch to `fileref:<id>` so the input cannot be deleted while it is still
   needed, and pushes the ID onto `queue`. Validation rejects a duplicate `custom_id`,
   which is what makes the exactly-once guarantee well defined.
3. **Claim**, worker. Every replica with the worker enabled runs a claim loop. `claim`
   pops the oldest queued ID, increments the global `fence` counter, writes `lease:<id>`
   with a TTL, stamps `holder` and `fence` onto the job, and adds it to `active` for the
   reaper. Exactly one replica wins.
4. **Execute**, executor. The owner opens the input, creates output and error segments
   named with its fence, and dispatches lines with bounded concurrency. Each line goes
   through the in-process engine described under Dispatch, so it is route matched, rate
   limited, fairness queued, KV-cache scheduled and access logged exactly as a client
   request is.
5. **Checkpoint**, executor. Results are appended as they arrive, and `ckpt:<id>` records
   the valid byte count per segment, the done bitmap, the counts and token usage. Every
   write verifies the fence first.
6. **Renew, or lose the job**, worker. The lease is renewed on a timer, and `renew`
   returns the current status in the same round trip, which is how a running executor
   learns it was cancelled. If the pod dies the lease expires, `reap` requeues the job,
   and the next owner resumes from the checkpoint, skipping lines in the done bitmap.
7. **Finalize**, executor. The valid prefix of each segment is concatenated into the
   output and error files, their file records are written, and the job transitions to
   `completed`.
8. **Poll and download**, batch API. The client polls `GET /v1/batches/{id}` throughout,
   then fetches `output_file_id` and `error_file_id`.

Only steps 3 to 7 need a worker. A replica with `worker.enabled: false` still serves
steps 1, 2 and 8, which is what lets an operator run the API everywhere and batches only
where the load is wanted.

#### API surface

Served from inside the router. `Handles` claims the paths and `Serve` answers them:

| Method | Path | Behaviour |
| --- | --- | --- |
| `POST` | `/v1/files` | multipart upload, `purpose` must be `batch`; streams to the file store |
| `GET` | `/v1/files` | list the caller's files, newest first, optional `purpose` filter |
| `GET` | `/v1/files/{id}` | file object |
| `GET` | `/v1/files/{id}/content` | raw bytes |
| `DELETE` | `/v1/files/{id}` | marks deleted; bytes stay while a batch still needs them |
| `POST` | `/v1/batches` | create from an `input_file_id` |
| `GET` | `/v1/batches` | list the caller's batches, newest first, paginated |
| `GET` | `/v1/batches/{id}` | batch object with live `request_counts` |
| `POST` | `/v1/batches/{id}/cancel` | cancel |

Errors use the OpenAI shape, which nests the fields under a top-level `error` object:

```json
{"error": {"message": "...", "type": "invalid_request_error", "param": null, "code": null}}
```

This differs from the rest of the router and is deliberate, because clients parse it. The
parity covers errors these endpoints generate themselves. A request rejected earlier, by
`AuthMiddleware`, still gets the router's existing `{"error": "<string>"}` body, so on a
401 an OpenAI client sees a string where it expects an object. Aligning that one response
is a small change to the auth middleware, called out here because it sits outside the
extension.

Status values and transitions match OpenAI:

```mermaid
stateDiagram-v2
    [*] --> validating
    validating --> in_progress
    validating --> expired
    validating --> cancelling
    validating --> failed
    in_progress --> finalizing
    in_progress --> expired
    in_progress --> cancelling
    in_progress --> failed
    finalizing --> completed
    finalizing --> failed
    cancelling --> cancelled
    completed --> [*]
    failed --> [*]
    expired --> [*]
    cancelled --> [*]
```

##### Why an extension hook rather than gin routes

The router does not register model paths individually. It registers one catch-all that
claims everything under `/v1` (`cmd/kthena-router/app/router.go:293-296`):

```go
v1Group := engine.Group("/v1")
v1Group.Use(AccessLogMiddleware(cfg.defaultRouter))
v1Group.Use(AuthMiddleware(cfg.defaultRouter))
v1Group.Any("/*path", cfg.defaultRouter.HandlerFunc())
```

gin builds a radix tree per HTTP method, and a catch-all owns its whole subtree. So
`/v1/files` beside `/v1/*path` is not a route that loses a priority contest, it is a
tree gin refuses to build. On gin v1.10.0 it panics at registration:

```
'/files' in new path '/v1/files' conflicts with existing wildcard '/*path'
in existing prefix '/v1/*path'
```

Registering the specific route first does not help, it only moves the panic:

```
catch-all wildcard '*path' in new path '/v1/*path' conflicts with existing
path segment 'files' in existing prefix '/v1/files'
```

That leaves two real options. Drop the catch-all and enumerate every model path, which
means the router stops forwarding any OpenAI path it has not been taught, and a new
upstream endpoint becomes a code change. Or let the catch-all hand off the paths it does
not own. This proposal takes the second, as a two-method seam:

```go
type APIExtension interface {
	Handles(method, path string) bool
	Serve(c *gin.Context)
}
```

`Router.HandlerFunc` (`pkg/kthena-router/router/router.go:284`) consults the extension
before it parses a model request, and only for `/v1/` paths, which `AuthMiddleware`
(`cmd/kthena-router/app/router.go:711`) has already authenticated. If `Handles` returns
false the request continues down the existing model path unchanged. When batch is
disabled the field is nil and the cost is one nil comparison per request.

The route table itself is never touched, which is the point: batch adds a branch inside
the catch-all instead of competing with it. Any future non-model OpenAI endpoint can use
the same seam.

#### State model

All keys are prefixed and live in Redis.

| Key | Type | Holds |
| --- | --- | --- |
| `job:<id>` | hash | the batch object, plus `holder`, `fence`, and per-status timestamps |
| `file:<id>` | hash | file record: tenant, filename, purpose, status, bytes, expiry |
| `queue` | zset | claimable job IDs, scored by enqueue time |
| `active` | zset | claimed job IDs, for the reaper |
| `lease:<id>` | string with TTL | `holder:fence` of the current owner |
| `fence` | counter | monotonic, incremented on every claim |
| `ckpt:<id>` | hash | resumable progress: segments, done bitmap, counts, usage |
| `fileref:<id>` | set | batches still needing this file, so a delete keeps the bytes |

Every multi-step state change is a Lua script, so it is atomic against other replicas.
There are seven, and each one is short enough to read in full: `claim`, `renew`,
`transition`, `checkpoint`, `cancel`, `reap`, `release`.

#### Ownership: leases and fencing

`claim` pops the oldest queued job, checks it is still claimable, increments the global
fence counter, writes `lease:<id>` with a TTL, and stamps `holder` and `fence` onto the
job. It expires a batch that blew its deadline while queued rather than starting it.

Every subsequent write — `transition`, `checkpoint` — verifies the fence still matches
before it does anything. A partitioned owner that comes back to life has a stale fence and
every write it attempts is rejected with `CONFLICT_FENCE`. This is what makes takeover
safe without needing to stop the old owner.

`renew` extends the lease and returns the current status in the same round trip, which is
how a running executor learns it has been cancelled.

`reap` runs on a timer, finds `active` entries whose lease key is gone, and requeues them.
`release` is the graceful path: on shutdown the worker hands unfinished batches straight
back to the queue instead of making the cluster wait out the lease.

#### Resumable execution

Naive resumption double-answers lines. The design avoids that with two pieces:

**Segments.** Each ownership attempt writes its own output and error segment, named with
the fence. The checkpoint records, for each segment, how many bytes are *valid*. On
resume, a new owner reads only the first `ValidBytes` of each earlier segment and starts
fresh segments of its own. Anything a dying owner half-wrote past its last checkpoint is
ignored, because the checkpoint never counted it.

**A done bitmap.** The checkpoint carries a bitmap of completed line numbers. A resuming
owner skips lines already answered. Combined with segments, every `custom_id` appears
exactly once in the committed output, even when a pod dies mid-write.

What the fence does not cover is work already in flight at the backend. An owner that has
been partitioned can still have requests outstanding when its lease expires, and taking
over does not cancel them. Its results are never committed, because every checkpoint write
is rejected on the stale fence, but the backend may have served those lines twice. The
guarantee is therefore exactly-once committed output, with duplicate backend work possible
for the lines in flight across a takeover.

The two stores have to be ordered against each other. A checkpoint is only safe once the
bytes it counts are durable, otherwise losing a node could leave Redis holding a done bit
for a line whose output never reached the volume. So the executor syncs both open segments
before it writes the checkpoint, which is the file-side counterpart to requiring AOF on
Redis: whichever store is weaker sets the real guarantee.

Finalizing concatenates the valid prefix of every segment into the final output and error
files, then transitions to `completed`. The result files are written under deterministic
names through `Create`, which stages a temporary file and renames it into place, so a
finalize that is interrupted and retried by the next owner rebuilds the same bytes and
swaps them in once.

#### Dispatch

Each line becomes an in-process HTTP request through the router's own `http.Handler`:

```go
identity := Identity{Tenant: request.Tenant, Dispatch: request.Dispatch}
httpRequest, err := http.NewRequestWithContext(
	WithIdentity(lineCtx, identity), http.MethodPost, request.Endpoint,
	bytes.NewReader(request.Body))
```

This is the core design decision and the reason batch lines get everything interactive
requests get: the same route matching, the same rate limits, the same fairness queue, the
same KV-cache aware scheduling, the same access log.

##### Where a line enters the chain

Not through the network-facing engine. A `/v1` request from a client passes
`AccessLogMiddleware` and then `AuthMiddleware`, and it is `AuthMiddleware` that sets
`common.UserIdKey` from the verified token. Per-user rate limiting and the fairness queue
read that key, so a line that skipped it would run as an empty user and the paragraph
above would not be true.

The worker therefore dispatches into a second gin engine, assembled in process and never
bound to a listener:

```
recovery -> batchIdentity -> [access log] -> Router.HandlerFunc()
```

`batchIdentity` stands in for `AuthMiddleware`. It reads the identity from the Go request
context, aborts with 403 when there is none, and sets `common.UserIdKey` and, in Gateway
API mode, the listener keys: the same gin context state the authenticated path would have
produced. From that point on the request is handled by the ordinary chain, so route
matching, rate limiting, fairness, KV-cache aware scheduling and the access log all behave
as they do for a user request.

This is also why the identity travels in the context rather than a header. A client cannot
write a value into a Go `context.Context`; the only writer is the dispatcher, in the same
process, and the engine that trusts it accepts no connections. The real `AuthMiddleware` is
left untouched, so nothing reachable from the network is taught to trust a context-carried
identity.

One piece of authenticated state is not reproduced. `AuthMiddleware` also stores the
parsed token under `common.JWTTokenKey`, which is what `auth.ClaimFromContext` reads for a
ModelServer whose session key source is `JWTClaim`. `batchIdentity` does not set it, so
that session key resolves empty on batch lines and those lines are not sticky. Replaying
the token itself would put a bearer credential in Redis for the life of the batch, and it
can expire mid-run, so the proposed fix is to resolve the session key once when the batch
is created and replay that value beside the tenant and gateway context. Flagged here for a
decision rather than settled.

Two details matter:

- **Identity is captured once, not per line.** The owning tenant and, under Gateway API,
  the listener the batch arrived on, are recorded when the batch is created and replayed
  on every line through `context.WithValue`, so a batch keeps the identity it was created
  with even when another replica takes it over.
- **The response writer has to look like a real connection.** `batchWriter` collects the
  body up to a cap and implements `Flush` and `CloseNotify`, because gin's streaming path
  casts the writer to `http.CloseNotifier` and a writer without it panics. Its notify
  channel follows the line's context, so the goroutine ends with the line.

A panic in a line is recovered and recorded as that line's error rather than taking down
the worker.

#### File storage

File bytes sit behind a `FileStore` interface with `Create`, `Open`, `Size`, `Append`,
`Remove` and `Probe`. The first implementation is a shared volume, which is what the
issue discussion settled on as the simpler starting point.

**Known limitation, stated plainly:** the current interface returns `*os.File` from
`Open` and `Append`. That is honest about the shared-volume implementation but is not
implementable for S3. Before an object-storage backend lands, those two methods need to
return `io.ReadSeekCloser` and `io.WriteCloser`. This is a small, mechanical change and is
called out here so reviewers can decide whether to make the interface abstract now or
when the second backend arrives. The recommendation is to do it now, while there is one
implementation to update.

**A second one, in the reference counting:** `fileref` only keeps the bytes alive if the
reference is taken atomically with the check that the file is still live. Creating a
batch validates the input and then adds the reference as a separate step, so a delete
landing between the two can see no references, conclude the bytes are free and remove
them, leaving the new batch pointing at an input that is gone. The window is narrow and
the result is a batch that fails rather than wrong output, but the sentence "a delete
keeps the bytes until the batch is done" is not true across it. Closing it is small:
re-read the deleted flag after taking the reference and release it if the file went away,
or take the reference and the check in one script.

#### Flow control

Implemented today: a cap on concurrently running batches per replica
(`maxConcurrentJobs`) and a per-batch cap on in-flight lines (`concurrency`), plus the
per-pool worker switch.

Proposed next, and **not yet built**: additive-increase/multiplicative-decrease on the
per-batch concurrency, driven by the backend in-flight counts the datastore already
tracks. When interactive pressure rises, batch concurrency backs off; when it falls,
batch creeps back up. This is the piece that turns "batch runs somewhere else" into
"batch yields", and it is deliberately scoped as a follow-up so the first PRs stay
reviewable.

#### Multi-tenancy

The owning tenant is the authenticated subject, read from the same gin key the rest of
the router uses (`common.UserIdKey`). Files and batches are scoped to it: a lookup that
does not match the caller's tenant is a 404, not a 403, so IDs cannot be probed.

When auth is disabled every caller shares one tenant, which is the same posture the rest
of the router takes.

#### Configuration

Off by default, under `kthenaRouter.batch` in the networking chart:

```yaml
batch:
  enabled: false
  storage:
    claimName: ""            # ReadWriteMany when replicas > 1
    mountPath: /var/lib/kthena/batch
    fsGroup: 65532
  worker:
    enabled: true            # false: serve the API here, run batches elsewhere
    maxConcurrentJobs: 2
    concurrency: 8
    leaseTTL: 30s
    lineTimeout: 10m
  limits:
    maxFileBytes: 209715200      # 200 MB, OpenAI's limit
    maxRequestsPerBatch: 50000   # OpenAI's limit
    maxLineBytes: 1048576
    maxResponseBytes: 2097152
  accessLog: true            # one access log line per batch request
  forwardHeaders: []         # client headers replayed, for header-matching ModelRoutes
```

Two startup guards, both fail-fast: batch enabled without Redis, and a configured volume
that is not actually writable by this pod.

`worker.enabled` is a property of a Deployment rather than of a single replica: the chart
renders one router Deployment, so every replica in it shares the setting. Splitting
interactive and batch load, as Story 2 does, means a second release of the chart with its
own `replicas` and `worker.enabled: true`, pointed at the same Redis and the same volume.
A single install can only turn the worker on everywhere or nowhere.

A response larger than `maxResponseBytes` is never truncated into the output file. The
dispatcher reports the line as failed, and it is written to the error file with code
`response_too_large` and counted in `request_counts.failed`, so the output file only ever
contains whole responses.

**Not built yet, and needed before the queue is fair to share:** the queue is FIFO by
enqueue time with no per-tenant bound, so one tenant submitting many large batches delays
everyone behind them, and nothing caps how much work may be queued at once. Two limits
belong in the block above, `maxQueuedBatchesPerTenant` and `maxActiveBatchesPerTenant`,
matching the per-organization batch limits OpenAI enforces for the same reason. The store
already keeps a per-tenant index of batches, so counting a tenant's queued and running
work needs no new state; what is missing is the admission check on create and a claim loop
that passes over a tenant already at its active cap.

#### Observability

**Not yet implemented, and required before this is production-ready.** The proposed
metrics:

| Metric | Type | Labels |
| --- | --- | --- |
| `kthena_batch_jobs` | gauge | `status` |
| `kthena_batch_lines_total` | counter | `outcome` |
| `kthena_batch_job_duration_seconds` | histogram | — |
| `kthena_batch_claims_total` | counter | `result` |
| `kthena_batch_takeovers_total` | counter | — |
| `kthena_batch_queue_depth` | gauge | — |

Batch lines already appear in the existing access log, with a per-pool switch to turn
that off for large batches.

#### Backward compatibility

Additive. With `batch.enabled: false` — the default — no Redis keys are written, no
volume is mounted, the API extension is nil, and `Router.HandlerFunc` performs one extra
nil check per request. Existing deployments are unaffected.

#### Test Plan

Unit tests per component: the Redis store against a Redis fake including every Lua script
and the fence-conflict paths; the file store including the probe and the size limits; the
API including tenant isolation, validation and the error envelope; the executor including
resume-from-checkpoint and the segment prefix rules; the worker including claim, renew,
lease loss and graceful release.

E2E on kind, with two router replicas sharing one volume and one Redis, against the
`llm-d-inference-sim` mock the existing e2e tests already use:

1. Happy path: every `custom_id` appears exactly once.
2. Takeover: kill the pod running a batch mid-flight, assert the batch still completes
   with no missing and no duplicated `custom_id`.
3. Cancel mid-flight: completed answers kept, the rest recorded as `batch_cancelled`.
4. Expiry: same shape, recorded as `batch_expired`.
5. Gateway API mode: a batch against a model only reachable through a Gateway-bound
   ModelRoute.

### Evidence from the proof of concept

The branch is [`poc/batch-api`](https://github.com/aeron-gh/kthena/tree/poc/batch-api).

Measured on kind: two router replicas, one shared volume, one Redis, `llm-d-inference-sim`
as the backend. Single node, so ReadWriteOnce sufficed there; a real multi-node cluster
needs ReadWriteMany.

**Correctness**

| Scenario | Result |
| --- | --- |
| 200 / 10k / 20k / 50k request batches | every `custom_id` exactly once |
| Pod killed (`--force --grace-period=0`) at 6,400 of 20,000 | other replica took over 36s later (30s lease + reap tick); final 20,000 completed, 0 missing, 0 duplicated |
| Cancel at 3,600 done | cancelled ~8s later; 3,800 answers kept, 16,200 `batch_cancelled` |
| Expiry | 2,400 kept, 17,600 `batch_expired` |
| Gateway API mode | 300/300 completed |

**Cost to interactive traffic**, 200 samples each, same cluster:

```
idle                            p50  8.3ms   p99 14.9ms
during a 50k batch              p50 16.8ms   p99 84.6ms
same, batch concurrency 1       p50  9.2ms   p99 73.5ms
```

Measured per pod while a batch ran, the replica doing the work was at p99 102ms and the
idle replica at p99 28ms, with the node using 1.2 of 8 cores. The cost lands on the
replica running the batch — which is the argument for the per-pool worker switch, and
for the adaptive back-off in [Flow control](#flow-control).

**Memory**: 15–40 MiB against the chart's 512Mi limit, even at 50,000 requests. The input
is streamed line by line and answers are appended as they arrive, so only the lines in
flight are held.

### Implementation Plan

Serial PRs, each independently reviewable:

1. Redis store, types and the Lua scripts
2. File store and `/v1/files`
3. `/v1/batches` and listing
4. Input validation
5. Executor, checkpointing and segments
6. Worker: claim, renew, reap, release
7. Router API extension seam and dispatch
8. Helm chart, startup guards and docs
9. Metrics
10. Adaptive flow control
11. E2E suite

### Alternatives

**A separate batch deployment.** Cleanest isolation, and batch load could never touch an
interactive replica. Rejected because it means a second deployable to scale and operate,
it duplicates the router's routing and scheduling or needs an RPC into it, and the issue
explicitly asks for a router-integrated design. Running a second router pool with the
worker enabled recovers most of the isolation benefit without a new component.

**Engine-native batch.** Would be less code. Not available: vLLM's online server has no
batch endpoints and SGLang's has none either. Waiting on upstream engines would block the
feature indefinitely.

**A Kubernetes Job per batch.** Kubernetes-native, and gets restart semantics for free.
Rejected because a batch would no longer flow through the router's scheduling path, so it
would lose KV-cache aware routing and the shared view of backend load, and because job
startup latency is poor for small batches.

**In-memory queue.** Simplest. Rejected outright: a batch would not survive a restart,
which defeats the purpose. In-memory implementations exist only in tests.

**HTTP loopback instead of an in-process handler.** Dispatching each line over localhost
to the router's own listener would reuse the path without the `APIExtension` seam.
Rejected for the extra serialization and connection overhead per line, and because the
identity would then have to travel in a header, where a client could forge it.
