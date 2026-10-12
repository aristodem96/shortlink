package shortid

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGenerator_RejectsOutOfRangeLengths(t *testing.T) {
	t.Parallel()

	for _, length := range []int{-1, 0, MinLength - 1, MaxLength + 1} {
		_, err := NewGenerator(length)
		assert.Error(t, err, "length %d should be rejected", length)
	}
}

func TestGenerate_LengthAndAlphabet(t *testing.T) {
	t.Parallel()

	for length := MinLength; length <= 12; length++ {
		g, err := NewGenerator(length)
		require.NoError(t, err)

		code, err := g.Generate()
		require.NoError(t, err)
		require.Len(t, code, length)

		for _, r := range code {
			assert.True(t, strings.ContainsRune(Alphabet, r), "unexpected character %q in %q", r, code)
		}
	}
}

func TestGenerate_NoObviousCollisions(t *testing.T) {
	t.Parallel()

	g, err := NewGenerator(7)
	require.NoError(t, err)

	const n = 20_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		code, err := g.Generate()
		require.NoError(t, err)
		_, dup := seen[code]
		require.False(t, dup, "duplicate code %q after %d draws", code, i)
		seen[code] = struct{}{}
	}
}

// TestGenerate_IsNotBiased is a coarse uniformity check. With rejection
// sampling every character must appear roughly equally often; a naive
// `b % 62` would over-represent the first eight characters by ~1.25x and blow
// through this tolerance.
func TestGenerate_IsNotBiased(t *testing.T) {
	t.Parallel()

	g, err := NewGenerator(8)
	require.NoError(t, err)

	counts := make(map[byte]int, len(Alphabet))
	const draws = 20_000
	total := draws * 8
	for i := 0; i < draws; i++ {
		code, err := g.Generate()
		require.NoError(t, err)
		for j := 0; j < len(code); j++ {
			counts[code[j]]++
		}
	}

	expected := float64(total) / float64(len(Alphabet))
	for i := 0; i < len(Alphabet); i++ {
		c := Alphabet[i]
		got := float64(counts[c])
		assert.InEpsilonf(t, expected, got, 0.15,
			"character %q appeared %.0f times, expected about %.0f", c, got, expected)
	}
}

func TestIsValid(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		code string
		want bool
	}{
		"typical code":      {"aZ09xYz", true},
		"minimum length":    {"abcd", true},
		"too short":         {"abc", false},
		"too long":          {strings.Repeat("a", MaxLength+1), false},
		"empty":             {"", false},
		"hyphen not base62": {"abc-def", false},
		"slash":             {"ab/cd", false},
		"unicode":           {"абвг", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsValid(tc.code))
		})
	}
}

func BenchmarkGenerate(b *testing.B) {
	g, err := NewGenerator(7)
	require.NoError(b, err)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := g.Generate(); err != nil {
			b.Fatal(err)
		}
	}
}
