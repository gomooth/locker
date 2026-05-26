package redislock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gomooth/locker"
)

func (d *distributedRedisLock) startWatchdog(wrappedKey, originKey string, entry *lockEntry) bool {
	if !d.watchDog {
		// 看门狗未启用，仍需将 entry 加入 map（用于重入检查）
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed.Load() {
			return false
		}
		d.entries[wrappedKey] = entry
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed.Load() {
		return false
	}

	if _, exists := d.entries[wrappedKey]; exists {
		// per-key mutex 保护下此分支理论上不可达；保留为防御性检查
		return true
	}

	stop := make(chan struct{})
	entry.stopCh = stop
	d.entries[wrappedKey] = entry

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(d.timeout / 3)
		defer ticker.Stop()

		var callbackWg sync.WaitGroup
		defer callbackWg.Wait()

		attrs := []locker.Attr{locker.AttrKey(originKey), locker.AttrOwner(d.owner)}

		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var shouldExit bool
				shouldExit, failures = d.renewTick(wrappedKey, originKey, entry, attrs, failures, &callbackWg)
				if shouldExit {
					return
				}
			}
		}
	}()
	return true
}

// renewTick 处理一次看门狗续期。
// 返回 (shouldExit, newFailures)：shouldExit=true 表示看门狗应退出。
func (d *distributedRedisLock) renewTick(
	wrappedKey, originKey string,
	entry *lockEntry,
	attrs []locker.Attr,
	failures int,
	callbackWg *sync.WaitGroup,
) (bool, int) {
	var span locker.Span
	if d.tracer != nil {
		_, span = d.tracer.StartSpan(entry.traceCtx, locker.SpanRenew, attrs...)
	}
	if span != nil {
		defer span.End()
	}

	start := time.Now()
	renewCtx, renewCancel := context.WithTimeout(context.Background(), d.getRenewTimeout())
	result, err := renewScript.Run(renewCtx, d.client, []string{wrappedKey}, d.owner, int(d.timeout.Milliseconds())).Result()
	renewCancel()

	if err != nil {
		failures++
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRenewFail, append(attrs, locker.AttrReason(err.Error()))...)
		}
		if span != nil {
			span.RecordError(err)
		}
		if failures >= d.maxRenewFailures {
			if d.recoveryProbe && d.tryRecoveryProbe(wrappedKey, originKey) {
				return false, 0
			}
			d.handleLockLost(wrappedKey, originKey, callbackWg)
			return true, failures
		}
		return false, failures
	}

	n, ok := result.(int64)
	if !ok || n == 0 {
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRenewFail, append(attrs, locker.AttrReason("key_not_found"))...)
		}
		if span != nil {
			span.RecordError(fmt.Errorf("renew returned 0"))
		}
		d.handleLockLost(wrappedKey, originKey, callbackWg)
		return true, failures
	}

	if d.metrics != nil {
		d.metrics.IncrementCounter(locker.MetricRenew, attrs...)
		d.metrics.RecordDuration(locker.MetricRenewDur, time.Since(start), attrs...)
	}
	return false, 0
}

// tryRecoveryProbe 在达到续期最大失败次数后，用更长超时做最后一次续期尝试。
// 成功说明 Redis 已恢复且锁仍在，避免因短暂网络抖动误报锁丢失。
func (d *distributedRedisLock) tryRecoveryProbe(wrappedKey, originKey string) bool {
	probeCtx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	result, err := renewScript.Run(probeCtx, d.client, []string{wrappedKey}, d.owner, int(d.timeout.Milliseconds())).Result()
	if err != nil {
		if d.metrics != nil {
			d.metrics.IncrementCounter(locker.MetricRenewFail,
				locker.AttrKey(originKey), locker.AttrOwner(d.owner), locker.AttrReason("recovery_probe_failed"))
		}
		return false
	}
	n, ok := result.(int64)
	if !ok || n == 0 {
		return false
	}
	if d.metrics != nil {
		d.metrics.IncrementCounter(locker.MetricRenew,
			locker.AttrKey(originKey), locker.AttrOwner(d.owner), locker.AttrReason("recovery_probe"))
	}
	return true
}

// handleLockLost 处理看门狗续期失败：清理 entries map 并触发锁丢失回调
func (d *distributedRedisLock) handleLockLost(wrappedKey, originKey string, callbackWg *sync.WaitGroup) {
	d.mu.Lock()
	entry, exists := d.entries[wrappedKey]
	if exists {
		delete(d.entries, wrappedKey)
	}
	suppressCallback := !exists || entry.unlocking || d.closed.Load()
	d.mu.Unlock()

	if suppressCallback {
		return
	}

	if d.metrics != nil {
		d.metrics.IncrementCounter(locker.MetricLost, locker.AttrKey(originKey), locker.AttrOwner(d.owner))
	}
	if d.tracer != nil {
		_, span := d.tracer.StartSpan(entry.traceCtx, locker.SpanLost, locker.AttrKey(originKey), locker.AttrOwner(d.owner))
		span.RecordError(fmt.Errorf("lock lost"))
		span.End()
	}
	if d.onLockLost != nil {
		callbackWg.Add(1)
		go func() {
			defer callbackWg.Done()
			defer func() {
				if r := recover(); r != nil {
					// 用户回调 panic 不应拖垮进程
				}
			}()
			d.onLockLost(originKey)
		}()
	}
}

func (d *distributedRedisLock) stopWatchdog(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry, exists := d.entries[key]; exists {
		if entry.stopCh != nil {
			close(entry.stopCh)
			entry.stopCh = nil
		}
		delete(d.entries, key)
	}
}
