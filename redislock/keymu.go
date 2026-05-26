package redislock

import "sync"

// keyMuEntry per-key 互斥锁条目，带引用计数
type keyMuEntry struct {
	mu     sync.Mutex
	refCnt int
}

// keyMuMap per-key 互斥锁映射，自动清理无引用条目
type keyMuMap struct {
	mu   sync.Mutex
	data map[string]*keyMuEntry
}

func newKeyMuMap() *keyMuMap {
	return &keyMuMap{data: make(map[string]*keyMuEntry)}
}

// acquire 获取指定 key 的互斥锁条目，并递增引用计数
func (m *keyMuMap) acquire(key string) *keyMuEntry {
	m.mu.Lock()
	entry, ok := m.data[key]
	if !ok {
		entry = &keyMuEntry{}
		m.data[key] = entry
	}
	entry.refCnt++
	m.mu.Unlock()
	return entry
}

// release 递减引用计数，归零时从映射中删除条目
func (m *keyMuMap) release(key string) {
	m.mu.Lock()
	if entry, ok := m.data[key]; ok {
		entry.refCnt--
		if entry.refCnt <= 0 {
			delete(m.data, key)
		}
	}
	m.mu.Unlock()
}

