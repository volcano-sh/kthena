-- Indexes ARGV[1] in KEYS[2] and then writes the record KEYS[1] from ARGV[2..], so an error leaves no record; an existing one stays.
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
redis.call('ZADD', KEYS[2], 0, ARGV[1])
redis.call('HSET', KEYS[1], unpack(ARGV, 2))
return 1
