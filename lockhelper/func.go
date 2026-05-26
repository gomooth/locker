package lockhelper

import "strings"

// Key 拼接锁键名，不含前缀。前缀由锁实现负责添加。
// 自动过滤空字符串片段，避免产生 "app::biz" 格式的键名。
func Key(key string, keys ...string) string {
	if key == "" {
		return ""
	}
	parts := make([]string, 0, 1+len(keys))
	parts = append(parts, key)
	for _, k := range keys {
		if k != "" {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, ":")
}
