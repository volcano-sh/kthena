-- Hands the running job KEYS[1] of the run with fence ARGV[1] back to the queue KEYS[2] at its created_at; a retry finds it queued.
local fence = redis.call('HGET', KEYS[1], 'fence')
if fence ~= ARGV[1] then
  if fence == '' and redis.call('ZSCORE', KEYS[2], ARGV[2]) then
    return 'OK'
  end
  return 'NOT_OWNER'
end
local status = redis.call('HGET', KEYS[1], 'status')
if status ~= 'validating' and status ~= 'in_progress' then
  return 'STATUS_CHANGED'
end
redis.call('HSET', KEYS[1], 'holder', '', 'fence', '')
redis.call('ZADD', KEYS[2], redis.call('HGET', KEYS[1], 'created_at'), ARGV[2])
return 'OK'
