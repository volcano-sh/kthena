-- Marks the record KEYS[1] deleted, takes ARGV[1] out of the index KEYS[3], and says whether a batch in KEYS[2] still needs the bytes.
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 'NOT_FOUND'
end
redis.call('HSET', KEYS[1], 'deleted', '1')
redis.call('ZREM', KEYS[3], ARGV[1])
if redis.call('SCARD', KEYS[2]) == 0 then
  return 'REMOVABLE'
end
return 'HELD'
