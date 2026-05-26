package redislock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomooth/locker"

	"github.com/redis/go-redis/v9"

	"github.com/gomooth/xerror"
)

// LockLostFunc 锁丢失回调函数类型
// 当看门狗续期失败时调用，key 为原始键名（不含前缀）
// 注意：回调在独立 goroutine 中执行，需保证线程安全
type LockLostFunc func(key string)

// lockEntry 记录单个锁的本地状态
type lockEntry struct {
	stopCh chan struct{} // 看门狗停止信号
	// Lock/TryLock 时的 trace context，用于续期/锁丢失创建子 span 时传播 trace 父子关系
	// 设计说明：OTel SpanContext 是不可变的，Lock span End() 后其 traceID/spanID 仍可传播，
	// 因此看门狗在 Lock span 结束后使用此 context 创建 Renew/Lost 子 span 是安全的。
	// 注意：仅通过 StartSpan 传入属性，不在 End() 后修改 span，符合 OTel 规范。
	traceCtx  context.Context
	localCnt  int   // 本地重入计数
	unlocking bool  // 标记正在主动解锁，防止看门狗误触 onLockLost
	token     int64 // Fencing Token，0 表示未启用或生成失败
}

type distributedRedisLock struct {
	client           redis.UniversalClient
	owner            string
	timeout          time.Duration
	maxRetries       int
	retryInterval    time.Duration
	watchDog         bool
	maxRenewFailures int
	onLockLost       LockLostFunc
	keyPrefix        string
	renewTimeout     time.Duration
	recoveryProbe    bool
	fencingEnabled   bool

	mu      sync.Mutex
	wg      sync.WaitGroup
	entries map[string]*lockEntry // key = wrappedKey
	closed  atomic.Bool           // Close() 调用后为 true

	// keyMu 为每个 wrappedKey 提供独立的互斥锁，
	// 串行化同一实例对同一 key 的 Lock/TryLock/UnLock 操作，
	// 防止并发 Lock 与 UnLock 时 localCnt 与 Redis count 不一致。
	keyMu *keyMuMap

	metrics locker.Metrics
	tracer  locker.Tracer
}

// New 创建分布式 redis 锁
func New(client redis.UniversalClient, opts ...func(*distributedRedisLock)) locker.ILocker {
	lock := &distributedRedisLock{
		client:           client,
		owner:            generateOwnerID(),
		timeout:          defaultTimeout,
		maxRetries:       defaultRetryCount,
		retryInterval:    defaultRetryInterval,
		watchDog:         defaultWatchDog,
		maxRenewFailures: defaultMaxRenewFailures,
		keyPrefix:        defaultKeyPrefix,
		renewTimeout:     defaultRenewTimeout,
		recoveryProbe:    defaultRecoveryProbe,
		entries:          make(map[string]*lockEntry),
		keyMu:            newKeyMuMap(),
	}

	for _, opt := range opts {
		opt(lock)
	}

	return lock
}

func generateOwnerID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read 在 Go 1.20+ 中失败会 panic 而非返回 error
	return hex.EncodeToString(b)
}

func (d *distributedRedisLock) wrapKey(key string) string {
	return d.keyPrefix + key
}

func (d *distributedRedisLock) validateKey(key string) (string, error) {
	if key == "" {
		return "", locker.ErrEmptyKey
	}
	return d.wrapKey(key), nil
}

// fenceKey 生成 Fencing Token 的 Redis 键名。
// 使用 hash tag {wrappedKey} 确保与锁键在同一 slot，兼容 Redis Cluster。
func (d *distributedRedisLock) fenceKey(wrappedKey string) string {
	return "{" + wrappedKey + "}:fence"
}

// fencingInt 返回 fencing 启用标志的整数值，用于 Lua 脚本 ARGV
func (d *distributedRedisLock) fencingInt() int {
	if d.fencingEnabled {
		return 1
	}
	return 0
}

// getRenewTimeout 返回续期超时，0 表示使用 timeout/2
func (d *distributedRedisLock) getRenewTimeout() time.Duration {
	if d.renewTimeout > 0 {
		return d.renewTimeout
	}
	return d.timeout / 2
}

// tryReentrant 检查是否可本地重入：entry 存在且非 unlocking 状态时递增 localCnt 并返回 true
func (d *distributedRedisLock) tryReentrant(wrappedKey string) bool {
	d.mu.Lock()
	if entry, exists := d.entries[wrappedKey]; exists && !entry.unlocking {
		entry.localCnt++
		d.mu.Unlock()
		return true
	}
	d.mu.Unlock()
	return false
}

// confirmLock 处理加锁成功后的确认流程：
// 1. 检查 Close 是否在 Redis 调用期间发生，是则补偿解锁
// 2. 记录 metrics
// 3. 启动看门狗
// token 由 Lua 脚本原子生成，0 表示 fencing 未启用或生成失败
func (d *distributedRedisLock) confirmLock(ctx context.Context, wrappedKey, originKey string, traceCtx context.Context, attrs []locker.Attr, start time.Time, token int64) error {
	// 二次检查：Lock 成功但 Close 可能在 Redis 调用期间发生
	if d.closed.Load() {
		unlockScript.Run(ctx, d.client, []string{wrappedKey}, d.owner)
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricAcquireFail, append(attrs, locker.AttrReason("closed"))...)
			d.metrics.RecordDuration(locker.MetricAcquireDur, time.Since(start), append(attrs, locker.AttrReason("closed"))...)
		}
		return locker.ErrLockerClosed
	}
	if d.metrics != nil {
		d.metrics.IncrementCounter(locker.MetricAcquire, attrs...)
		d.metrics.RecordDuration(locker.MetricAcquireDur, time.Since(start), attrs...)
	}

	entry := &lockEntry{traceCtx: traceCtx, localCnt: 1, token: token}

	// Fencing Token 生成失败降级记录（token 已由 Lua 脚本原子生成）
	if d.fencingEnabled && token == 0 {
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricFencingTokenFail, append(attrs, locker.AttrReason("incr_failed"))...)
		}
	}

	if !d.startWatchdog(wrappedKey, originKey, entry) {
		unlockScript.Run(ctx, d.client, []string{wrappedKey}, d.owner)
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricAcquireFail, append(attrs, locker.AttrReason("closed"))...)
		}
		return locker.ErrLockerClosed
	}
	return nil
}

// recordAcquireFail 记录加锁失败的 metrics 和 span 错误
func (d *distributedRedisLock) recordAcquireFail(attrs []locker.Attr, reason string, span locker.Span, err error) {
	if d.metrics != nil {
		d.metrics.IncrementCounter(locker.MetricAcquireFail, append(attrs, locker.AttrReason(reason))...)
	}
	if span != nil {
		span.RecordError(err)
	}
}

func (d *distributedRedisLock) Lock(ctx context.Context, key string) error {
	wrappedKey, err := d.validateKey(key)
	if err != nil {
		return err
	}

	if d.closed.Load() {
		return locker.ErrLockerClosed
	}

	// Fast path: 本地重入检查
	if d.tryReentrant(wrappedKey) {
		return nil
	}

	// Slow path: per-key 串行化，确保同一实例同一 key 只有一个 goroutine 进入 Redis 加锁流程
	keyEntry := d.keyMu.acquire(wrappedKey)
	keyEntry.mu.Lock()
	defer func() {
		keyEntry.mu.Unlock()
		d.keyMu.release(wrappedKey)
	}()

	// Double-check: 等待 keyMu 期间另一 goroutine 可能已注册 entry
	if d.tryReentrant(wrappedKey) {
		return nil
	}

	attrs := []locker.Attr{locker.AttrKey(key), locker.AttrOwner(d.owner)}
	var span locker.Span
	var traceCtx context.Context
	if d.tracer != nil {
		traceCtx, span = d.tracer.StartSpan(ctx, locker.SpanLock, attrs...)
	} else {
		traceCtx = context.Background()
	}
	if span != nil {
		defer span.End()
	}

	start := time.Now()

	// maxRetries 为初始尝试后的最大重试次数，总尝试次数 = maxRetries + 1
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if d.closed.Load() {
			if span != nil {
				span.RecordError(locker.ErrLockerClosed)
			}
			return locker.ErrLockerClosed
		}

		select {
		case <-ctx.Done():
			if span != nil {
				span.RecordError(ctx.Err())
			}
			return ctx.Err()
		default:
		}

		result, err := lockScript.Run(ctx, d.client, []string{wrappedKey, d.fenceKey(wrappedKey)}, d.owner, int(d.timeout.Milliseconds()), d.fencingInt()).Result()
			if err != nil {
			// Redis 错误：若有重试次数则重试，与看门狗续期容错策略一致
			if attempt < d.maxRetries {
				// Bounded jitter: [interval/2, interval*3/2]，避免高竞争下的惊群效应
				waitTime := d.retryInterval/2 + time.Duration(mrand.Int64N(int64(d.retryInterval)))
				select {
				case <-ctx.Done():
					if span != nil {
						span.RecordError(ctx.Err())
					}
					return ctx.Err()
				case <-time.After(waitTime):
					if d.closed.Load() {
						if span != nil {
							span.RecordError(locker.ErrLockerClosed)
						}
						return locker.ErrLockerClosed
					}
				}
				continue
			}
			d.recordAcquireFail(attrs, err.Error(), span, err)
			return xerror.Wrap(err, "get lock failed")
		}

		n, ok := result.(int64)
		if !ok {
			err := fmt.Errorf("unexpected lock result type: %T", result)
			if span != nil {
				span.RecordError(err)
			}
			return err
		}
		// n > 0: 成功（fencing 启用时 n 为 token 值，否则为 1）
		// n == -1: 成功但 fencing token 生成失败（优雅降级）
		// n == 0: 被占用
		if n != 0 {
			token := int64(0)
			if d.fencingEnabled && n > 0 {
				token = n
			}
			if err := d.confirmLock(ctx, wrappedKey, key, traceCtx, attrs, start, token); err != nil {
				if span != nil {
					span.RecordError(err)
				}
				return err
			}
			return nil
		}

		if attempt < d.maxRetries {
			// Bounded jitter: [interval/2, interval*3/2]，避免高竞争下的惊群效应
			waitTime := d.retryInterval/2 + time.Duration(mrand.Int64N(int64(d.retryInterval)))
			select {
			case <-ctx.Done():
				if span != nil {
					span.RecordError(ctx.Err())
				}
				return ctx.Err()
			case <-time.After(waitTime):
				if d.closed.Load() {
					if span != nil {
						span.RecordError(locker.ErrLockerClosed)
					}
					return locker.ErrLockerClosed
				}
			}
		}
	}

	d.recordAcquireFail(attrs, "occupied", span, locker.ErrLockOccupied)
	return locker.ErrLockOccupied
}

func (d *distributedRedisLock) TryLock(ctx context.Context, key string) error {
	wrappedKey, err := d.validateKey(key)
	if err != nil {
		return err
	}

	if d.closed.Load() {
		return locker.ErrLockerClosed
	}

	// Fast path: 本地重入检查
	if d.tryReentrant(wrappedKey) {
		return nil
	}

	// Slow path: 非阻塞尝试获取 per-key mutex
	keyEntry := d.keyMu.acquire(wrappedKey)
	if !keyEntry.mu.TryLock() {
		d.keyMu.release(wrappedKey)
		// 另一 goroutine 正在获取或释放此 key，检查是否可重入
		if d.tryReentrant(wrappedKey) {
			return nil
		}
		return locker.ErrLockOccupied
	}
	defer func() {
		keyEntry.mu.Unlock()
		d.keyMu.release(wrappedKey)
	}()

	// Double-check
	if d.tryReentrant(wrappedKey) {
		return nil
	}

	attrs := []locker.Attr{locker.AttrKey(key), locker.AttrOwner(d.owner)}
	var span locker.Span
	var traceCtx context.Context
	if d.tracer != nil {
		traceCtx, span = d.tracer.StartSpan(ctx, locker.SpanTryLock, attrs...)
	} else {
		traceCtx = context.Background()
	}
	if span != nil {
		defer span.End()
	}

	start := time.Now()

	result, err := lockScript.Run(ctx, d.client, []string{wrappedKey, d.fenceKey(wrappedKey)}, d.owner, int(d.timeout.Milliseconds()), d.fencingInt()).Result()
	if err != nil {
		d.recordAcquireFail(attrs, err.Error(), span, err)
		return xerror.Wrap(err, "get lock failed")
	}

	n, ok := result.(int64)
	if !ok {
		err := fmt.Errorf("unexpected lock result type: %T", result)
		if span != nil {
			span.RecordError(err)
		}
		return err
	}
	// n > 0: 成功（fencing 启用时 n 为 token 值，否则为 1）
	// n == -1: 成功但 fencing token 生成失败（优雅降级）
	// n == 0: 被占用
	if n != 0 {
		token := int64(0)
		if d.fencingEnabled && n > 0 {
			token = n
		}
		if err := d.confirmLock(ctx, wrappedKey, key, traceCtx, attrs, start, token); err != nil {
			if span != nil {
				span.RecordError(err)
			}
			return err
		}
		return nil
	}

	d.recordAcquireFail(attrs, "occupied", span, locker.ErrLockOccupied)
	return locker.ErrLockOccupied
}

func (d *distributedRedisLock) UnLock(ctx context.Context, key string) error {
	wrappedKey, err := d.validateKey(key)
	if err != nil {
		return err
	}

	if d.closed.Load() {
		return locker.ErrLockerClosed
	}

	// Acquire per-key mutex to serialize with concurrent Lock/TryLock on the same key.
	keyEntry := d.keyMu.acquire(wrappedKey)
	keyEntry.mu.Lock()
	defer func() {
		keyEntry.mu.Unlock()
		d.keyMu.release(wrappedKey)
	}()

	// 本地 entry 前置检查：若无本地记录，说明本地未持有锁，直接返回
	// 避免无意义的 Redis 调用，也避免极端情况下误解锁他人持有的锁
	d.mu.Lock()
	entry, exists := d.entries[wrappedKey]
	if !exists {
		d.mu.Unlock()
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, locker.AttrKey(key), locker.AttrOwner(d.owner), locker.AttrReason("not_found"))
		}
		return locker.ErrLockNotFound
	}
	if entry.localCnt > 1 {
		entry.localCnt--
		d.mu.Unlock()
		// 无看门狗时主动续期，防止 TTL 过期后本地仍认为持有锁
		if !d.watchDog {
			renewScript.Run(ctx, d.client, []string{wrappedKey}, d.owner, int(d.timeout.Milliseconds()))
		}
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, locker.AttrKey(key), locker.AttrOwner(d.owner))
		}
		return nil
	}
	entry.unlocking = true
	d.mu.Unlock()

	needRollback := true // Redis 调用失败时需要回滚 unlocking 标志

	defer func() {
		if needRollback {
			d.mu.Lock()
			entry.unlocking = false
			d.mu.Unlock()
		}
	}()

	attrs := []locker.Attr{locker.AttrKey(key), locker.AttrOwner(d.owner)}
	var span locker.Span
	if d.tracer != nil {
		_, span = d.tracer.StartSpan(ctx, locker.SpanUnLock, attrs...)
	}
	if span != nil {
		defer span.End()
	}

	result, err := unlockScript.Run(ctx, d.client, []string{wrappedKey}, d.owner).Result()
	if err != nil {
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, append(attrs, locker.AttrReason(err.Error()))...)
		}
		if span != nil {
			span.RecordError(err)
		}
		return xerror.Wrap(err, "unlock failed")
	}

	n, ok := result.(int64)
	if !ok {
		err := fmt.Errorf("unexpected unlock result type: %T", result)
		if span != nil {
			span.RecordError(err)
		}
		return err
	}

	switch n {
	case -1:
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, append(attrs, locker.AttrReason("not_found"))...)
		}
		if span != nil {
			span.RecordError(locker.ErrLockNotFound)
		}
		d.stopWatchdog(wrappedKey)
		needRollback = false
		return locker.ErrLockNotFound
	case -2:
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, append(attrs, locker.AttrReason("not_owner"))...)
		}
		if span != nil {
			span.RecordError(locker.ErrNotLockOwner)
		}
		d.stopWatchdog(wrappedKey)
		needRollback = false
		return locker.ErrNotLockOwner
	case 0:
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, attrs...)
		}
		d.stopWatchdog(wrappedKey)
		needRollback = false
	default:
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRelease, attrs...)
		}
		needRollback = false
	}

	return nil
}

// Token 返回当前锁的 Fencing Token。
// 必须在 Lock/TryLock 成功后调用。
// 返回 0 表示未持有锁或 Fencing Token 未启用。
func (d *distributedRedisLock) Token(key string) int64 {
	wrappedKey, err := d.validateKey(key)
	if err != nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry, exists := d.entries[wrappedKey]; exists {
		return entry.token
	}
	return 0
}

func (d *distributedRedisLock) Close() error {
	d.closed.Store(true)
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, entry := range d.entries {
		if entry.stopCh != nil {
			close(entry.stopCh)
			entry.stopCh = nil
		}
		delete(d.entries, key)
	}
	return nil
}

func (d *distributedRedisLock) Wait() error {
	d.wg.Wait()
	return nil
}
