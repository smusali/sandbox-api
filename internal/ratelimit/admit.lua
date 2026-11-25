local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
if not limit or limit < 1 or limit ~= math.floor(limit)
    or not window or window < 1 or window ~= math.floor(window) then
  return redis.error_reply("INVALID_LIMIT_POLICY")
end

local current = redis.pcall("GET", KEYS[1])
if type(current) == "table" then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if not current then
  redis.call("SET", KEYS[1], "1", "PX", window)
  return {1, window}
end

local ttl = redis.call("PTTL", KEYS[1])
if ttl < 0 then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if type(current) ~= "string"
    or not string.match(current, "^[1-9]%d*$")
    or #current > 15 then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end

local count = tonumber(current)
if not count or count < 1 or count ~= math.floor(count) then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if count >= limit then
  return {0, ttl}
end

redis.call("INCR", KEYS[1])
return {1, ttl}
