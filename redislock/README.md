# redislock

基于 Redis 的分布式锁，支持可重入和看门狗自动续期。

## 特性

- **原子操作**：加锁、解锁、续期均使用 Lua 脚本，保证 Redis 端原子性
- **可重入**：同一 owner 对同一 key 可多次加锁，计数递减至零才完全释放
- **看门狗**：自动续期，防止业务未完成锁已过期
- **恢复探测**：续期达到最大失败次数后，用更长超时做最后一次尝试，避免短暂网络抖动误报锁丢失
- **Fencing Token**：原子生成单调递增令牌，防止延迟解锁问题
- **重试**：支持配置重试次数和间隔
- **Context**：支持取消和超时
- **并发安全**：per-key mutex 保护同一实例的并发 Lock/TryLock

## 选项

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `WithTimeout(d)` | 锁超时时间 | 30s |
| `WithRetry(n, d)` | 重试次数和间隔 | 0（不重试） |
| `WithWatchDog(b)` | 是否启用看门狗 | true |
| `WithMaxRenewFailures(n)` | 看门狗续期最大连续失败次数 | 3 |
| `WithOnLockLost(fn)` | 锁丢失回调（独立 goroutine 执行） | 无 |
| `WithKeyPrefix(s)` | Redis key 前缀 | `"lock:"` |
| `WithRenewTimeout(d)` | 看门狗续期超时 | timeout/2 |
| `WithRecoveryProbe(b)` | 续期最大失败后是否执行恢复探测 | true |
| `WithFencingToken(b)` | 启用 Fencing Token | false |

## 示例

```go
package main

import (
    "context"
    "time"

    "github.com/gomooth/locker/lockhelper"
    "github.com/gomooth/locker/redislock"
    "github.com/redis/go-redis/v9"
)

func main() {
    client := redis.NewClient(&redis.Options{
        Addr: "127.0.0.1:6379",
    })

    lock := redislock.New(client,
        redislock.WithTimeout(30*time.Second),
        redislock.WithWatchDog(true),
    )

    ctx := context.Background()
    key := lockhelper.Key("app", "order", "12345")

    // 加锁（支持重试）
    if err := lock.Lock(ctx, key); err != nil {
        panic(err)
    }
    defer lock.UnLock(ctx, key)

    // 或一次性尝试
    // if err := lock.TryLock(ctx, key); err != nil { ... }

    // 可重入：同一实例对同一 key 再次加锁不会阻塞
    if err := lock.Lock(ctx, key); err != nil {
        panic(err)
    }
    // 需要对应次数的解锁
    lock.UnLock(ctx, key) // 计数递减
    lock.UnLock(ctx, key) // 完全释放
}
```

## Fencing Token

启用后，每次加锁通过 Lua 脚本内的 `INCR` 原子生成单调递增的 token，可通过 `Token(key)` 获取：

```go
lock := redislock.New(client, redislock.WithFencingToken(true))

ft, ok := lock.(locker.FencingTokener)
if ok {
    lock.Lock(ctx, key)
    token := ft.Token(key) // 单调递增
    lock.UnLock(ctx, key)
}
```

Fencing key 使用 hash tag（`{lock:key}:fence`）确保与锁键在同一 Redis Cluster slot，兼容集群模式。

## 关闭与资源回收

```go
lock.Close() // 停止所有看门狗，标记 closed
lock.Wait()  // 阻塞等待所有 goroutine 退出
```

- `Close()` 不会主动释放 Redis 中的锁，锁将依赖 TTL 自然过期
- `Wait()` 必须在 `Close()` 之后调用
- Close 后的 Lock/UnLock 返回 `ErrLockerClosed`

## lockhelper.Key

纯拼接工具，用 `:` 连接各部分，不含前缀：

```go
lockhelper.Key("app", "order", "12345")
// 输出: "app:order:12345"
// Redis 中实际存储为: "lock:app:order:12345"（默认前缀）
```

## Key 前缀

默认 Redis key 前缀为 `"lock:"`，可通过 `WithKeyPrefix` 自定义：

```go
lock := redislock.New(client,
    redislock.WithKeyPrefix("myapp:lock:"),
)
// Redis key: "myapp:lock:app:order:12345"

// 允许空前缀
lock := redislock.New(client,
    redislock.WithKeyPrefix(""),
)
// Redis key: "app:order:12345"
```
