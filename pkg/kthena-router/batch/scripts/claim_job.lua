-- Takes the oldest runnable batch in KEYS[1] for holder ARGV[2] with fence ARGV[3], noted in KEYS[2] so a retry returns the same one.
local claimed = redis.call('GET', KEYS[2])
if claimed then
  return claimed
end
for _ = 1, tonumber(ARGV[4]) do
  local item = redis.call('ZPOPMIN', KEYS[1])
  if #item == 0 then
    return false
  end
  local job = ARGV[1] .. 'job:' .. item[1]
  local status = redis.call('HGET', job, 'status')
  if status == 'validating' or status == 'in_progress' then
    redis.call('HSET', job, 'holder', ARGV[2], 'fence', ARGV[3])
    if status == 'validating' then
      redis.call('HSET', job, 'status', 'in_progress', 'in_progress_at', redis.call('TIME')[1])
    end
    redis.call('SET', KEYS[2], item[1], 'EX', 600)
    return item[1]
  end
end
return false
