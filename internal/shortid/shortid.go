// Package shortid generates the random base62 codes used as short links.
//
// Codes are random rather than derived from a database sequence on purpose: a
// sequential code is trivially enumerable, which would let anyone walk the
// whole corpus of links (and their targets) by counting upwards.
package shortid

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// Alphabet is the base62 character set. It contains no separators, so codes are
// safe in a URL path without escaping.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const (
	// MinLength keeps the keyspace large enough that guessing is impractical.
	MinLength = 4
	// MaxLength matches the alias limit so both kinds of code share one column.
	MaxLength = 32

	// maxUnbiased is the largest multiple of len(Alphabet) that fits in a byte.
	// Rejecting bytes at or above it removes the modulo bias that a plain
	// b % 62 would introduce for the first four characters of the alphabet.
	maxUnbiased = 256 - (256 % len(Alphabet)) // 248
)

// Generator produces random codes of a fixed length.
type Generator struct {
	length int
}

// NewGenerator returns a generator emitting codes of the given length.
func NewGenerator(length int) (*Generator, error) {
	if length < MinLength || length > MaxLength {
		return nil, fmt.Errorf("shortid: length %d out of range [%d,%d]", length, MinLength, MaxLength)
	}
	return &Generator{length: length}, nil
}

// Length reports the length of the codes this generator produces.
func (g *Generator) Length() int { return g.length }

// Generate returns a new uniformly distributed base62 code.
func (g *Generator) Generate() (string, error) {
	var sb strings.Builder
	sb.Grow(g.length)

	// Read in chunks and reject biased bytes. Over-reading by a quarter makes a
	// second syscall unlikely without wasting much entropy.
	buf := make([]byte, g.length+g.length/4+4)
	for sb.Len() < g.length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("shortid: read entropy: %w", err)
		}
		for _, b := range buf {
			if int(b) >= maxUnbiased {
				continue
			}
			sb.WriteByte(Alphabet[int(b)%len(Alphabet)])
			if sb.Len() == g.length {
				break
			}
		}
	}
	return sb.String(), nil
}

// IsValid reports whether s could have been produced by a generator: the right
// length range and base62 characters only.
func IsValid(s string) bool {
	if len(s) < MinLength || len(s) > MaxLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}
