---
slug: model-booster-controller-blog-post
title: All you should know about model-booster controller
authors: [ huntersman ]
tags: [ ]
---

# All you should know about model-booster controller

## 1. Brief Introduction

Model-booster controller is a k8s [operator](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) that helps
you manage your model deployments. It automates the deployment of models in a Kubernetes
cluster. If you don't use model-booster to manager models in Kthena, you need to create `ModelServing`,
`AutoscalingPolicy`,`AutoscalingPolicyBinding`, `ModelRoute`, `ModelServer` one by one.
Model-booster controller will help you create these resources automatically based on the `ModelBooster` custom resource.

There are three kinds of conditions for `ModelBooster` custom resource:

- `Initialized`: The `ModelBooster` CR passed validation and is processing.
- `Active`: Everything is ok, now you can use the model.
- `Failed`: There is something wrong, check the message for details.

We use `true`, `false` and `unknown` to represent the status of each condition. `true` means the condition is met,
`false` means the
condition is not met, and `unknown` means we don't know for yet.

In kthena version v0.1.0, we only support `vLLM` as the model backend. In the future, we will support `SGLang`, `MindIE`.

## 2. Core Features

### 2.1 Automated Deployment

With model-booster controller, you can create/update/delete models with a single
`ModelBooster` custom resource, the controller will create/update/delete the corresponding `ModelServing`,
`AutoscalingPolicy`,`AutoscalingPolicyBinding`, `ModelRoute`, `ModelServer` resources automatically.

`ModelBooster` supports models from different sources, including:
- `HuggingFace`(recommend): Models hosted on HuggingFace.
- `S3`: Models stored in S3-compatible storage.
- `PVC`: Models stored in Persistent Volume Claims.

When you first deploy a model using `ModelBooster`, the controller will download the model from the sources into the `cache` you specified, next time when you deploy the same model, the controller will use the cached model to speed up the deployment.

### 2.2 Validation

The webhook and the controller will validate the `ModelBooster` custom resource before
creating/updating/deleting the corresponding resources to ensure that the resource is valid.

You can find the validations of CR in
the [CRD Reference](https://kthena.volcano.sh/docs/reference/crd/workload.serving.volcano.sh#modelbooster)

### 2.3 Reconciliation

The controller will continuously monitor the state of the `ModelBooster` custom resource and the
corresponding resources, and will take action to ensure that the desired state is always maintained. And when all the
resources are ready, the controller will update the condition of the `ModelBooster` custom resource to `Active`.

### 2.4 Best Practice templates

We provide some best practice templates for `ModelBooster` custom resource to help you get started quickly. Including:
- DeepSeek-R1
- DeepSeek-R1-Distill-Qwen-7B
- DeepSeek-R1-Distill-Qwen-32B
- gemma-2-2b-it
- gemma-2-27b-it
- gemma-3-4b-it
- gemma-3-27b-it
- Llama-3.2-1B-Instruct
- Llama-3.3-70B-Instruct
- Llama-4-Maverick-17B-128E-Instruct-FP8
- Llama-4-Scout-17B-16E-Instruct
- Meta-Llama-3-8B
- Mistral-Small-24B-Instruct-2501
- Mixtral-8x7B-Instruct-v0.1
- Mixtral-8x22B-Instruct-v0.1
- gpt-oss-20b
- gpt-oss-120b
- Qwen3-32B

For more details, please refer to the [CLI documentation](https://github.com/volcano-sh/kthena/blob/main/cli/kthena/README.md).

## 3. Conclusion

Model-booster controller simplifies the management of model deployments in Kthena. It automates the creation and
management of multiple resources, ensuring that your models are always deployed and running as expected.
