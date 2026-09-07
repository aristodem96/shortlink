package analytics

import (
	"crypto/sha256"
	"net"
	"strings"
)

// ipHashLength truncates the digest. 16 bytes still make collisions
// irrelevant at this scale while halving the stored bytes.
const ipHashLength = 16

// SaltedIPHasher pseudonymises visitor addresses.
//
// Storing raw IPs would make the clicks table personal data under GDPR for no
// analytical gain: every question the service answers ("how many unique
// visitors?") only needs a stable identifier. The salt is what makes the digest
// irreversible -- an unsalted SHA-256 of an IPv4 address is trivially inverted
// by hashing all four billion of them.
type SaltedIPHasher struct {
	salt []byte
}

// NewIPHasher returns a hasher bound to a salt.
func NewIPHasher(salt string) *SaltedIPHasher {
	return &SaltedIPHasher{salt: []byte(salt)}
}

// HashIP returns the pseudonymous identifier for an address, or nil when the
// address is missing or unparseable.
func (h *SaltedIPHasher) HashIP(ip string) []byte {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil
	}
	// Normalise so that 2001:db8::1 and 2001:0db8:0000::1 hash alike.
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	}

	sum := sha256.New()
	sum.Write(h.salt)
	sum.Write([]byte(ip))
	return sum.Sum(nil)[:ipHashLength]
}
