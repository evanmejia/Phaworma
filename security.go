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

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
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

// Access passwords are stored as hex(sha256(salt + ":" + password)) with a
// per-form random salt. I deliberately don't use bcrypt/argon2 here: the
// salt is per-form, the forms are short-lived, and I avoid a slow hash on
// every respondent submission. If I ever let respondents hold account
// passwords, I upgrade this.
func newAccessPasswordCredentials(password string) (salt, hash string, err error) {
	saltBytes := make([]byte, 16)
	if _, rerr := rand.Read(saltBytes); rerr != nil {
		err = fmt.Errorf("crypto/rand failed: %w", rerr)
		return
	}
	salt = hex.EncodeToString(saltBytes)
	sum := sha256.Sum256([]byte(salt + ":" + password))
	hash = hex.EncodeToString(sum[:])
	return
}

func verifyAccessPassword(password, salt, hash string) bool {
	if salt == "" || hash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(salt + ":" + password))
	candidate := hex.EncodeToString(sum[:])
	// hmac.Equal is my timing-safe comparison — I never use plain == here,
	// so a scanner can't fingerprint the password by response latency.
	return hmac.Equal([]byte(candidate), []byte(hash))
}
