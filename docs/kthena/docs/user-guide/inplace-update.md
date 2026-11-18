# In-place Update

By default, a ModelServing rolling update deletes and recreates each outdated `ServingGroup` or Role. For LLM inference this means rescheduling, reloading model weights, and warming caches again.

When an update only changes container images, for example a new vLLM or SGLang version, Kthena can update the Pods in place instead. The controller patches the images on the existing Pods, and kubelet restarts only the changed containers. Pods keep their name, UID, node, IP address, and volumes.

## Enable In-place Update

Set `spec.rolloutStrategy.updateMethod` to `InPlace`:

```yaml
spec:
  rolloutStrategy:
    type: ServingGroupRollingUpdate   # or RoleRollingUpdate
    updateMethod: InPlace             # default: Recreate
    progressDeadlineSeconds: 1800     # optional, default 1800, minimum 60
    rollingUpdateConfiguration:
      maxUnavailable: 1               # must resolve to at least 1
```

The rollout still respects `maxUnavailable` and `partition`. With `RoleRollingUpdate`, configure them on each Role, and only Roles whose template changed are updated.

Pods must use `restartPolicy: Always` (the default), and containers must not set their own `restartPolicy` or `restartPolicyRules`. Otherwise kubelet would not restart the container after the image changes.

## How It Works

Pods that move to the new revision together form an *update unit*: a whole `ServingGroup` with `ServingGroupRollingUpdate`, or one Role instance (entry Pod plus workers) with `RoleRollingUpdate`. For each unit selected by the rollout:

1. **Check.** If anything other than regular-container images changed, the unit is recreated instead (see [Fallback to Recreate](#fallback-to-recreate)).
2. **Patch.** Kthena patches the images on every Pod of the unit and records progress in the `modelserving.volcano.sh/inplace-update-state` Pod annotation. The unit now uses one slot of `maxUnavailable`.
3. **Restart.** Kubelet restarts each changed container with the new image. This one restart is expected and does not trigger `recoveryPolicy`.
4. **Complete.** Once every Pod in the unit is Ready on the new image, Kthena updates the Pods' `modelserving.volcano.sh/revision` label and moves on to the next unit.

A unit never finishes with mixed revisions. The update state lives on the Pods, so a restarted controller resumes unfinished updates.

## Example

Deploy [`examples/model-serving/updateMethod/inplace-update.yaml`](https://github.com/volcano-sh/kthena/blob/main/examples/model-serving/updateMethod/inplace-update.yaml). It creates two `ServingGroups`, each with a `prefill` and a `decode` Role, and uses `ServingGroupRollingUpdate` with `updateMethod: InPlace`:

```sh
kubectl apply -f examples/model-serving/updateMethod/inplace-update.yaml
```

After all Pods are Ready, change only the images:

```sh
kubectl patch modelserving sample-inplace --type=json -p '[
  {"op": "replace", "path": "/spec/template/roles/0/entryTemplate/spec/containers/0/image", "value": "nginx:1.27"},
  {"op": "replace", "path": "/spec/template/roles/0/workerTemplate/spec/containers/0/image", "value": "nginx:1.27"},
  {"op": "replace", "path": "/spec/template/roles/1/entryTemplate/spec/containers/0/image", "value": "nginx:1.27"},
  {"op": "replace", "path": "/spec/template/roles/1/workerTemplate/spec/containers/0/image", "value": "nginx:1.27"}
]'
```

With `maxUnavailable: 1`, the two `ServingGroups` are updated one after the other. When the rollout is done, the Pods have the same names and UIDs as before, run the new image, and show one restart each:

```sh
kubectl get pods -l modelserving.volcano.sh/name=sample-inplace \
  -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,IMAGE:.spec.containers[0].image,RESTARTS:.status.containerStatuses[0].restartCount'
```

For `RoleRollingUpdate`, see [`role-inplace-update.yaml`](https://github.com/volcano-sh/kthena/blob/main/examples/model-serving/updateMethod/role-inplace-update.yaml). Changing only the `prefill` images there leaves the `decode` Pods untouched.

## Fallback to Recreate

`InPlace` never blocks a rollout. When a unit cannot be updated in place, it is recreated as with `Recreate`, and an `InPlaceUpdateFallback` event on the ModelServing explains why. This happens when:

- The change is not image-only. Examples include init container or sidecar images, commands, env, resources, probes, volumes, Pod labels or annotations, `workerReplicas`, `schedulerName`, `spec.plugins`, and added or removed containers.
- A Pod or container restart policy prevents kubelet from restarting the container.
- The live Pod differs from its template, for example because a webhook renamed a container or rewrote an image.

Every outdated resource is recreated, with no fallback event, when:

- A plugin in `spec.plugins` does not support in-place updates. `lws-standard-labels` supports them; `headless-service` does not.
- `maxUnavailable` resolves to `0`. Under `RoleRollingUpdate`, this applies per Role. In that case `maxSurge` is used, as with `Recreate`.

:::note
With a single `ServingGroup` and a single Role replica, an in-place update makes the service unavailable until the Pods are Ready on the new image. To avoid that, use `maxUnavailable: 0` and `maxSurge: 1`, which recreates the replica next to a surge replica, or run more than one `ServingGroup`.
:::

## Failure Handling

If a unit is not Ready on the new image within `progressDeadlineSeconds`, Kthena emits a `Warning` event `InPlaceUpdateTimeout` and handles the unfinished Pods according to `recoveryPolicy`:

| `recoveryPolicy` | Result |
|---|---|
| `RoleRecreate` (default) | The Role instance is recreated after `restartGracePeriodSeconds`. |
| `ServingGroupRecreate` | The whole `ServingGroup` is recreated after `restartGracePeriodSeconds`. |
| `None`, or `restartGracePeriodSeconds: -1` | Pods are left alone; the update completes once they become Ready on the new image. |

Set the deadline longer than the time your engine needs to pull the image, load the model, and pass its readiness probe.

A crash beyond the expected restart, such as a crash loop on the new image, is handled by `recoveryPolicy` like any other failed Pod, without waiting for the deadline.

If you change the spec during an update, the latest spec always wins. A new image re-patches the unit. Reverting to the unit's previous revision, or switching to `updateMethod: Recreate`, recreates units whose in-place update is unfinished.

## Observability

- **Events** on the ModelServing (`kubectl describe modelserving <name>`): `InPlaceUpdateStarted`, `InPlaceUpdateCompleted`, `InPlaceUpdateFallback`, and `InPlaceUpdateTimeout`.
- **Pod annotation** `modelserving.volcano.sh/inplace-update-state`: it has no `"done": true` while the Pod is being updated. Do not edit it by hand.
- **Pod label** `modelserving.volcano.sh/revision`: it switches to the new revision only after the whole unit is complete.

## Related Documentation

- [Multi-node Inference: Rolling Update](./multi-node-inference.md#rolling-update)
- [ModelServing Plugin Framework](./modelserving-plugin-framework.md#interaction-with-in-place-image-updates)
- [Workload CRD reference](../reference/crd/workload.serving.volcano.sh.md)
