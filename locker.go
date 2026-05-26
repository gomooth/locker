package locker

import (
	"context"
	"errors"
)

var (
	ErrLockOccupied = errors.New("lock is occupied")
	ErrLockNotFound = errors.New("lock not found")
	ErrNotLockOwner = errors.New("not lock owner")
	ErrEmptyKey     = errors.New("empty lock key")
	ErrLockerClosed = errors.New("locker is closed")
)

// ILocker 分布式锁约定
//
// 每个 ILocker 实例拥有唯一的 owner 标识，同一实例对同一 key 的加锁为可重入。
// 如需不同协程独立竞争同一 key，应各自创建独立的 ILocker 实例，
// 而非共享同一实例（共享实例时协程间可互相解锁）。
type ILocker interface {
	// Lock 获得锁（支持重试和 context 取消，Redis 错误也会重试）
	Lock(ctx context.Context, key string) error
	// TryLock 尝试获得锁，不重试
	TryLock(ctx context.Context, key string) error
	// UnLock 释放锁（可重入时递减计数，计数归零才完全释放）
	UnLock(ctx context.Context, key string) error
	// Close 释放锁实例资源（停止看门狗等）。
	// Close 不会主动释放 Redis 中的锁，锁将依赖 TTL 自然过期。
	// 调用 Close 后再调用 Lock/UnLock 将返回 ErrLockerClosed。
	Close() error
	// Wait 阻塞等待所有看门狗 goroutine 和锁丢失回调 goroutine 退出。
	// 必须在 Close() 之后调用，且 Close() 应在所有 Lock/UnLock 调用完成后调用。
	// 典型用法：lk.Close(); lk.Wait()
	// 注意：在 Close() 之前调用 Wait() 可能导致 Wait() 在新的看门狗 goroutine 启动前返回。
	Wait() error
}

// FencingTokener 可选接口，支持 Fencing Token 的锁实现。
//
// Fencing Token 是单调递增的令牌，用于防止分布式锁的"延迟解锁"问题：
// 当持有锁的进程因 GC 停顿等原因暂停后继续操作共享资源时，
// 共享资源可通过校验 token 值拒绝旧请求。
//
// 使用方式：通过类型断言检查锁实例是否支持 Fencing Token：
//
//	ft, ok := lk.(locker.FencingTokener)
//	if ok { token := ft.Token(key) }
//
// 共享资源侧应存储最近见过的最大 token 值，
// 拒绝 token 小于已存储值的请求。
type FencingTokener interface {
	ILocker
	// Token 返回当前锁的 Fencing Token。
	// 必须在 Lock/TryLock 成功后调用。
	// 返回 0 表示未持有锁、Fencing Token 未启用或 token 生成失败。
	Token(key string) int64
}
