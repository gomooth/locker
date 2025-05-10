package lockhelper

import "strings"

func Key(key string, keys ...string) string {
	if len(key) == 0 {
		key = "empty"
	}

	keys = append([]string{"lock", key}, keys...)
	return strings.Join(keys, ":")
}
