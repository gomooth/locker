package redislock

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/gomooth/locker"

	"github.com/redis/go-redis/v9"

	"github.com/save95/xerror"
)

type distributedRedisLock struct {
	client *redis.Client

	lockVal string
	timeout time.Duration
}

// New 创建分布式 redis 锁
func New(client *redis.Client, opts ...func(*distributedRedisLock)) locker.ILocker {
	// 生成随机值作为锁的内容
	// 在释放时，通过该值判断是否为持有者。只有锁的持有者才能释放
	val := fmt.Sprintf("%d_%d", time.Now().Nanosecond(), rand.Int64())

	lock := &distributedRedisLock{
		client:  client,
		lockVal: val,
		timeout: defaultTimeout,
	}

	for _, opt := range opts {
		opt(lock)
	}

	return lock
}

func (d *distributedRedisLock) wrapKey(key string) string {
	if strings.HasPrefix(key, "lock:") {
		return key
	}
	return fmt.Sprintf("lock:%s", key)
}

func (d *distributedRedisLock) Lock(key string) error {
	key = d.wrapKey(key)
	// 加锁
	ctx := context.Background()
	set, err := d.client.SetNX(ctx, key, d.lockVal, d.timeout).Result()
	if err != nil {
		return xerror.Wrap(err, "get lock failed")
	}

	// 锁被占用
	if !set {
		return locker.ErrLockOccupied
	}

	return nil
}

func (d *distributedRedisLock) UnLock(key string) error {
	key = d.wrapKey(key)
	// 获得锁信息
	ctx := context.Background()
	val, err := d.client.Get(ctx, key).Result()
	if nil != err {
		return xerror.Wrap(err, "found lock failed")
	}

	// 判断锁的持有者
	if val != d.lockVal {
		return xerror.New("unlock failed, not owner")
	}

	// 删除锁
	err = d.client.Del(ctx, key).Err()
	if nil != err {
		return xerror.Wrap(err, "unlock failed")
	}

	return nil
}
