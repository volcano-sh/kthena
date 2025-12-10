# Model Deployment

## ModelBooster vs ModelServing Deployment Approaches

Kthena provides two approaches for deploying LLM inference workloads: the **ModelBooster approach** and the **ModelServing approach**. This section compares both approaches to help you choose the right one for your use case.

### Deployment Approach Comparison

| Deployment Method | Manually Created CRDs                 | Automatically Managed Components        | Use Case                                     |
|-------------------|---------------------------------------|-----------------------------------------|----------------------------------------------|
| **ModelBooster**  | ModelBooster                          | ModelServer, ModelRoute, Pod Management | Simplified deployment, automated management  |
| **ModelServing**  | ModelServing, ModelServer, ModelRoute | Pod Management                          | Fine-grained control, complex configurations |

### ModelBooster Approach

**Advantages:**

- Simplified configuration with built-in disaggregation support optimized for NPUs
- Automatic KV cache transfer configuration using NPU-optimized protocols
- Integrated support for Huawei Ascend NPUs with automatic resource allocation
- Streamlined deployment process with NPU-specific optimizations
- Built-in HCCL (Huawei Collective Communication Library) configuration

**Automatically Managed Components:**

- ✅ ModelServer (automatically created and managed with NPU awareness)
- ✅ ModelRoute (automatically created and managed)
- ✅ Inter-service communication configuration (HCCL-optimized)
- ✅ Load balancing and routing for NPU workloads
- ✅ NPU resource scheduling and allocation

**User Only Needs to Create:**

- ModelBooster CRD with NPU resource specifications

### ModelServing Approach

**Advantages:**

- Fine-grained control over NPU container configuration
- Support for init containers and complex volume mounts for NPU drivers
- Detailed environment variable configuration for Ascend NPU settings
- Flexible NPU resource allocation (`huawei.com/ascend-1980`)
- Custom HCCL network interface configuration

**Manually Created Components:**

- ❌ ModelServing CRD with NPU resource specifications
- ❌ ModelServer CRD with NPU-aware workload selection
- ❌ ModelRoute CRD for NPU service routing
- ❌ Manual inter-service communication configuration (HCCL settings)

**NPU-Specific Networking Components:**

- **ModelServer** - Manages inter-service communication and load balancing for NPU workloads
- **ModelRoute** - Provides request routing and traffic distribution to NPU services
- **Supported KV Connector Types** - nixl, mooncake (optimized for NPU communication)
- **HCCL Integration** - Huawei Collective Communication Library for NPU-to-NPU communication

### Selection Guidance

- **Recommended: Use ModelBooster Approach** - Suitable for most NPU deployment scenarios, simple deployment, high automation with NPU optimization
- **Use ModelServing Approach** - Only when fine-grained NPU control or special Ascend-specific configurations are required

## ModelBooster Lifecycle

`ModelBooster` CR has several conditions that indicate the status of the model. These conditions are:

| ConditionType | Description                                                          |
|---------------|----------------------------------------------------------------------|
| `Initialized` | The ModelBooster CR has been validated and is waiting to be processed.      |
| `Active`      | The ModelBooster is ready for inference.                                    |
| `Failed`      | The ModelBooster failed to become active. See the message for more details. |

## Examples

You can find examples of model booster CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-booster), and model serving CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-serving).

## Advanced features

### Gang Scheduling

`GangPolicy` is enabled by default, we may make it optional in future release.
