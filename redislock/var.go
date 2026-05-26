package redislock

import "time"

const (
	defaultTimeout          = 30 * time.Second
	defaultRetryCount       = 0
	defaultRetryInterval    = 200 * time.Millisecond
	defaultWatchDog         = true
	defaultMaxRenewFailures = 3
	defaultKeyPrefix        = "lock:"
	defaultRenewTimeout     = 0 // 0 表示使用 timeout/2
	defaultRecoveryProbe    = true
)
