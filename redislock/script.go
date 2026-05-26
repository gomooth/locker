package redislock

import "github.com/redis/go-redis/v9"

// Lua 脚本：原子加锁（支持可重入 + Fencing Token）
// KEYS[1]=锁键, KEYS[2]=fence 键（hash tag 确保与 KEYS[1] 同 slot）
// ARGV[1]=owner, ARGV[2]=TTL(ms), ARGV[3]=fencing(1=启用,0=禁用)
// 返回值：0=被占用, -1=成功但 fencing token 生成失败, >0=成功
//
//	启用 fencing 时返回值为 token 值（≥1），禁用时返回 1。
//	重入（同一 owner）不生成新 token，返回 1。
//	使用 redis.pcall 调用 INCR，确保 INCR 异常时脚本不中断，
//	已执行的 HSET/PEXPIRE 不会被回滚，返回 -1 供调用方降级处理。
var lockScript = redis.NewScript(`
local key = KEYS[1]
local fence_key = KEYS[2]
local owner = ARGV[1]
local ttl = tonumber(ARGV[2])
local fencing = tonumber(ARGV[3])

if redis.call('EXISTS', key) == 0 then
    redis.call('HSET', key, 'owner', owner, 'count', 1)
    redis.call('PEXPIRE', key, ttl)
    if fencing == 1 then
        local token = redis.pcall('INCR', fence_key)
        if type(token) == 'number' then
            return token
        end
        return -1
    end
    return 1
end

local current_owner = redis.call('HGET', key, 'owner')
if current_owner == owner then
    redis.call('HINCRBY', key, 'count', 1)
    redis.call('PEXPIRE', key, ttl)
    return 1
end

return 0
`)

// Lua 脚本：原子解锁（支持可重入计数递减）
// 返回值：0=完全释放，>0=剩余计数，-1=key不存在，-2=非持有者
// 注意：当前 Go 实现中重入仅递增 localCnt，Redis count 始终为 1，
// 故 count > 0 分支在正常流程中不可达。保留此分支仅为防御性兼容。
// 部分解锁不重置 TTL：无看门狗时由 Go 侧显式调用 renewScript 续期，
// 有看门狗时由看门狗负责续期。
var unlockScript = redis.NewScript(`
local key = KEYS[1]
local owner = ARGV[1]

if redis.call('EXISTS', key) == 0 then
    return -1
end

local current_owner = redis.call('HGET', key, 'owner')
if current_owner ~= owner then
    return -2
end

local count = redis.call('HINCRBY', key, 'count', -1)
if count > 0 then
    return count
end

redis.call('DEL', key)
return 0
`)

// Lua 脚本：原子续期
// 返回值：1=成功，0=失败（key不存在或非持有者）
var renewScript = redis.NewScript(`
local key = KEYS[1]
local owner = ARGV[1]
local ttl = tonumber(ARGV[2])

if redis.call('EXISTS', key) == 0 then
    return 0
end

local current_owner = redis.call('HGET', key, 'owner')
if current_owner ~= owner then
    return 0
end

redis.call('PEXPIRE', key, ttl)
return 1
`)
