package redislock_test

import (
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/gomooth/locker/lockhelper"
	"github.com/gomooth/locker/redislock"

	"github.com/redis/go-redis/v9"

	"github.com/stretchr/testify/assert"
)

var _redisClient *redis.Client

func init() {
	_redisClient = redis.NewClient(&redis.Options{
		Addr:     "127.0.0.1:6379",
		Password: "",
		DB:       0,
	})

	ctx := context.Background()
	_, err := _redisClient.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("err: %+v", err)
	}

}

func TestNewDistributedRedisLock(t *testing.T) {
	lock := redislock.New(_redisClient)
	key := lockhelper.Key("distributed_redis", "test")

	// 加锁
	err := lock.Lock(key)
	assert.Nil(t, err)

	var wg sync.WaitGroup
	// 并发抢锁
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			err := lock.Lock(key)
			assert.NotNil(t, err)
		}(i)
	}
	wg.Wait()

	// 释放锁
	err = lock.UnLock(key)
	assert.Nil(t, err)

	// 并发抢锁，过程展示
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			err := lock.Lock(key)
			log.Printf("[%s][%d] get lock: %+v\n", time.Now().Format(".00000"), i, err)
		}(i)
	}
	wg.Wait()

	err = lock.UnLock(key)
	log.Printf("unlock: %+v\n", err)
}
