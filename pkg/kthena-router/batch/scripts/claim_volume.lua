-- Records ARGV[1] as the volume of these records in KEYS[1] unless one is there, and returns the recorded one.
redis.call('SET', KEYS[1], ARGV[1], 'NX')
return redis.call('GET', KEYS[1])
