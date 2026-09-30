// Package password hashes account passwords with Argon2id.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	MinLen = 8
	MaxLen = 256 // bounds the hashing work an attacker can ask for

	// OWASP's recommended Argon2id settings: cheap enough for a small
	// server to take many sign-ins, slow enough to hamper offline cracking.
	memory  = 19 * 1024 // KiB
	time    = 2
	threads = 1
	keyLen  = 32
	saltLen = 16
)

var errFormat = errors.New("unrecognised password hash")

// Check reports whether pw is an acceptable password.
func Check(pw string) error {
	if len(pw) < MinLen || len(pw) > MaxLen {
		return fmt.Errorf("password must be %d-%d characters", MinLen, MaxLen)
	}
	return nil
}

// Hash returns a PHC-format Argon2id hash of pw.
func Hash(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, time, memory, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether pw matches hash. The parameters are read from the
// hash, so hashes made with older settings keep working.
func Verify(pw, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errFormat
	}
	var v int
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errFormat
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errFormat
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummy is verified against when there is no real hash, so that a sign-in
// for an unknown user takes as long as one for a known user.
var dummy, _ = Hash("pogo-pad-dummy-password")

// VerifyNothing does the work of a Verify that always fails.
func VerifyNothing(pw string) {
	Verify(pw, dummy)
}
