package locker

import "errors"

var (
	ErrLockOccupied = errors.New("lock is occupied")
)
