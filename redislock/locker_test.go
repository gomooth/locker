package redislock_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gomooth/locker"
	"github.com/gomooth/locker/lockhelper"
	"github.com/gomooth/locker/redislock"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	return client, mr
}

// waitUntil 轮询等待条件满足，替代 time.Sleep 固定等待以减少 flaky test
func waitUntil(t *testing.T, timeout time.Duration, interval time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatal(msg)
}

func TestLockAndUnlock(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "basic")

	err := lk.Lock(ctx, key)
	assert.Nil(t, err)

	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)
}

func TestTryLock(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "trylock")

	err := lk.TryLock(ctx, key)
	assert.Nil(t, err)

	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)
}

func TestConcurrentContention(t *testing.T) {
	client, _ := newTestClient(t)

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent")

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lk := redislock.New(client, redislock.WithWatchDog(false))
			err := lk.TryLock(ctx, key)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
				time.Sleep(50 * time.Millisecond)
				_ = lk.UnLock(ctx, key)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, successCount, "only one goroutine should acquire the lock")
}

func TestReentrantLock(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "reentrant")

	// 第一次加锁
	err := lk.Lock(ctx, key)
	assert.Nil(t, err)

	// 同一实例再次加锁（可重入）
	err = lk.Lock(ctx, key)
	assert.Nil(t, err)

	// 第一次解锁（计数递减，未完全释放）
	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)

	// 锁仍被持有，其他实例无法获取
	lk2 := redislock.New(client, redislock.WithWatchDog(false))
	err = lk2.TryLock(ctx, key)
	assert.Equal(t, locker.ErrLockOccupied, err)

	// 第二次解锁（完全释放）
	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)

	// 释放后其他实例可以获取
	err = lk2.TryLock(ctx, key)
	assert.Nil(t, err)
	_ = lk2.UnLock(ctx, key)
}

func TestNotOwnerCannotUnlock(t *testing.T) {
	client, _ := newTestClient(t)
	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	lk2 := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "owner")

	err := lk1.Lock(ctx, key)
	assert.Nil(t, err)

	// 非持有者不能解锁（本地无 entry，前置检查返回 ErrLockNotFound）
	err = lk2.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)

	// 持有者可以解锁
	err = lk1.UnLock(ctx, key)
	assert.Nil(t, err)
}

func TestLockAutoExpire(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithTimeout(1*time.Second), redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "expire")

	err := lk.Lock(ctx, key)
	assert.Nil(t, err)

	// 模拟锁过期
	mr.FastForward(2 * time.Second)

	// 锁已过期，其他实例可以获取
	lk2 := redislock.New(client, redislock.WithWatchDog(false))
	err = lk2.TryLock(ctx, key)
	assert.Nil(t, err)
	_ = lk2.UnLock(ctx, key)
}

func TestWatchDogRenewal(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithTimeout(3*time.Second), redislock.WithWatchDog(true))

	ctx := context.Background()
	key := lockhelper.Key("test", "watchdog")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 等待看门狗至少续期一次（timeout/3 = 1s 后触发）
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		ttl := client.PTTL(ctx, "lock:"+key).Val()
		return ttl.Milliseconds() > 2000
	}, "TTL should be renewed by watchdog")

	// 持有者可以解锁（看门狗应停止）
	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)
}

func TestLockRetry(t *testing.T) {
	client, mr := newTestClient(t)

	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	lk2 := redislock.New(client, redislock.WithRetry(5, 100*time.Millisecond), redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "retry")

	// lk1 持有锁
	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	// lk2 在另一个 goroutine 中重试获取锁
	gotLock := make(chan error, 1)
	go func() {
		gotLock <- lk2.Lock(ctx, key)
	}()

	// 短暂等待后释放 lk1
	time.Sleep(300 * time.Millisecond)
	_ = lk1.UnLock(ctx, key)

	// lk2 应该能通过重试获取锁
	err = <-gotLock
	assert.Nil(t, err)

	_ = lk2.UnLock(ctx, key)
	mr.Close()
}

func TestContextCancel(t *testing.T) {
	client, _ := newTestClient(t)

	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	lk2 := redislock.New(client, redislock.WithRetry(100, 50*time.Millisecond), redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "cancel")

	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	// 用可取消的 context
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	gotErr := make(chan error, 1)
	go func() {
		gotErr <- lk2.Lock(ctx2, key)
	}()

	// 短暂等待后取消 context
	time.Sleep(200 * time.Millisecond)
	cancel()

	err = <-gotErr
	assert.Equal(t, context.Canceled, err)

	_ = lk1.UnLock(ctx, key)
}

func TestEmptyKey(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()

	err := lk.Lock(ctx, "")
	assert.Equal(t, locker.ErrEmptyKey, err)

	err = lk.TryLock(ctx, "")
	assert.Equal(t, locker.ErrEmptyKey, err)

	err = lk.UnLock(ctx, "")
	assert.Equal(t, locker.ErrEmptyKey, err)
}

func TestUnlockNotFound(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "notfound")

	err := lk.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)
}

func TestLockKeyPrefix(t *testing.T) {
	client, mr := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("app", "biz")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 验证 Redis 中的 key 有 lock: 前缀
	exists := mr.Exists("lock:app:biz")
	assert.True(t, exists, "key should have 'lock:' prefix in Redis")

	_ = lk.UnLock(ctx, key)
}

func TestWatchDogStopsOnLockLost(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "watchdog-lost")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁丢失：直接删除 Redis 中的 key
	mr.Del("lock:" + key)

	// 等待看门狗检测到锁丢失（timeout/3 = 1s 后触发续期）
	select {
	case lostKey := <-lost:
		assert.Equal(t, key, lostKey)
	case <-time.After(3 * time.Second):
		t.Fatal("OnLockLost should have been called after lock loss")
	}
}

func TestWatchDogStopsOnRedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithMaxRenewFailures(2),
		redislock.WithRecoveryProbe(false), // 禁用恢复探测以加速测试
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "redis-error")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 关闭 miniredis 模拟 Redis 连接故障
	mr.Close()

	// 等待看门狗检测到续期失败
	select {
	case lostKey := <-lost:
		assert.Equal(t, key, lostKey)
	case <-time.After(5 * time.Second):
		t.Fatal("OnLockLost should have been called after Redis errors")
	}
}

func TestNoFalseLockLostOnUnlock(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "no-false-lost")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 正常解锁，不应触发 onLockLost
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// 等待超过看门狗续期间隔，确认没有误触发
	select {
	case k := <-lost:
		t.Fatalf("onLockLost should not be called on explicit unlock, got key: %s", k)
	case <-time.After(2 * time.Second):
		// 预期：超时，无回调
	}
}

func TestClose(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(30*time.Second),
		redislock.WithWatchDog(true),
	)

	ctx := context.Background()
	key1 := lockhelper.Key("test", "close-1")
	key2 := lockhelper.Key("test", "close-2")

	err := lk.Lock(ctx, key1)
	require.Nil(t, err)
	err = lk.Lock(ctx, key2)
	require.Nil(t, err)

	// Close 应停止所有看门狗 goroutine
	err = lk.Close()
	assert.Nil(t, err)

	// Close 可以安全重复调用（幂等）
	err = lk.Close()
	assert.Nil(t, err)

	// 锁仍存在于 Redis 中（Close 不自动释放锁）
	exists := mr.Exists("lock:" + key1)
	assert.True(t, exists)

	// Close 后调用 Lock/UnLock 应返回 ErrLockerClosed
	err = lk.Lock(ctx, key1)
	assert.Equal(t, locker.ErrLockerClosed, err)

	err = lk.TryLock(ctx, key1)
	assert.Equal(t, locker.ErrLockerClosed, err)

	err = lk.UnLock(ctx, key1)
	assert.Equal(t, locker.ErrLockerClosed, err)
}

func TestPartialUnlockWatchdogContinues(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "partial-unlock-watchdog")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 重入加锁
	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	// 部分解锁（count 2→1），看门狗应继续运行
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// 等待看门狗续期（timeout/3 = 1s 后触发）
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		ttl := client.PTTL(ctx, "lock:"+key).Val()
		return ttl.Milliseconds() > 2000
	}, "TTL should be renewed by watchdog after partial unlock")

	// 完全释放
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)
}

func TestLockRetryOnRedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithRetry(3, 100*time.Millisecond),
		redislock.WithWatchDog(false),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "retry-redis-error")

	// 先加锁
	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 在另一个 goroutine 中尝试获取锁，期间模拟 Redis 故障
	gotResult := make(chan error, 1)
	go func() {
		lk2 := redislock.New(client,
			redislock.WithRetry(3, 100*time.Millisecond),
			redislock.WithWatchDog(false),
		)
		gotResult <- lk2.Lock(ctx, key)
	}()

	// 短暂等待后释放锁
	time.Sleep(200 * time.Millisecond)
	_ = lk.UnLock(ctx, key)

	// 应该能通过重试获取锁
	err = <-gotResult
	assert.Nil(t, err)
}

func TestClosePreventsFalseOnLockLost(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "close-no-false-lost")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 直接删除 Redis key 模拟锁丢失
	mr.Del("lock:" + key)

	// 立即 Close，看门狗可能还没检测到
	err = lk.Close()
	require.Nil(t, err)

	// 等待超过看门狗续期间隔，确认 Close 阻止了误触回调
	select {
	case k := <-lost:
		t.Fatalf("onLockLost should not be called after Close, got key: %s", k)
	case <-time.After(2 * time.Second):
		// 预期：Close 阻止了回调
	}
}

func TestLockHelperKeyEmptyParts(t *testing.T) {
	// 空 key 应返回空
	assert.Equal(t, "", lockhelper.Key(""))

	// 空片段应被过滤
	assert.Equal(t, "app:biz", lockhelper.Key("app", "", "biz"))
	assert.Equal(t, "app", lockhelper.Key("app", "", ""))
	assert.Equal(t, "a:b:c", lockhelper.Key("a", "b", "c"))

	// 全空片段
	assert.Equal(t, "app", lockhelper.Key("app", "", "", ""))
}

type mockMetrics struct {
	mu        sync.Mutex
	counters  map[string][]AttrsSnapshot
	durations map[string][]durationSnapshot
}

type AttrsSnapshot struct {
	Attrs []locker.Attr
}

type durationSnapshot struct {
	Duration time.Duration
	Attrs    []locker.Attr
}

func newMockMetrics() *mockMetrics {
	return &mockMetrics{
		counters:  make(map[string][]AttrsSnapshot),
		durations: make(map[string][]durationSnapshot),
	}
}

func (m *mockMetrics) IncrementCounter(name string, attrs ...locker.Attr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name] = append(m.counters[name], AttrsSnapshot{Attrs: append([]locker.Attr{}, attrs...)})
}

func (m *mockMetrics) RecordDuration(name string, d time.Duration, attrs ...locker.Attr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations[name] = append(m.durations[name], durationSnapshot{Duration: d, Attrs: append([]locker.Attr{}, attrs...)})
}

func (m *mockMetrics) getCounter(name string) []AttrsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[name]
}

func (m *mockMetrics) getDurations(name string) []durationSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.durations[name]
}

// getCounterReasons 获取指定 counter 的所有 reason 属性值
func (m *mockMetrics) getCounterReasons(name string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var reasons []string
	for _, snap := range m.counters[name] {
		for _, attr := range snap.Attrs {
			if attr.Key == locker.AttrReasonStr {
				reasons = append(reasons, attr.Value)
			}
		}
	}
	return reasons
}

type mockSpan struct {
	mu        sync.Mutex
	ended     bool
	attrs     []locker.Attr
	errs      []error
	name      string
	parentCtx context.Context
}

func (s *mockSpan) End() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}
func (s *mockSpan) SetAttributes(attrs ...locker.Attr) {
	s.mu.Lock()
	s.attrs = append(s.attrs, attrs...)
	s.mu.Unlock()
}
func (s *mockSpan) RecordError(err error) {
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}

func (s *mockSpan) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

func (s *mockSpan) getName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

func (s *mockSpan) getParentCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parentCtx
}

type mockTracer struct {
	mu    sync.Mutex
	spans []*mockSpan
}

func newMockTracer() *mockTracer {
	return &mockTracer{}
}

func (t *mockTracer) StartSpan(ctx context.Context, name string, attrs ...locker.Attr) (context.Context, locker.Span) {
	s := &mockSpan{name: name, attrs: append([]locker.Attr{}, attrs...), parentCtx: ctx}
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return ctx, s
}

func (t *mockTracer) getSpans() []*mockSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spans
}

func TestLockMetricsAndTrace(t *testing.T) {
	client, _ := newTestClient(t)
	mm := newMockMetrics()
	mt := newMockTracer()

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithMetrics(mm),
		redislock.WithTracer(mt),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "metrics-lock")

	// Lock 成功
	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	assert.NotEmpty(t, mm.getCounter(locker.MetricAcquire), "should record lock.acquire counter")
	assert.NotEmpty(t, mm.getDurations(locker.MetricAcquireDur), "should record lock.acquire.duration")
	spans := mt.getSpans()
	assert.Equal(t, 1, len(spans), "should create one span for Lock")
	assert.True(t, spans[0].isEnded(), "span should be ended")

	// UnLock
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)
}

func TestLockFailMetrics(t *testing.T) {
	client, _ := newTestClient(t)
	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	mm := newMockMetrics()
	lk2 := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "metrics-fail")

	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	// lk2 TryLock 应失败
	err = lk2.TryLock(ctx, key)
	assert.Equal(t, locker.ErrLockOccupied, err)

	assert.NotEmpty(t, mm.getCounter(locker.MetricAcquireFail), "should record lock.acquire.fail counter")

	_ = lk1.UnLock(ctx, key)
}

func TestWatchDogRenewMetrics(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	mm := newMockMetrics()

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "renew-metrics")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 等待看门狗至少续期一次（timeout/3 = 1s）
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		return len(mm.getCounter(locker.MetricRenew)) > 0
	}, "should record lock.renew counter")

	err = lk.UnLock(ctx, key)
	require.Nil(t, err)
}

func TestLockLostMetrics(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	mm := newMockMetrics()

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "lost-metrics")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁丢失
	mr.Del("lock:" + key)

	// 等待看门狗检测到
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		return len(mm.getCounter(locker.MetricLost)) > 0
	}, "should record lock.lost counter")
}

func TestWaitAfterClose(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "wait-after-close")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	err = lk.Close()
	require.Nil(t, err)

	done := make(chan struct{})
	go func() {
		_ = lk.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Expected: Wait returns after Close
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() should return after Close()")
	}
}

func TestWaitNoGoroutines(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	// Wait without any lock should return immediately
	done := make(chan struct{})
	go func() {
		_ = lk.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait() should return immediately when no goroutines")
	}
}

func TestLockAfterCloseReturnsErrLockerClosed(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(true))

	ctx := context.Background()
	key := lockhelper.Key("test", "lock-after-close")

	err := lk.Close()
	require.Nil(t, err)

	err = lk.Lock(ctx, key)
	assert.Equal(t, locker.ErrLockerClosed, err)

	err = lk.TryLock(ctx, key)
	assert.Equal(t, locker.ErrLockerClosed, err)
}

func TestConcurrentLockAndClose(t *testing.T) {
	client, _ := newTestClient(t)

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent-lock-close")

	var wg sync.WaitGroup

	lk := redislock.New(client, redislock.WithWatchDog(true))
	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 并发 Close + 多个 TryLock
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk2 := redislock.New(client, redislock.WithWatchDog(false))
			_ = lk2.TryLock(ctx, key)
			_ = lk2.UnLock(ctx, key)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		_ = lk.Close()
	}()

	wg.Wait()
	// 不应有 panic，所有 goroutine 应正常退出
}

func TestCloseIsIdempotent(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	err := lk.Close()
	assert.Nil(t, err)
	err = lk.Close()
	assert.Nil(t, err)
	err = lk.Close()
	assert.Nil(t, err)
}

func TestOnLockLostAsyncExecution(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "async-callback")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁丢失
	mr.Del("lock:" + key)

	// 回调应在独立 goroutine 中执行
	select {
	case lostKey := <-lost:
		assert.Equal(t, key, lostKey)
	case <-time.After(3 * time.Second):
		t.Fatal("onLockLost should have been called asynchronously")
	}

	_ = lk.Close()
	_ = lk.Wait()
}

func TestOnLockLostPanicRecovery(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			panic("callback panic")
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "panic-callback")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁丢失
	mr.Del("lock:" + key)

	// 回调 panic 不应拖垮进程，Wait 应正常返回
	done := make(chan struct{})
	go func() {
		_ = lk.Close()
		_ = lk.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 预期：Wait 正常返回
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() should return even if onLockLost panics")
	}
}

func TestUnlockNotFoundMetrics(t *testing.T) {
	client, _ := newTestClient(t)
	mm := newMockMetrics()

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "metrics-not-found")

	err := lk.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)

	counters := mm.getCounter(locker.MetricRelease)
	assert.NotEmpty(t, counters, "should record lock.release counter for not_found")
}

func TestUnlockNotOwnerMetrics(t *testing.T) {
	client, _ := newTestClient(t)
	mm := newMockMetrics()

	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	lk2 := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "metrics-not-owner")

	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	// lk2 非持有者解锁 → 本地无 entry，前置检查返回 ErrLockNotFound
	err = lk2.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)

	counters := mm.getCounter(locker.MetricRelease)
	assert.NotEmpty(t, counters, "should record lock.release counter for not_found locally")

	_ = lk1.UnLock(ctx, key)
}

func TestReentrantLocalCount(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "local-reentrant")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// Re-entry should not increment Redis count
	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	// Verify Redis count remains 1 (local count optimization)
	count, err := client.HGet(ctx, "lock:"+key, "count").Int()
	require.Nil(t, err)
	assert.Equal(t, 1, count, "Redis count should remain 1 with local reentrant optimization")

	_ = lk.UnLock(ctx, key)
	_ = lk.UnLock(ctx, key)
}

func TestReentrantPartialUnlockLocal(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "partial-unlock-local")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)
	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	// Partial UnLock (localCnt 2→1), should NOT go to Redis DEL
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// Redis key should still exist
	exists := mr.Exists("lock:" + key)
	assert.True(t, exists, "Redis key should still exist after partial unlock")

	// Another instance still cannot acquire the lock
	lk2 := redislock.New(client, redislock.WithWatchDog(false))
	err = lk2.TryLock(ctx, key)
	assert.Equal(t, locker.ErrLockOccupied, err)

	_ = lk.UnLock(ctx, key)
}

func TestReentrantFinalUnlockRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "final-unlock-redis")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)
	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	// Partial UnLock
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// Final UnLock should go to Redis DEL
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// Redis key should be deleted
	exists := mr.Exists("lock:" + key)
	assert.False(t, exists, "Redis key should be deleted after final unlock")

	// Another instance can acquire the lock
	lk2 := redislock.New(client, redislock.WithWatchDog(false))
	err = lk2.TryLock(ctx, key)
	assert.Nil(t, err)
	_ = lk2.UnLock(ctx, key)
}

func TestUnLockSpan(t *testing.T) {
	client, _ := newTestClient(t)
	mt := newMockTracer()

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithTracer(mt),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "unlock-span")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	spans := mt.getSpans()
	assert.Equal(t, 2, len(spans), "should have Lock and UnLock spans")

	unlockSpan := spans[1]
	assert.Equal(t, locker.SpanUnLock, unlockSpan.getName())
	assert.True(t, unlockSpan.isEnded(), "UnLock span should be ended")
}

func TestRenewSpan(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mt := newMockTracer()

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithTracer(mt),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "renew-span")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// Wait for at least one watchdog renewal
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		for _, s := range mt.getSpans() {
			if s.getName() == locker.SpanRenew {
				return true
			}
		}
		return false
	}, "should have at least one Renew span")

	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	spans := mt.getSpans()
	renewSpans := 0
	for _, s := range spans {
		if s.getName() == locker.SpanRenew {
			renewSpans++
			assert.True(t, s.isEnded(), "Renew span should be ended")
		}
	}
	assert.GreaterOrEqual(t, renewSpans, 1, "should have at least one Renew span")
}

func TestLockLostSpan(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mt := newMockTracer()

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithTracer(mt),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "lost-span")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// Simulate lock loss
	mr.Del("lock:" + key)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("OnLockLost should have been called")
	}

	spans := mt.getSpans()
	lostSpans := 0
	for _, s := range spans {
		if s.getName() == locker.SpanLost {
			lostSpans++
			assert.True(t, s.isEnded(), "Lost span should be ended")
		}
	}
	assert.Equal(t, 1, lostSpans, "should have one Lost span")

	_ = lk.Close()
	_ = lk.Wait()
}

func TestLockSpanParentChild(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mt := newMockTracer()

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithTracer(mt),
		redislock.WithOnLockLost(func(key string) {}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "span-parent")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 等待看门狗续期
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		for _, s := range mt.getSpans() {
			if s.getName() == locker.SpanRenew {
				return true
			}
		}
		return false
	}, "should have at least one Renew span")

	// 模拟锁丢失
	mr.Del("lock:" + key)
	time.Sleep(1500 * time.Millisecond)

	_ = lk.Close()
	_ = lk.Wait()

	spans := mt.getSpans()

	// 找到 Lock span 及其返回的 context
	var lockSpanCtx context.Context
	for _, s := range spans {
		if s.getName() == locker.SpanLock {
			lockSpanCtx = s.getParentCtx()
			break
		}
	}
	require.NotNil(t, lockSpanCtx, "should have a Lock span with a parentCtx")

	// Renew 和 Lost span 应该以 Lock span 的 context 作为 parent
	for _, s := range spans {
		name := s.getName()
		if name == locker.SpanRenew || name == locker.SpanLost {
			assert.Equal(t, lockSpanCtx, s.getParentCtx(),
				"%s span should have Lock span's context as parent", name)
		}
	}
}

// --- Per-key Mutex 并发 Lock 测试 ---

func TestConcurrentLockSameKey(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent-reentrant")

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- lk.Lock(ctx, key)
		}()
	}
	wg.Wait()
	close(errCh)

	successCount := 0
	for err := range errCh {
		if err == nil {
			successCount++
		}
	}
	assert.Equal(t, 2, successCount, "both goroutines should succeed (reentrant)")

	// 两次 UnLock 应正确释放
	err := lk.UnLock(ctx, key)
	assert.Nil(t, err)
	err = lk.UnLock(ctx, key)
	assert.Nil(t, err)

	// 释放后其他实例可以获取
	lk2 := redislock.New(client, redislock.WithWatchDog(false))
	err = lk2.TryLock(ctx, key)
	assert.Nil(t, err)
	_ = lk2.UnLock(ctx, key)
}

func TestConcurrentTryLockSameKey(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent-trylock")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 并发 TryLock 应重入
	err = lk.TryLock(ctx, key)
	assert.Nil(t, err)

	_ = lk.UnLock(ctx, key)
	_ = lk.UnLock(ctx, key)
}

func TestConcurrentLockUnlockSameKey(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent-lock-unlock")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 另一个 goroutine Lock 同一实例同一 key 应重入
	done := make(chan error, 1)
	go func() {
		done <- lk.Lock(ctx, key)
	}()
	err = <-done
	assert.Nil(t, err, "concurrent Lock on same instance should re-enter")

	_ = lk.UnLock(ctx, key)
	_ = lk.UnLock(ctx, key)
}

func TestLockRetryExitsOnClose(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk1 := redislock.New(client, redislock.WithWatchDog(false))
	ctx := context.Background()
	key := lockhelper.Key("test", "retry-close")
	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	lk2 := redislock.New(client,
		redislock.WithRetry(50, 100*time.Millisecond),
		redislock.WithWatchDog(false),
	)

	gotErr := make(chan error, 1)
	go func() {
		gotErr <- lk2.Lock(ctx, key)
	}()

	time.Sleep(300 * time.Millisecond)
	_ = lk2.Close()

	select {
	case err := <-gotErr:
		assert.Equal(t, locker.ErrLockerClosed, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Lock should exit after Close")
	}

	_ = lk1.UnLock(ctx, key)
}

func TestConcurrentLockAndCloseP1(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(true))

	ctx := context.Background()
	key := lockhelper.Key("test", "concurrent-lock-close-p1")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		_ = lk.Close()
	}()

	_ = lk.Wait()
	wg.Wait()

	err = lk.Lock(ctx, key)
	assert.Equal(t, locker.ErrLockerClosed, err)
}

// --- 部分解锁续期 TTL 测试 ---

func TestPartialUnlockRenewsWithoutWatchdog(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(false),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "partial-renew")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	// 等待接近 TTL
	mr.FastForward(2500 * time.Millisecond)

	// 部分解锁应续期 TTL
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	ttl := client.PTTL(ctx, "lock:"+key).Val()
	assert.Greater(t, ttl.Milliseconds(), int64(2000), "TTL should be renewed after partial unlock without watchdog")

	_ = lk.UnLock(ctx, key)
}

func TestPartialUnlockWithWatchdog(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "partial-watchdog")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	err = lk.Lock(ctx, key)
	require.Nil(t, err)

	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// 看门狗应正常续期
	waitUntil(t, 3*time.Second, 100*time.Millisecond, func() bool {
		ttl := client.PTTL(ctx, "lock:"+key).Val()
		return ttl.Milliseconds() > 2000
	}, "watchdog should renew TTL")

	_ = lk.UnLock(ctx, key)
}

// --- Key 前缀测试 ---

func TestCustomKeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithKeyPrefix("myapp:"),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "prefix")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	exists := mr.Exists("myapp:test:prefix")
	assert.True(t, exists, "key should use custom prefix")

	_ = lk.UnLock(ctx, key)
}

func TestEmptyKeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithKeyPrefix(""),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "noprefix")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	exists := mr.Exists("test:noprefix")
	assert.True(t, exists, "key should have no prefix")

	_ = lk.UnLock(ctx, key)
}

func TestDefaultKeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "default-prefix")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	exists := mr.Exists("lock:test:default-prefix")
	assert.True(t, exists, "key should use default 'lock:' prefix")

	_ = lk.UnLock(ctx, key)
}

// --- Lock fast path 与 UnLock 竞态修复验证 ---

func TestConcurrentLockUnlockRace(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "lock-unlock-race")

	const iterations = 100
	for i := 0; i < iterations; i++ {
		err := lk.Lock(ctx, key)
		require.Nil(t, err)

		var wg sync.WaitGroup
		var lockErr, unlockErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			lockErr = lk.Lock(ctx, key)
		}()
		go func() {
			defer wg.Done()
			unlockErr = lk.UnLock(ctx, key)
		}()
		wg.Wait()

		require.Nil(t, lockErr, "iteration %d: Lock should succeed", i)
		require.Nil(t, unlockErr, "iteration %d: UnLock should succeed", i)

		// 最终释放：无论哪种交错顺序，一次 UnLock 应完全释放锁
		err = lk.UnLock(ctx, key)
		assert.Nil(t, err, "iteration %d: final UnLock should succeed", i)

		// 释放后其他实例应能获取锁
		lk2 := redislock.New(client, redislock.WithWatchDog(false))
		err = lk2.TryLock(ctx, key)
		assert.Nil(t, err, "iteration %d: other instance should acquire lock after full release", i)
		_ = lk2.UnLock(ctx, key)
	}
}

func TestConcurrentTryLockUnlockRace(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "trylock-unlock-race")

	const iterations = 100
	for i := 0; i < iterations; i++ {
		err := lk.Lock(ctx, key)
		require.Nil(t, err)

		var wg sync.WaitGroup
		var tryLockErr, unlockErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			tryLockErr = lk.TryLock(ctx, key)
		}()
		go func() {
			defer wg.Done()
			unlockErr = lk.UnLock(ctx, key)
		}()
		wg.Wait()

		// UnLock 必须成功
		require.Nil(t, unlockErr, "iteration %d: UnLock should succeed", i)

		if tryLockErr == nil {
			// TryLock re-entry 成功，需要额外 UnLock
			err = lk.UnLock(ctx, key)
			assert.Nil(t, err, "iteration %d: UnLock after TryLock re-entry should succeed", i)
		}

		// 确保锁已完全释放
		lk2 := redislock.New(client, redislock.WithWatchDog(false))
		err = lk2.TryLock(ctx, key)
		assert.Nil(t, err, "iteration %d: other instance should acquire lock after cleanup", i)
		_ = lk2.UnLock(ctx, key)
	}
}

// --- Fencing Token 测试 ---

func TestFencingToken(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithFencingToken(true),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "fencing")

	ft, ok := lk.(locker.FencingTokener)
	require.True(t, ok, "should implement FencingTokener")

	// 未加锁时 Token 应返回 0
	assert.Equal(t, int64(0), ft.Token(key))

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 加锁后 Token 应为 1（首次 INCR）
	token := ft.Token(key)
	assert.Equal(t, int64(1), token, "first lock should have token 1")

	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// 解锁后 Token 应返回 0
	assert.Equal(t, int64(0), ft.Token(key))

	// 再次加锁，Token 应为 2
	lk2 := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithFencingToken(true),
	)
	ft2, ok := lk2.(locker.FencingTokener)
	require.True(t, ok, "lk2 should implement FencingTokener")
	err = lk2.Lock(ctx, key)
	require.Nil(t, err)
	token2 := ft2.Token(key)
	assert.Equal(t, int64(2), token2, "second lock should have token 2")

	_ = lk2.UnLock(ctx, key)
}

func TestFencingTokenReentrant(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithFencingToken(true),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "fencing-reentrant")

	ft, ok := lk.(locker.FencingTokener)
	require.True(t, ok, "should implement FencingTokener")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	token1 := ft.Token(key)

	// 可重入应返回相同 Token
	err = lk.Lock(ctx, key)
	require.Nil(t, err)
	assert.Equal(t, token1, ft.Token(key), "re-entrant lock should return same token")

	_ = lk.UnLock(ctx, key)
	_ = lk.UnLock(ctx, key)
}

func TestFencingTokenDisabled(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "fencing-disabled")

	ft, ok := lk.(locker.FencingTokener)
	require.True(t, ok, "should implement FencingTokener even without fencing enabled")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 未启用 Fencing Token 时应返回 0
	assert.Equal(t, int64(0), ft.Token(key))

	_ = lk.UnLock(ctx, key)
}

func TestFencingTokenerInterface(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithFencingToken(true),
	)

	// 类型断言应成功
	ft, ok := lk.(locker.FencingTokener)
	assert.True(t, ok, "should implement FencingTokener interface")

	ctx := context.Background()
	key := lockhelper.Key("test", "fencing-interface")

	err := ft.Lock(ctx, key)
	require.Nil(t, err)

	token := ft.Token(key)
	assert.Greater(t, token, int64(0), "token should be positive")

	_ = ft.UnLock(ctx, key)
}

// --- keyMu 引用计数清理测试 ---

func TestKeyMuCleanup(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key := lockhelper.Key("test", "keymu-cleanup")

	// 加锁 + 解锁
	err := lk.Lock(ctx, key)
	require.Nil(t, err)
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)

	// 再次加锁（应创建新的 keyMu 条目，验证清理后功能正常）
	err = lk.Lock(ctx, key)
	require.Nil(t, err, "should be able to re-lock after cleanup")
	err = lk.UnLock(ctx, key)
	require.Nil(t, err)
}

func TestKeyMuCleanupWithMultipleKeys(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client, redislock.WithWatchDog(false))

	ctx := context.Background()
	key1 := lockhelper.Key("test", "keymu-cleanup-1")
	key2 := lockhelper.Key("test", "keymu-cleanup-2")

	// 同时持有两个锁
	err := lk.Lock(ctx, key1)
	require.Nil(t, err)
	err = lk.Lock(ctx, key2)
	require.Nil(t, err)

	// 释放 key1，key2 应仍正常
	err = lk.UnLock(ctx, key1)
	require.Nil(t, err)

	// key2 仍可重入
	err = lk.Lock(ctx, key2)
	require.Nil(t, err)

	_ = lk.UnLock(ctx, key2)
	_ = lk.UnLock(ctx, key2)
}

// --- UnLock 前置检查测试 ---

func TestUnlockPreCheckNoEntry(t *testing.T) {
	client, _ := newTestClient(t)
	mm := newMockMetrics()
	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithMetrics(mm),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "precheck-noentry")

	// 从未加锁，UnLock 应返回 ErrLockNotFound（本地前置检查，不调 Redis）
	err := lk.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)

	// 仍应记录 metrics
	counters := mm.getCounter(locker.MetricRelease)
	assert.NotEmpty(t, counters, "should record lock.release counter even with pre-check")
}

func TestUnlockAfterLockLost(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithRecoveryProbe(false),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "unlock-after-lost")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁丢失
	mr.Del("lock:" + key)

	// 等待看门狗检测到锁丢失
	time.Sleep(1500 * time.Millisecond)

	// 看门狗清理了 entry 后，UnLock 应返回 ErrLockNotFound（前置检查）
	err = lk.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)
}

// --- 恢复探测测试 ---

func TestRecoveryProbeSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	mm := newMockMetrics()
	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithMaxRenewFailures(2),
		redislock.WithRecoveryProbe(true),
		redislock.WithMetrics(mm),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "recovery-probe")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 锁仍在 Redis 中，不触发丢失
	// 等待看门狗续期几次
	waitUntil(t, 5*time.Second, 200*time.Millisecond, func() bool {
		return len(mm.getCounter(locker.MetricRenew)) > 0
	}, "should record renew metrics")

	// 锁应仍然持有
	_ = lk.UnLock(ctx, key)

	// 不应触发锁丢失
	select {
	case k := <-lost:
		t.Fatalf("onLockLost should not be called, got key: %s", k)
	default:
	}
}

func TestRecoveryProbeDisabled(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithMaxRenewFailures(2),
		redislock.WithRecoveryProbe(false),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "no-recovery-probe")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 关闭 Redis 模拟连接故障
	mr.Close()

	// 禁用恢复探测时，达到最大失败次数应立即触发锁丢失
	select {
	case lostKey := <-lost:
		assert.Equal(t, key, lostKey)
	case <-time.After(10 * time.Second):
		t.Fatal("OnLockLost should have been called")
	}
}

// --- WithRenewTimeout 测试 ---

func TestCustomRenewTimeout(t *testing.T) {
	client, _ := newTestClient(t)
	lk := redislock.New(client,
		redislock.WithWatchDog(false),
		redislock.WithRenewTimeout(5*time.Second),
	)
	// 验证选项被接受，不 panic
	require.NotNil(t, lk)
}

// --- Watchdog 错误分类测试 ---

func TestWatchDogKeyNotFoundImmediateLoss(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lost := make(chan string, 1)
	lk := redislock.New(client,
		redislock.WithTimeout(3*time.Second),
		redislock.WithWatchDog(true),
		redislock.WithOnLockLost(func(key string) {
			lost <- key
		}),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "key-not-found-immediate")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 直接删除 Redis key，看门狗续期时应检测到 key 不存在
	mr.Del("lock:" + key)

	// 应立即触发锁丢失，不需要等待 maxRenewFailures 次
	select {
	case lostKey := <-lost:
		assert.Equal(t, key, lostKey)
	case <-time.After(3 * time.Second):
		t.Fatal("lock lost should be triggered immediately when key not found")
	}
}

// --- UnLock Redis 侧 -1/-2 错误返回测试 ---

func TestUnlockExpiredLockReturnsErrLockNotFound(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk := redislock.New(client,
		redislock.WithTimeout(1*time.Second),
		redislock.WithWatchDog(false),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "expired-unlock")

	err := lk.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁过期
	mr.FastForward(2 * time.Second)

	// UnLock 应返回 ErrLockNotFound（Redis key 已不存在，unlockScript 返回 -1）
	err = lk.UnLock(ctx, key)
	assert.Equal(t, locker.ErrLockNotFound, err)
}

func TestUnlockStolenLockReturnsErrNotLockOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lk1 := redislock.New(client,
		redislock.WithTimeout(1*time.Second),
		redislock.WithWatchDog(false),
	)

	ctx := context.Background()
	key := lockhelper.Key("test", "stolen-unlock")

	err := lk1.Lock(ctx, key)
	require.Nil(t, err)

	// 模拟锁过期
	mr.FastForward(2 * time.Second)

	// 另一个实例获取锁
	lk2 := redislock.New(client,
		redislock.WithTimeout(5*time.Second),
		redislock.WithWatchDog(false),
	)
	err = lk2.Lock(ctx, key)
	require.Nil(t, err)

	// lk1 UnLock 应返回 ErrNotLockOwner（Redis key 存在但 owner 不同，unlockScript 返回 -2）
	err = lk1.UnLock(ctx, key)
	assert.Equal(t, locker.ErrNotLockOwner, err)

	// lk2 仍可正常解锁
	err = lk2.UnLock(ctx, key)
	assert.Nil(t, err)
}
