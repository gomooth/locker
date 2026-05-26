package redislock

import (
	"time"

	"github.com/gomooth/locker"
)

// WithTimeout 设置锁超时时间
func WithTimeout(expire time.Duration) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		if expire <= 0 {
			expire = defaultTimeout
		}
		d.timeout = expire
	}
}

// WithRetry 设置获取锁重试次数和间隔
func WithRetry(maxRetries int, interval time.Duration) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		if maxRetries < 0 {
			maxRetries = 0
		}
		if interval <= 0 {
			interval = defaultRetryInterval
		}
		d.maxRetries = maxRetries
		d.retryInterval = interval
	}
}

// WithWatchDog 设置是否启用看门狗自动续期
func WithWatchDog(enable bool) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		d.watchDog = enable
	}
}

// WithMaxRenewFailures 设置看门狗续期最大连续失败次数，超过后停止续期并触发锁丢失回调
func WithMaxRenewFailures(n int) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		if n <= 0 {
			n = defaultMaxRenewFailures
		}
		d.maxRenewFailures = n
	}
}

// WithOnLockLost 设置锁丢失回调，当看门狗续期失败时调用
// 回调在独立 goroutine 中执行，需保证线程安全
func WithOnLockLost(fn LockLostFunc) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		d.onLockLost = fn
	}
}

// WithMetrics 设置指标采集接口
func WithMetrics(m locker.Metrics) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		d.metrics = m
	}
}

// WithTracer 设置链路追踪接口
func WithTracer(t locker.Tracer) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		d.tracer = t
	}
}

// WithKeyPrefix 设置锁键前缀，默认 "lock:"
func WithKeyPrefix(prefix string) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		d.keyPrefix = prefix
	}
}

// WithRenewTimeout 设置看门狗续期超时。
// 默认 0 表示使用 timeout/2。设置更长的超时可提高网络抖动容忍度。
func WithRenewTimeout(d time.Duration) func(*distributedRedisLock) {
	return func(l *distributedRedisLock) {
		if d < 0 {
			d = 0
		}
		l.renewTimeout = d
	}
}

// WithRecoveryProbe 设置达到续期最大失败次数后是否执行恢复探测。
// 启用后（默认），在看门狗判定锁丢失前会用更长的超时做最后一次续期尝试，
// 避免因短暂网络抖动误报锁丢失。
func WithRecoveryProbe(enable bool) func(*distributedRedisLock) {
	return func(l *distributedRedisLock) {
		l.recoveryProbe = enable
	}
}

// WithFencingToken 启用 Fencing Token 支持。
// 启用后每次加锁会通过 Redis INCR 生成单调递增的令牌，
// 可通过 Token(key) 获取，用于防止分布式锁的"延迟解锁"问题。
// 注意：启用后每次加锁增加一次 Redis INCR 调用。
func WithFencingToken(enable bool) func(*distributedRedisLock) {
	return func(l *distributedRedisLock) {
		l.fencingEnabled = enable
	}
}
