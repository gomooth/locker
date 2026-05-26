# locker

Go 分布式锁库，基于 Redis 实现。

## 特性

- 原子加锁/解锁（Lua 脚本）
- 可重入锁（本地计数优化，减少 Redis 交互）
- 看门狗自动续期（续期失败自动停止 + 恢复探测 + 锁丢失回调）
- Fencing Token（原子生成，防止延迟解锁问题）
- Context 取消支持
- 重试机制（bounded jitter 避免惊群效应）
- 密码学安全的 owner 标识
- 可观测性（Metrics + Tracing，支持 OpenTelemetry）
- 并发安全（per-key mutex 保护同一实例的并发 Lock/TryLock）

## 安装

```shell
go get github.com/gomooth/locker
```

## 锁实现

- [Redis Lock](redislock/README.md)

## 接口

```go
type ILocker interface {
    // Lock 获得锁（支持重试和 context 取消，Redis 错误也会重试）
    Lock(ctx context.Context, key string) error
    // TryLock 尝试获得锁，不重试
    TryLock(ctx context.Context, key string) error
    // UnLock 释放锁（可重入时递减本地计数，归零才完全释放）
    UnLock(ctx context.Context, key string) error
    // Close 释放锁实例资源（停止看门狗等）。
    // Close 不会主动释放 Redis 中的锁，锁将依赖 TTL 自然过期。
    // 调用 Close 后再调用 Lock/UnLock 将返回 ErrLockerClosed。
    Close() error
    // Wait 阻塞等待所有看门狗 goroutine 和锁丢失回调 goroutine 退出。
    // 必须在 Close() 之后调用，典型用法：lk.Close(); lk.Wait()
    Wait() error
}
```

### Fencing Token

```go
type FencingTokener interface {
    ILocker
    // Token 返回当前锁的 Fencing Token。
    // 必须在 Lock/TryLock 成功后调用。
    // 返回 0 表示未持有锁、Fencing Token 未启用或 token 生成失败。
    Token(key string) int64
}
```

Fencing Token 是单调递增的令牌，用于防止分布式锁的"延迟解锁"问题：当持有锁的进程因 GC 停顿等原因暂停后继续操作共享资源时，共享资源可通过校验 token 值拒绝旧请求。

Token 在加锁 Lua 脚本中原子生成（Redis INCR），确保加锁与 token 生成无间隔窗口。Fencing key 使用 hash tag（`{lock:key}:fence`）确保与锁键在同一 Redis Cluster slot。

使用方式：

```go
lk := redislock.New(client, redislock.WithFencingToken(true))

ft, ok := lk.(locker.FencingTokener)
if ok {
    err := lk.Lock(ctx, key)
    if err == nil {
        token := ft.Token(key) // 单调递增的 token
        // 将 token 传递给共享资源侧做校验
    }
}
```

共享资源侧应存储最近见过的最大 token 值，拒绝 token 小于已存储值的请求。

## 可观测性

### Metrics

```go
type Metrics interface {
    IncrementCounter(name string, attrs ...Attr)
    RecordDuration(name string, d time.Duration, attrs ...Attr)
}
```

指标名称约定：

| 名称 | 说明 |
|------|------|
| `lock.acquire` | 加锁成功 |
| `lock.acquire.fail` | 加锁失败 |
| `lock.acquire.fencing_fail` | Fencing Token 生成失败（加锁成功，但 fencing 降级） |
| `lock.acquire.duration` | 加锁耗时 |
| `lock.release` | 解锁 |
| `lock.renew` | 续期成功 |
| `lock.renew.fail` | 续期失败 |
| `lock.renew.duration` | 续期耗时 |
| `lock.lost` | 锁丢失 |

### Tracing

```go
type Tracer interface {
    StartSpan(ctx context.Context, name string, attrs ...Attr) (context.Context, Span)
}
```

Span 名称约定：

| 名称 | 说明 |
|------|------|
| `lock.Lock` | Lock 操作 |
| `lock.TryLock` | TryLock 操作 |
| `lock.UnLock` | UnLock 操作 |
| `lock.renew` | 看门狗续期 |
| `lock.lost` | 锁丢失 |

Renew 和 Lost span 以对应 Lock span 的 context 为父级，形成完整的 trace 链路。

### OpenTelemetry 适配器

```go
import lockerotel "github.com/gomooth/locker/otel"

metrics, err := lockerotel.NewMetricsProvider(otelMP)
if err != nil {
    // handle error
}

lk := redislock.New(client,
    redislock.WithTracer(lockerotel.NewTracerProvider(otelTP)),
    redislock.WithMetrics(metrics),
)
```

## 重入锁说明

同一 `ILocker` 实例对同一 key 的加锁为可重入。重入时仅递增本地计数，不访问 Redis。
如需不同协程独立竞争同一 key，应各自创建独立的 `ILocker` 实例。

### 并发安全性

同一 `ILocker` 实例的并发 `Lock`/`TryLock` 调用是安全的：内部使用 per-key mutex 串行化同一 key 的加锁流程，确保本地重入计数与 Redis 状态一致。

注意事项：
- 并发 `Lock` 同一 key 时，后到的 goroutine 会等待前一个完成，然后作为重入处理
- 并发 `TryLock` 同一 key 时，若另一 goroutine 正在获取中，`TryLock` 会返回 `ErrLockOccupied`
- 共享同一实例的协程可以互相解锁，这是设计预期行为

## 关闭与资源回收

```go
lk.Close() // 停止所有看门狗，标记实例为 closed
lk.Wait()  // 阻塞等待所有 goroutine 退出
```

- `Close()` 不会主动释放 Redis 中的锁，锁将依赖 TTL 自然过期
- `Wait()` 必须在 `Close()` 之后调用
- Close 后的 Lock/UnLock 调用返回 `ErrLockerClosed`
