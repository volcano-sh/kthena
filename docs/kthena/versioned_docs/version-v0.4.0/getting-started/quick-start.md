---
sidebar_position: 2
---
import QuickStartYaml from '../assets/examples/model-booster/Qwen2.5-0.5B-Instruct.yaml?raw';
import CodeBlock from '@theme/CodeBlock';

# Quick Start

Get up and running with Kthena in minutes! This guide will walk you through deploying your first AI model.
We'll install a model from Hugging Face and perform inference using a simple curl command.

We have two optional ways to quickly start using kthena to deploy LLM.

1. ModelBooster
2. ModelServing

## Prerequisites

- Kthena installed on your Kubernetes cluster (see [Installation](./installation.md))
- Access to a Kubernetes cluster with `kubectl` configured
- Pod in Kubernetes can access the internet
- [volcano](https://volcano.sh/en/docs/installation/) is installed.

## ModelBooster

Kthena ModelBooster is a Custom Resource Definitions(CRD) of Kthena that provides a simple way to deploy LLMs. It allows you to deploy LLMs with a single click.

**Step 1: Create a ModelBooster Resource**

Create the example model in your namespace (replace `<your-namespace>` with your actual namespace):

```shell
kubectl apply -n <your-namespace> -f https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/model-booster/Qwen2.5-0.5B-Instruct.yaml
```

Content of the Model:

<CodeBlock language="yaml" showLineNumbers>
    {QuickStartYaml}
</CodeBlock>

**Step 2: Wait for Model to be Ready**

Wait model condition `Active` to become `true`. You can check the status using:

```bash
kubectl get modelBooster demo -o jsonpath='{.status.conditions}' -n <your-namespace>
```

And the status section should look like this when the model is ready:

```json
[
  {
    "lastTransitionTime": "2025-09-05T02:14:16Z",
    "message": "Model initialized",
    "reason": "ModelCreating",
    "status": "True",
    "type": "Initialized"
  },
  {
    "lastTransitionTime": "2025-09-05T02:18:46Z",
    "message": "Model is ready",
    "reason": "ModelAvailable",
    "status": "True",
    "type": "Active"
  }
]
```

**Step 3: Perform Inference**

You can now perform inference using the model. Here's an example of how to send a request:

```bash
curl -X POST http://<model-route-ip>/v1/chat/completions \
-H "Content-Type: application/json" \
-d '{
  "model": "demo",
  "messages": [
    {
      "role": "user",
      "content": "Where is the capital of China?"
    }
  ],
  "stream": false
}'
```

Use the following command to get the `<model-route-ip>`:

```bash
kubectl get svc kthena-router -o jsonpath='{.spec.clusterIP}' -n <your-namespace>
```

This IP can only be used inside the cluster. If you want to chat from outside the cluster, you can use the `EXTERNAL-IP`
of `kthena-router` after you bind it.

## ModelServing

In addition to using Kthena with a single click via modelBooster, you can also flexibly configure your own LLM through modelServing.

Model Serving Controller is a component of Kthena that provides a flexible and customizable way to deploy LLMs. It allows you to configure your own LLM through `ModelServing` CRD. ModelServing supports deploying large language models (LLMs) based on roles, with support for gang scheduling and network topology scheduling. It also provides fundamental features such as scaling and rolling updates.

Here is an [example](https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/model-serving/gpu-PD.yaml) of deploying the PD-disaggregation Qwen-8B Model on GPU Using `ModelServing`.

**Step 1: Create a ModelServing Resource Object:**

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/model-serving/gpu-pd-disaggregation.yaml
```

**Step 2: Wait for ModelServing to be Ready**

After all Pods awaiting deployment have started running, you can run the following command to see the result:

```sh
kubectl get po

NAMESPACE            NAME                                          READY   STATUS    RESTARTS   AGE
default              PD-sample-0-decode-0-0                        1/1     Running   0          2m
default              PD-sample-0-prefill-0-0                       1/1     Running   0          2m

------------------------------------------

kubectl get modelserving sample -o jsonpath='{.status.conditions}' | jq '.' 

[
  {
    "lastTransitionTime": "2025-09-29T08:11:16Z",
    "message": "Some groups is progressing: [0]",
    "reason": "GroupProgressing",
    "status": "False",
    "type": "Progressing"
  },
  {
    "lastTransitionTime": "2025-09-29T08:11:21Z",
    "message": "All Serving groups are ready",
    "reason": "AllGroupsReady",
    "status": "True",
    "type": "Available"
  }
]
```

**Step 3: Perform Inference**

Before you can perform inference, you need to create `ModelRoute` and `ModelServer`. You can refer to [modelRoute Configuration](../user-guide/prefill-decode-disaggregation/vllm-ascend-mooncake.md#3-modelroute-configuration) and [modelServer Configuration](../user-guide/prefill-decode-disaggregation/vllm-ascend-mooncake.md#2-modelserver-configuration).

Then you can use the following command to send a request:

```bash
export MODEL="models/Qwen3-8B"

curl http://$ROUTER_IP/v1/completions -H "Content-Type: application/json" -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
}"
```
