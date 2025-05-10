# redislock

Redis 分布式锁


## Examples

```golang
package main

import (
	"time"

	"github.com/gomooth/locker/lockhelper"
	"github.com/gomooth/locker/redislock"
	"github.com/redis/go-redis/v9"
)

var client *redis.Client

func init() {
	client = redis.NewClient(&redis.Options{
		Addr:     "127.0.0.1:6379",
		Password: "",
		DB:       0,
	})
}

func main() {
	lock := redislock.New(client, redislock.WithTimeout(15*time.Minute))

	key := lockhelper.Key("app", "package", "business")
	if err := lock.Lock(key); err != nil {
		panic(err)
	}
	defer func() {
		_ = lock.UnLock(key)
	}()

	// do something ...
}

```



