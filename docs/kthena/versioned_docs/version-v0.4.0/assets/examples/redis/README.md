# Redis Deployment for Kthena

This directory contains Redis deployment configuration for Kthena.

## When to Deploy Redis

Redis is required when using the following Kthena features:
- **KV Cache Aware Plugin** - For caching key-value pairs to improve performance
- **Global Rate Limit** - To share and synchronize the token counts across all router pods

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
