import LightboxImage from '@site/src/components/LightboxImage';
import modelBoosterControllerArchitecture from '../assets/diagrams/architecture/model-booster-controller-architecture.svg';

# Model Booster Controller

## Overview

As an optional component of the Kthena project, the Model Booster Controller primarily provides users with a convenient deployment form - `Model`. Based on the `Model CR` (Model Custom Resource) information provided by users, this component can automatically configure and deploy the components required for inference services, such as router routing rules, inference engine instances, and dynamic scaling configurations.

## Feature Description

The Model Booster Controller provides one-stop deployment capabilities, enabling users to only need to be aware of `ModelBooster` information while ignoring the complex interaction specifications between various components within ModelServing, such as the use of some built-in labels and the binding relationships between CRDs. The following diagram illustrates the interaction principle of the Model Booster Controller:

<LightboxImage src={modelBoosterControllerArchitecture} alt="Architecture Overview"></LightboxImage>

The Model Booster Controller subscribes to the creation/modification/deletion events of the `ModelBooster` CR and synchronizes them to the CRs of `ModelServer`, `ModelRoute`, `ModelServing`, `AutoscalingPolicy`, and `AutoscalingPolicyBinding` that have the same semantic content as the corresponding events. All these CRs will carry the label `registry.volcano.sh/managed-by=registry.volcano.sh`.

### How Model Booster Controller works

Model Booster Controller watches for changes to `ModelBooster` CR in the Kubernetes cluster. When a `ModelBooster` CR is created or updated,
the controller performs the following steps:

1. Convert the `ModelBooster` CR to `ModelServing` CR, `ModelServer` CR, `ModelRoute` CR. `AutoscalingPolicy` CR and
   `AutoscalingPolicyBinding` CR are optional, only created when the `ModelBooster` CR has `autoscalingPolicy` defined.
2. Use the result of step 1 to create or update the `ModelServing`, `ModelServer`, `ModelRoute` , `AutoscalingPolicy`,
   `AutoscalingPolicyBinding`in the Kubernetes.
3. Set the conditions of `ModelBooster` CR.
    - After creating the related resources, the `Initialized` condition is set to `true`.
    - The controller then monitors the status of the `ModelServing` resources. Once all `ModelServing` resources are
      `Available`, the `Active` condition on the `ModelBooster` is set to `true`.
    - If any error occurs during the process, set the `Failed` condition to true and provide an error message.

The `OwnerReference` is set to the `ModelBooster` CR for all the created resources, so that when the `ModelBooster` CR is deleted, all
the related resources will be deleted as well.

## Example

Read the [Model Booster CR Examples](https://github.com/volcano-sh/kthena/tree/main/examples/model-booster) to learn more.

## Limitations

The `Model` can only cover most inference service scenarios and will be continuously updated. Some scenarios are not yet supported and require manual configuration of relevant CRDs. 

Please note the following limitations when using the Model Booster:
- Each `Model` can create only one `Model Route`.
- Rate limiting for `Model Route` is not supported.
- Topology configuration for `Model Infer` is not supported.
- The `panicPolicy` configuration for `AutoscalingPolicy` is not supported.
- Behavior configuration for `AutoscalingPolicy` is not supported.

In these cases, you can manually create `Model Infer`, `Model Server`, `Model Route`, `AutoscalingPolicy`, `AutoscalingPolicyBinding` resources as needed.