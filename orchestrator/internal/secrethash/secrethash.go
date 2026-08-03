// Package secrethash hashes and verifies the two kinds of secret the
// orchestrator stores instead of the plaintext: bearer tokens (session,
// CSRF, runner credentials -- a fast, unsalted SHA-256 digest, since these
// are already high-entropy random values and are looked up by exact hash
// match) and user passwords (scrypt, salted, low-entropy human input). None
// of this touches the database or any gRPC type; it used to live as free
// functions inside package server -- see issue #285.
package secrethash

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// HashSecret returns the hex-encoded SHA-256 digest of value. Used to store
// and look up bearer tokens (session tokens, CSRF tokens, runner
// credentials, registration tokens) without ever persisting the token
// itself: these are already 256 bits of random data, so a fast unsalted
// digest is the right tool -- unlike a password, there is no low-entropy
// input for an attacker's precomputed table to target.
func HashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// ValidPassword reports whether password meets the orchestrator's minimum
// complexity bar: 8-1024 bytes, with at least one digit, uppercase letter,
// lowercase letter and symbol.
func ValidPassword(password string) bool {
	if len(password) < 8 || len(password) > 1024 {
		return false
	}
	var digit, upper, lower, symbol bool
	for _, character := range password {
		switch {
		case character >= '0' && character <= '9':
			digit = true
		case character >= 'A' && character <= 'Z':
			upper = true
		case character >= 'a' && character <= 'z':
			lower = true
		default:
			symbol = true
		}
	}
	return digit && upper && lower && symbol
}

// PasswordHash returns password encoded as a self-describing scrypt hash:
// "scrypt$16384$8$1$<salt>$<digest>", both base64url-encoded. It rejects a
// password that fails ValidPassword rather than hash it, so an invalid
// password can never be persisted.
func PasswordHash(password string) (string, error) {
	if !ValidPassword(password) {
		return "", errors.New("password is invalid")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := scrypt.Key([]byte(password), salt, 1<<14, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"scrypt", "16384", "8", "1", base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(digest)}, "$"), nil
}

// PasswordMatches reports whether password matches the stored, encoded
// scrypt hash. It intentionally always returns (false, nil) -- never an
// error -- for a hash that doesn't parse as the expected format: to the
// caller this must be indistinguishable from a wrong password, since
// otherwise the error return would give a timing/behavior oracle for which
// accounts have a corrupted password_hash row. That indistinguishability is
// exactly what makes a corrupted row silently unrecoverable, so the
// unparseable cases are logged here -- for an operator reading logs, not for
// the caller -- before returning.
func PasswordMatches(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" || parts[1] != "16384" || parts[2] != "8" || parts[3] != "1" {
		slog.Error("password hash has an unrecognized format", "encoding", parts[0])
		return false, nil
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		slog.Error("password hash has an invalid salt encoding", "error", err)
		return false, nil
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		slog.Error("password hash has an invalid digest encoding", "error", err)
		return false, nil
	}
	actual, err := scrypt.Key([]byte(password), salt, 1<<14, 8, 1, len(expected))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
