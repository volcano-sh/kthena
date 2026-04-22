# Redis Deployment for Kthena

This directory contains Redis deployment configuration for Kthena.

## When to Deploy Redis

Redis is required when using the following Kthena features:
- **KV Cache Aware Plugin** - For token-block based KV cache matching across pods via Redis. The Kthena Runtime sidecar writes token block hashes into Redis, and the router's `kvcache-aware` score plugin queries Redis to route requests to pods that already have matching cached blocks.
- **Global Rate Limit** - To share and synchronize the token counts across all router pods

## Enabling KV Cache Aware Plugin with Redis

After deploying Redis, you need to enable the `kvcache-aware` plugin in the router's scheduler configuration. Create or update the router ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kthena-router-config
  namespace: <namespace>
data:
  routerConfiguration: |-
    scheduler:
      pluginConfig:
      - name: kvcache-aware
        args:
          blockSizeToHash: 128
          maxBlocksToMatch: 128
      plugins:
        Score:
          enabled:
            - name: least-request
              weight: 1
            - name: kvcache-aware
              weight: 1
```

The `kvcache-aware` plugin also requires:
1. **Kthena Runtime sidecar** deployed alongside each vLLM pod (automatically set up by ModelBooster, or manually via ModelServing).
2. **Redis environment variables** (`REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`) available to both the runtime sidecar and the router pods.

For the full end-to-end setup guide, see the [KV Cache Aware Plugin user guide](../../docs/kthena/docs/user-guide/kvcache-aware.md).

## Quick Start

Deploy Redis using the provided configuration:

```bash
kubectl apply -f redis-standalone.yaml -n <namespace>
```

This will create:
- Redis server deployment
- Redis service
- Required ConfigMap and Secret for Kthena integration

## Configuration

The deployment creates the following resources that Kthena components automatically use:

- **ConfigMap** (`redis-config`): Contains Redis connection information
  - `REDIS_HOST`: `redis-server`
  - `REDIS_PORT`: `6379`

- **Secret** (`redis-secret`): Contains Redis authentication (empty password by default)

**Note**: If Redis is not deployed, Kthena components will start normally with Redis features disabled. All Redis environment variables are configured as optional.

## Production Considerations

The provided configuration is suitable for development and testing. For production environments, consider:

1. **High Availability**: Deploy Redis with replication or clustering
2. **Persistence**: Configure Redis persistence (RDB/AOF)
3. **Authentication**: Set up Redis password authentication
4. **Resource Limits**: Adjust CPU and memory limits based on your workload
5. **Monitoring**: Set up Redis monitoring and alerting
6. **Backup**: Configure regular backups

## Custom Redis Deployment

If you have an existing Redis deployment or prefer a different configuration:

1. Ensure Redis is accessible from the Kthena namespace
2. Create the required ConfigMap and Secret in the same namespace as Kthena:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-config
  namespace: <your-namespace>  # Same namespace as Kthena
data:
  REDIS_HOST: "your-redis-host"
  REDIS_PORT: "6379"
---
apiVersion: v1
kind: Secret
metadata:
  name: redis-secret
  namespace: <your-namespace>  # Same namespace as Kthena
type: Opaque
data:
  REDIS_PASSWORD: "base64-encoded-password"  # Use empty string "" for no password
```
