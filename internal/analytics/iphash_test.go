package analytics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSaltedIPHasher(t *testing.T) {
	t.Parallel()

	h := NewIPHasher("pepper")

	t.Run("is stable for the same address", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, h.HashIP("203.0.113.7"), h.HashIP("203.0.113.7"))
	})

	t.Run("differs between addresses", func(t *testing.T) {
		t.Parallel()
		assert.NotEqual(t, h.HashIP("203.0.113.7"), h.HashIP("203.0.113.8"))
	})

	t.Run("differs between salts", func(t *testing.T) {
		t.Parallel()
		// Rotating the salt must invalidate old digests, otherwise the salt
		// would give no protection once leaked.
		assert.NotEqual(t, h.HashIP("203.0.113.7"), NewIPHasher("other").HashIP("203.0.113.7"))
	})

	t.Run("normalises equivalent IPv6 forms", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, h.HashIP("2001:db8::1"), h.HashIP("2001:0db8:0000:0000:0000:0000:0000:0001"))
	})

	t.Run("returns nil for a missing address", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, h.HashIP(""))
		assert.Nil(t, h.HashIP("   "))
	})

	t.Run("does not embed the address", func(t *testing.T) {
		t.Parallel()
		assert.NotContains(t, string(h.HashIP("203.0.113.7")), "203.0.113.7")
	})

	t.Run("has the documented length", func(t *testing.T) {
		t.Parallel()
		assert.Len(t, h.HashIP("203.0.113.7"), ipHashLength)
	})
}
