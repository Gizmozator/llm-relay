// Package auth: password hashing, JWT issuing/verification, API key
// generation and hashing.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const KeyPrefixLen = 11 // "sk-" + 8 hex chars, kept for display

// HashPassword hashes a plaintext password with bcrypt.
func HashPassword(pass string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether pass matches the bcrypt hash.
func CheckPassword(hash, pass string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}

// GenerateAPIKey returns a fresh key ("sk-" + 40 hex chars) and its SHA-256
// hex hash. Only the hash is ever stored.
func GenerateAPIKey() (plain, hash, prefix string, err error) {
	buf := make([]byte, 20)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("key rand: %w", err)
	}
	plain = "sk-" + hex.EncodeToString(buf)
	return plain, HashKey(plain), plain[:KeyPrefixLen], nil
}

// GenerateServerToken returns a fresh federation server token ("fed-" +
// 40 hex chars) and its SHA-256 hex digest. Only the hash is ever stored.
// The distinct prefix is deliberate: it makes the token self-describing
// about which surface it authenticates (sk- → /v1/*, fed- → /federation/*),
// so a leaked string is visibly out of scope in any log or review.
func GenerateServerToken() (plain, hash string, err error) {
	buf := make([]byte, 20)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("token rand: %w", err)
	}
	plain = "fed-" + hex.EncodeToString(buf)
	return plain, HashKey(plain), nil
}

// GenerateRemoteToken returns a fresh remote-backend connect token
// ("rb-" + 40 hex chars) and its SHA-256 hex digest. Only the hash is ever
// stored; the raw token is shown once at create/rotate. The prefix keeps the
// surfaces self-describing: sk- → /v1/*, fed- → /federation/*, rb- →
// /remote/v1/*.
func GenerateRemoteToken() (plain, hash string, err error) {
	buf := make([]byte, 20)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("token rand: %w", err)
	}
	plain = "rb-" + hex.EncodeToString(buf)
	return plain, HashKey(plain), nil
}

// HashKey returns the SHA-256 hex digest of an API key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Claims carried in the dashboard JWT.
type Claims struct {
	UserID int64  `json:"uid"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// SignToken issues an HS256 JWT valid for the given duration.
func SignToken(secret string, userID int64, role string, ttl time.Duration) (string, error) {
	if len(secret) < minSecretLen {
		return "", ErrWeakSecret
	}
	claims := Claims{
		UserID: userID,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

var ErrBadToken = errors.New("bad token")

// ErrWeakSecret refuses to sign or verify with a secret below the HS256
// floor. The config boundary enforces the same length, but the auth
// package is the last line: a short secret makes tokens offline-
// bruteforceable, and no caller should be able to bypass the check by
// constructing a Server (or a test) directly.
var ErrWeakSecret = fmt.Errorf("jwt secret must be at least %d bytes", minSecretLen)

// minSecretLen is RFC 7518's floor for HS256: the key must be at least as
// long as the hash output (32 bytes).
const minSecretLen = 32

// ParseToken verifies a dashboard JWT and returns its claims.
func ParseToken(secret, token string) (*Claims, error) {
	if len(secret) < minSecretLen {
		return nil, ErrWeakSecret
	}
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrBadToken
		}
		return []byte(secret), nil
	})
	if err != nil || !parsed.Valid {
		return nil, ErrBadToken
	}
	return claims, nil
}
