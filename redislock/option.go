package redislock

import "time"

func WithTimeout(expire time.Duration) func(*distributedRedisLock) {
	return func(d *distributedRedisLock) {
		if expire <= 0 {
			expire = defaultTimeout
		}
		d.timeout = expire
	}
}
