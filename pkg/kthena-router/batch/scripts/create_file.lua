-- KEYS: file, index. ARGV: index member, then the record as field and value pairs.
-- Stores a new file and indexes it in one step; a record that already exists is left as it is.
-- The record is written last, so an error reply always means no record was stored.
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
redis.call('ZADD', KEYS[2], 0, ARGV[1])
redis.call('HSET', KEYS[1], unpack(ARGV, 2))
return 1
