-- Moves the job KEYS[1] from ARGV[2] to ARGV[3] for the run with fence ARGV[1]; a final move drops batch ARGV[6] from its input's holders KEYS[2].
if redis.call('HGET', KEYS[1], 'fence') ~= ARGV[1] then
  return 'NOT_OWNER'
end
local status = redis.call('HGET', KEYS[1], 'status')
if status == ARGV[3] then
  return 'OK'
end
if status ~= ARGV[2] then
  return 'STATUS_CHANGED'
end
local fields = {'status', ARGV[3]}
if ARGV[4] ~= '' then
  fields[#fields + 1] = ARGV[4]
  fields[#fields + 1] = redis.call('TIME')[1]
end
for i = 7, #ARGV do
  fields[#fields + 1] = ARGV[i]
end
redis.call('HSET', KEYS[1], unpack(fields))
if ARGV[5] == '1' then
  redis.call('SREM', KEYS[2], ARGV[6])
end
return 'OK'
