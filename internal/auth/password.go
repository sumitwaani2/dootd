// Package auth implements dashboard authentication: one admin user with an
// argon2id password hash, server-side sessions, CSRF tokens and login
// rate limiting (docs/architecture.md §14).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: 64 MiB, 3 passes, 1 lane.
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLen is the minimum password length.
const MinPasswordLen = 12

// loginSlots bounds concurrent argon2 computations (64 MiB each), so a
// burst of logins cannot exhaust a 1 GB VPS.
var loginSlots = make(chan struct{}, 2)

// HashPassword returns a PHC-style argon2id hash string.
func HashPassword(pw string) (string, error) {
	if err := CheckPassword(pw); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	loginSlots <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-loginSlots
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword enforces the password policy.
func CheckPassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}

// VerifyPassword checks pw against a hash from HashPassword in constant time.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || m > 1<<20 || t == 0 || t > 16 || p == 0 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	loginSlots <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-loginSlots
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when the email is unknown, so timing does
// not reveal whether the email matched.
var dummyHash = sync.OnceValue(func() string {
	salt := make([]byte, saltLen)
	key := argon2.IDKey([]byte("dummy password!"), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key))
})
