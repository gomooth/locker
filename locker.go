package locker

// ILocker 锁约定
type ILocker interface {
	// Lock 获得锁
	Lock(key string) error
	// UnLock 释放锁
	UnLock(key string) error
}
