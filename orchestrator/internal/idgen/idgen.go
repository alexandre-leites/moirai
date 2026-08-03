// Package idgen generates and validates the opaque identifiers the
// orchestrator hands out: UUIDv4 primary keys and random, URL-safe bearer
// secrets (session/CSRF tokens, runner credentials, registration tokens).
// None of it touches the database or any gRPC type, which is exactly why it
// used to live as free functions inside package server -- see issue #285.
package idgen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// NewID returns a random UUIDv4, formatted the same way the database's own
// id columns are: lowercase hex with dashes.
func NewID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// RandomSecret returns a 256-bit random value, base64url-encoded without
// padding. Used for every bearer secret this server issues: session tokens,
// CSRF tokens, runner credentials and registration tokens.
func RandomSecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// ValidID reports whether value has the exact shape NewID produces: a
// lowercase-or-uppercase-hex UUID with dashes at the canonical positions.
// It does not check that the id refers to an existing row -- callers still
// need the database round trip for that -- only that the value is safe to
// use in an equality-keyed lookup.
func ValidID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
