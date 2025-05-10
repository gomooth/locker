package redislock

import (
	"time"
)

const (
	// 默认超时时间，5分钟有效
	defaultTimeout = 5 * 60 * time.Second
)
