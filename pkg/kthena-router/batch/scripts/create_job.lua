-- Checks the input file KEYS[1], holds it in KEYS[2] for batch ARGV[2], queues it in KEYS[4] and writes the job KEYS[3] last, all at once.
if redis.call('EXISTS', KEYS[3]) == 1 then
  return 'EXISTS'
end
local file = redis.call('HMGET', KEYS[1], 'tenant', 'deleted', 'expires_at', 'purpose')
if file[1] ~= ARGV[1] or file[2] == '1' then
  return 'NO_FILE'
end
local expires = tonumber(file[3]) or 0
if expires > 0 and tonumber(redis.call('TIME')[1]) >= expires then
  return 'NO_FILE'
end
if file[4] ~= 'batch' then
  return 'NOT_BATCH'
end
redis.call('SADD', KEYS[2], ARGV[2])
redis.call('ZADD', KEYS[4], ARGV[3], ARGV[2])
redis.call('HSET', KEYS[3], unpack(ARGV, 4))
return 'OK'
