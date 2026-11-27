-- Records the counts ARGV[2..4] on the job KEYS[1] for the run with fence ARGV[1]; absolute values, so a retry changes nothing.
if redis.call('HGET', KEYS[1], 'fence') ~= ARGV[1] then
  return 'NOT_OWNER'
end
redis.call('HSET', KEYS[1], 'total', ARGV[2], 'completed', ARGV[3], 'failed', ARGV[4])
return 'OK'
