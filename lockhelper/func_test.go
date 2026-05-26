package lockhelper

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKey(t *testing.T) {
	assert.Equal(t, "", Key(""))
	assert.Equal(t, "app", Key("app"))
	assert.Equal(t, "app:biz", Key("app", "biz"))
	assert.Equal(t, "app:biz", Key("app", "", "biz"))
	assert.Equal(t, "app", Key("app", "", ""))
	assert.Equal(t, "a:b:c", Key("a", "b", "c"))
	assert.Equal(t, "app", Key("app", "", "", ""))
}
