// security.go — my short-link generation and access-password helpers.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/crypto/argon2"
)

// shortCodeAlphabet — 32 characters. I dropped 0/O/1/l because my codes get
// read aloud and retyped by humans; ambiguous characters are how support
// tickets get created.
const shortCodeAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

const shortCodeLength = 6

// generateShortCode returns a collision-free short code. 32^6 ≈ 1 billion
// combinations, so the retry loop is almost never exercised — but I bound
// it anyway so a pathological table can't spin the CPU. (No modulo bias:
// 256 is divisible by 32, so byte % len(alphabet) is uniform.)
func generateShortCode(app core.App) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		buf := make([]byte, shortCodeLength)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("crypto/rand failed: %w", err)
		}
		code := make([]byte, shortCodeLength)
		for i, b := range buf {
			code[i] = shortCodeAlphabet[int(b)%len(shortCodeAlphabet)]
		}
		shortCode := string(code)

		existing, err := app.FindFirstRecordByFilter(formsCollectionName,
			"short_code = {:code}", dbx.Params{"code": shortCode})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if existing != nil {
			continue // collision — regenerate
		}
		return shortCode, nil
	}
	return "", errors.New("could not allocate a unique short code")
}

// Access passwords are stored as argon2id (audit #1) with a per-form
// random salt — slow on purpose, so a leaked hash can't be brute-forced
// offline and an online attacker pays real CPU for every guess. The
// cost per check is the point; the per-IP rate limiting in main.go caps
// how often an attacker can spend it.
//
// Storage reuses my existing columns — no migration: the salt is hex in
// access_password_salt (32 chars vs Max 64) and the hash is
// argon2idHashPrefix + hex in access_password_hash (73 chars vs Max 128).
// A stored hash WITHOUT the prefix is a legacy hex(sha256(salt + ":" +
// password)) entry from before this upgrade: it keeps verifying, and my
// verify call sites re-hash it in place the first time the right password
// is seen (upgradeLegacyAccessPassword).
const argon2idHashPrefix = "argon2id$"

// My argon2id parameters, sized for one LXC: 64 MiB of memory per hash,
// 3 iterations, 1 lane — above the OWASP online-attack floor, well under
// the 192 MiB "high security" tier that a small container shouldn't
// allocate per request. Tune here if the machine changes.
const (
	argon2idMemory    = 64 << 20
	argon2idTime      = 3
	argon2idThreads   = 1
	argon2idKeyLength = 32
)

func argon2idKey(password, salt []byte) []byte {
	return argon2.IDKey(password, salt, argon2idTime, argon2idMemory, argon2idThreads, argon2idKeyLength)
}

func newAccessPasswordCredentials(password string) (salt, hash string, err error) {
	saltBytes := make([]byte, 16)
	if _, rerr := rand.Read(saltBytes); rerr != nil {
		err = fmt.Errorf("crypto/rand failed: %w", rerr)
		return
	}
	salt = hex.EncodeToString(saltBytes)
	hash = argon2idHashPrefix + hex.EncodeToString(argon2idKey([]byte(password), []byte(salt)))
	return
}

func verifyAccessPassword(password, salt, hash string) bool {
	if salt == "" || hash == "" {
		return false
	}
	if strings.HasPrefix(hash, argon2idHashPrefix) {
		want, derr := hex.DecodeString(strings.TrimPrefix(hash, argon2idHashPrefix))
		if derr != nil {
			// A corrupt entry must read as "wrong password", not crash.
			return false
		}
		got := argon2idKey([]byte(password), []byte(salt))
		return hmac.Equal(got, want)
	}
	// Legacy entry from before my argon2id upgrade — the fast path it
	// always had; my callers re-hash it after a successful verify.
	sum := sha256.Sum256([]byte(salt + ":" + password))
	candidate := hex.EncodeToString(sum[:])
	// hmac.Equal is my timing-safe comparison — I never use plain == here,
	// so a scanner can't fingerprint the password by response latency.
	return hmac.Equal([]byte(candidate), []byte(hash))
}

// isLegacyAccessPasswordHash tells my verify call sites whether a
// SUCCESSFUL check ran against a pre-upgrade sha256 entry — the one state
// worth re-hashing, because the password is known-good in hand.
func isLegacyAccessPasswordHash(hash string) bool {
	return hash != "" && !strings.HasPrefix(hash, argon2idHashPrefix)
}

// upgradeLegacyAccessPassword re-stores a just-verified access password
// with my argon2id scheme. Fire-and-forget on purpose, matching my other
// non-fatal side effects: the respondent's request already succeeded, and
// a failed re-hash just means the legacy entry keeps working until the
// next correct password retries the upgrade. Concurrent upgrades are
// harmless — both writes are valid entries for the same password.
func upgradeLegacyAccessPassword(app core.App, form *core.Record, password string) {
	salt, hash, err := newAccessPasswordCredentials(password)
	if err != nil {
		app.Logger().Warn("failed to re-hash legacy access password",
			"form", form.GetString("id"), "error", err)
		return
	}
	form.Set("access_password_hash", hash)
	form.Set("access_password_salt", salt)
	if serr := app.Save(form); serr != nil {
		app.Logger().Warn("failed to persist upgraded access password",
			"form", form.GetString("id"), "error", serr)
	}
}
