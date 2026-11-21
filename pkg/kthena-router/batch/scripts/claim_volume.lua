-- KEYS: volume. ARGV: the volume ID of the caller.
-- Records the first volume these records belong to, and returns the one recorded.
redis.call('SET', KEYS[1], ARGV[1], 'NX')
return redis.call('GET', KEYS[1])
