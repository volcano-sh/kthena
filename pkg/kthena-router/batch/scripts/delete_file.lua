-- KEYS: file, fileref, index. ARGV: index member.
-- Marks the file deleted and reports whether a batch still needs its bytes, in one step.
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 'NOT_FOUND'
end
redis.call('HSET', KEYS[1], 'deleted', '1')
redis.call('ZREM', KEYS[3], ARGV[1])
if redis.call('SCARD', KEYS[2]) == 0 then
  return 'REMOVABLE'
end
return 'HELD'
