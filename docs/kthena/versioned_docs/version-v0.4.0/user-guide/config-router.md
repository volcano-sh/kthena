# Inference Router Customization

## Overview

ConfigMap is a Kubernetes API object used to store configuration data. Kthena Router uses ConfigMap to configure scheduler plugins and authentication settings, allowing users to customize router behavior without recompiling the code.

**NOTICE:** The ConfigMap must be prepared before launching the router pod, otherwise it will not take effect. Because we do not support hot reload.

## Configuration options

### Scheduler Configuration

The scheduler configuration includes plugin configurations and lists of enabled/disabled plugins.

Plugin Configuration (PluginConfig):

|Plugin Name| Parameters                                              |Description|
|-|---------------------------------------------------------|-|
|least-request| maxWaitingRequests                                      |Sets the maximum number of waiting requests|
|least-latency| TTFTTPOTWeightFactor                                    |Sets the weight factor for TTFT and TPOT|
|prefix-cache| blockSizeToHash<br />maxBlocksToMatch<br />maxHashCacheSize |Configures prefix cache parameters|

Filter Plugins (Filter):

|Configuration Name|Description|
|-|-|
|enabled|List of enabled filter plugins|
|disabled|List of disabled filter plugins|

Score Plugins (Score):

|Configuration Item|Description|
|-|-|
|enabled|List of enabled score plugins (with weights)|
|disabled|List of disabled score plugins|

### Authentication Configuration

Authentication configuration is used to enable and configure JWT authentication.

|Parameter|Type|Description|
|-|-|-|
|issuer|string|JWT issuer|
|audiences|[]string|JWT audiences list|
|jwksUri|string|Jwks Provider  URI|

<!-- Add routing rules here -->

## Examples

<!-- Add examples here -->
### Basic Scheduler Configuration

Here's a complete ConfigMap example showing how to configure the scheduler:

```yaml showLineNumbers
apiVersion: v1
kind: ConfigMap
metadata:
  name: kthena-router-config
  namespace: default
apiVersion: v1
kind: ConfigMap
metadata:
  name: kthena-router-config
  namespace: default
data:
  schedulerConfiguration: |-
    pluginConfig:
    - name: least-request
      args: 
        maxWaitingRequests: 10
    - name: least-latency
      args:
        TTFTTPOTWeightFactor: 0.5
    - name: prefix-cache
      args:
        blockSizeToHash: 64
        maxBlocksToMatch: 128
        maxHashCacheSize: 50000
    plugins:
      Filter:
        enabled:
          - least-request
        disabled:
          - lora-affinity
      Score:
        enabled:
          - name: least-request
            weight: 1
          - name: kv-cache
            weight: 1
          - name: least-latency
            weight: 1
          - name: prefix-cache
            weight: 1
```

If you want to use Authentication feature of router. Here is an example:

```yaml showLineNumbers
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "kthena.name" . }}-router-config
  namespace: {{ .Release.Namespace }}
data:
  routerConfiguration: |-
    scheduler:
      pluginConfig:
      - name: least-request
        args: 
          maxWaitingRequests: 10
      - name: least-latency
        args:
          TTFTTPOTWeightFactor: 0.5
      - name: prefix-cache
        args:
          blockSizeToHash: 64
          maxBlocksToMatch: 128
          maxHashCacheSize: 50000
      plugins:
        Filter:
          enabled:
            - least-request
          disabled:
            - lora-affinity
        Score:
          enabled:
            - name: least-request
              weight: 1
            - name: kv-cache
              weight: 1
            - name: least-latency
              weight: 1
            - name: prefix-cache
              weight: 1
    auth:
      issuer: "testing@secure.istio.io"
      audiences: ["kthena.io"]
      jwksUri: "https://raw.githubusercontent.com/istio/istio/release-1.27/security/tools/jwt/samples/jwks.json"
```

After creating or updating the ConfigMap, you need to restart the Router Pod for the configuration to take effect:

```bash
# Create ConfigMap
kubectl apply -f configmap.yaml

# Restart Router Pod
kubectl rollout restart deployment/kthena-router
```
