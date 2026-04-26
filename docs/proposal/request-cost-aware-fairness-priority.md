---
title: Request Cost Aware Fairness Priority
authors:
- "@JagjeevanAK"
reviewers:
- "@hzxuzhonghu"
- TBD
approvers:
- "@hzxuzhonghu"
- TBD

creation-date: 2026-04-23

---

## Request Cost Aware Fairness Priority

### Summary

This proposal improves Kthena router fairness scheduling by including the
estimated cost of the current request in the enqueue priority. The existing
fairness model already tracks historical usage in a sliding window after
requests complete. However, enqueue priority based only on historical usage can
rank a small request and a large request from users with the same historical
usage equally, even though the large request is expected to consume more
capacity.

The proposed score is:

```text
estimatedRequestCost = inputWeight * inputTokens + outputWeight * requestedOutputTokens
finalPriority = historicalUsage + estimatedRequestCost
```

Lower numeric priority continues to mean higher scheduling priority. The
historical component is the current fairness priority for the request's
`(user, model)` pair, computed from the existing sliding-window tracker and
priority weights. The current request cost is an enqueue-time estimate and does
not replace post-response accounting.

### Motivation

Fairness scheduling should account for both recent completed work and the
expected cost of the request currently entering the queue. Without the current
request estimate, two users with the same historical usage are treated the same
at enqueue time even when one request asks for a much larger output budget.

This proposal also clarifies terminology from the design discussion in `#757`:
`historicalUsage` means the existing fairness score derived from the
sliding-window tracker for `(user, model)`. It is not a new persisted field and
it is not the estimated cost of the request being enqueued.

#### Goals

1. Add current request cost to fairness enqueue priority.
2. Keep existing lower-value-is-higher-priority queue semantics.
3. Keep historical fairness state based on the sliding-window token tracker.
4. Preserve request-size sensitivity when queued priorities are refreshed.
5. Keep the change internal to router/datastore fairness scheduling.
6. Add focused unit coverage for parsing, priority calculation, and refresh
   behavior.

#### Non-Goals

1. Redesign cross-model fairness.
2. Add distributed or HA fairness state.
3. Add new CRDs, Helm values, or public API fields.
4. Redesign queue refresh, cancellation, or dequeue rate control beyond what is
   required to preserve the new request-cost component.
5. Replace post-response token accounting.
6. Change scheduler behavior outside the fairness queue.

### Proposal

At enqueue time, the router calculates a request-cost-aware priority from two
parts:

1. Historical usage: the same historical fairness score already used by queue
   refresh and rebuild, derived from recent weighted usage for the same
   `(user, model)`.
2. Estimated request cost: weighted input tokens plus requested output token
   budget from the current request.

The router then enqueues the request with:

```text
priority = historicalUsage + estimatedRequestCost
```

The estimated request cost is also stored as a per-request priority offset so
dequeue-time priority refresh can recompute the historical component while still
keeping the current request's size in the final score:

```text
refreshedPriority = refreshedHistoricalPriority + estimatedRequestCost
```

This avoids a mismatch where enqueue ordering accounts for request size, but a
later refresh drops that request-size component.

#### Requested Output Token Resolution

The router resolves the requested output token budget in this order:

1. `max_completion_tokens`
2. `max_tokens`
3. `0` when neither field is present

Missing, invalid, negative, `NaN`, or infinite values are treated as `0`. Token
budget values should be handled as numeric token counts before they are used in
priority calculations.

#### Configuration Scope

This proposal uses the existing fairness token cost weights for the enqueue-time
request estimate:

```text
FAIRNESS_INPUT_TOKEN_WEIGHT
FAIRNESS_OUTPUT_TOKEN_WEIGHT
```

When these variables are unset, the enqueue-time calculation should default
both input and output token weights to `1.0`. This avoids introducing a silent
asymmetric default for output tokens in router-side priority calculation.

It also preserves the existing historical fairness configuration for enqueue,
refresh, and rebuild:

```text
FAIRNESS_PRIORITY_TOKEN_WEIGHT
FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT
```

This means enqueue continues to honor existing priority-weight tuning for the
historical component, while the new request-cost component remains based on the
input/output token weights above. This proposal does not introduce CRD, CLI, or
Helm value changes.

#### Observability

Add one debug log at enqueue time at `V(4)` with the priority components:

```text
requestID, userID, model, historicalUsage, inputTokens,
requestedOutputTokens, estimatedRequestCost, finalPriority
```

No new metrics are required for this proposal.

### Design Details

#### Priority Components

`historicalUsage` is the existing fairness score for the `(user, model)` pair:

```text
historicalUsage =
  FAIRNESS_PRIORITY_TOKEN_WEIGHT * weightedTokenUsage +
  FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT * requestCount
```

with the same defaults and semantics already used by datastore priority refresh.
This keeps enqueue and refresh calculations aligned.

`estimatedRequestCost` is only the current request's estimated cost:

```text
inputWeight * inputTokens + outputWeight * requestedOutputTokens
```

These two values are added for enqueue ordering. The request-cost estimate
reuses `FAIRNESS_INPUT_TOKEN_WEIGHT` and `FAIRNESS_OUTPUT_TOKEN_WEIGHT`, which
are also used by the token tracker when recording weighted token usage; this is
intentional so both terms stay in the same weighted-token units. After the
request completes, the existing post-response accounting continues to update
real usage in the token tracker.

#### Refresh Behavior

If the fairness queue refreshes or rebuilds priorities while requests are
waiting, it must preserve the current request cost. The refresh path should
recompute the historical part from the current token tracker state, then add the
stored request-cost offset.

This keeps the enqueue score and refreshed score consistent:

```text
enqueuePriority = historicalPriorityAtEnqueue + requestCostOffset
refreshedPriority = historicalPriorityAtRefresh + requestCostOffset
```

#### Test Plan

Unit tests should cover:

1. Requested output token parsing for `max_completion_tokens`, `max_tokens`,
   missing values, invalid values, and negative values.
2. Weight application for input and requested output tokens.
3. Final score calculation with both historical usage and estimated request
   cost.
4. Request-size sensitivity for users with the same historical usage.
5. Fallback to input-only cost when output budget is missing or zero.
6. Historical-score consistency with configured priority token/request-count
   weights.
7. Preservation of the request-cost offset during priority refresh and heap
   rebuild, including the unchanged-final-priority equality case.
8. Debug logging gate behavior where practical.

E2E and load tests are out of scope for this narrowly scoped proposal.

### Alternatives

#### Keep Historical Usage Only

The simplest option is to keep enqueue priority based only on historical usage.
This preserves current behavior but does not distinguish between small and large
requests from users with the same recent usage.

#### Charge Estimated Cost Only After Completion

Another option is to keep all accounting post-response. This remains accurate
for completed usage, but admission ordering cannot account for the current
request's expected cost while the request is waiting in the queue.

#### Redesign Fairness Configuration In This Change

The router and datastore fairness paths include both input/output token weights
and priority token/request-count weights. This proposal keeps those existing
configuration surfaces and aligns enqueue with refresh/rebuild rather than
trying to collapse them into a new model. A broader deprecation or simplification
of the fairness knobs can be evaluated separately.
