package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

const (
	// KeyPrefix starts every secret, so leaked keys are easy to spot in logs
	// and by secret scanners.
	KeyPrefix = "sk-ooa-"

	// secretBytes of randomness (256 bits) follow the prefix.
	secretBytes = 32
	// displayLen is how much of the secret the UI shows to tell keys apart:
	// the prefix plus four random characters.
	displayLen = len(KeyPrefix) + 4

	maxNameLen = 64

	// cacheTTL bounds how long a revoked key could still work if it was
	// revoked by another process. Revoke in this process drops the entry
	// immediately.
	cacheTTL = 30 * time.Second
	// lastUsedEvery keeps last_used_at off the hot path: one write per key
	// per minute is plenty for a "last used" column.
	lastUsedEvery = time.Minute
)

// Key is a stored API key without its secret. Timestamps are unix
// milliseconds.
type Key struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
}

// Keys manages API keys in the api_keys table and authenticates them. Only
// the SHA-256 of a secret is stored.
type Keys struct {
	db  *store.DB
	now func() time.Time

	mu       sync.Mutex
	cache    map[string]cached    // by secret hash
	lastUsed map[string]time.Time // by key id
	// revokes counts Revoke calls. Authenticate only caches a lookup when no
	// revoke happened while it was reading, otherwise a key revoked between
	// the read and the cache write would be cached as live.
	revokes uint64
}

type cached struct {
	id      string
	expires time.Time
}

// NewKeys returns a Keys service on db.
func NewKeys(db *store.DB) *Keys {
	return &Keys{db: db, now: time.Now, cache: map[string]cached{}, lastUsed: map[string]time.Time{}}
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Create stores a new key and returns it with its secret, which cannot be
// retrieved again.
func (k *Keys) Create(ctx context.Context, name string) (Key, string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxNameLen {
		return Key{}, "", httpx.Unprocessable("name must be 1 to %d characters", maxNameLen)
	}
	var raw [secretBytes]byte
	// crypto/rand.Read does not fail; it aborts the process if the OS
	// source breaks.
	_, _ = rand.Read(raw[:])
	secret := KeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])

	key := Key{
		ID:        store.NewID(store.PrefixAPIKey),
		Name:      name,
		Prefix:    secret[:displayLen],
		CreatedAt: store.Millis(k.now()),
	}
	_, err := k.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, prefix, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		key.ID, key.Name, key.Prefix, hashSecret(secret), key.CreatedAt)
	if err != nil {
		return Key{}, "", fmt.Errorf("auth: insert key: %w", err)
	}
	return key, secret, nil
}

// List returns the non-revoked keys, newest first.
func (k *Keys) List(ctx context.Context) ([]Key, error) {
	rows, err := k.db.QueryContext(ctx,
		`SELECT id, name, prefix, created_at, last_used_at FROM api_keys
		 WHERE revoked_at IS NULL ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("auth: list keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := []Key{}
	for rows.Next() {
		var key Key
		var used sql.NullInt64
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &used); err != nil {
			return nil, fmt.Errorf("auth: scan key: %w", err)
		}
		if used.Valid {
			key.LastUsedAt = &used.Int64
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list keys: %w", err)
	}
	return keys, nil
}

// Count returns the number of non-revoked keys.
func (k *Keys) Count(ctx context.Context) (int, error) {
	var n int
	err := k.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("auth: count keys: %w", err)
	}
	return n, nil
}

// Revoke disables a key. Revoking an already revoked key succeeds; an
// unknown id is a 404.
func (k *Keys) Revoke(ctx context.Context, id string) error {
	res, err := k.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
		store.Millis(k.now()), id)
	if err != nil {
		return fmt.Errorf("auth: revoke key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: revoke key: %w", err)
	}
	if n == 0 {
		return httpx.NotFound("api key %q does not exist", id)
	}
	k.mu.Lock()
	for h, c := range k.cache {
		if c.id == id {
			delete(k.cache, h)
		}
	}
	delete(k.lastUsed, id)
	k.revokes++
	k.mu.Unlock()
	return nil
}

// Authenticate reports whether secret is a live key. The secret is hashed
// before any comparison, so lookups do not leak how much of a guess matched.
// Only successes are cached, which keeps the cache bounded by the number of
// real keys.
func (k *Keys) Authenticate(ctx context.Context, secret string) bool {
	if !strings.HasPrefix(secret, KeyPrefix) {
		return false
	}
	hash := hashSecret(secret)
	now := k.now()

	k.mu.Lock()
	c, ok := k.cache[hash]
	if ok && !now.Before(c.expires) {
		delete(k.cache, hash)
		ok = false
	}
	revokes := k.revokes
	k.mu.Unlock()

	id := c.id
	if !ok {
		err := k.db.QueryRowContext(ctx,
			`SELECT id FROM api_keys WHERE hash = ? AND revoked_at IS NULL`, hash).Scan(&id)
		if err != nil {
			// Unknown key and database error both deny; failing closed is
			// the safe answer for the second.
			return false
		}
		k.mu.Lock()
		if k.revokes == revokes {
			k.cache[hash] = cached{id: id, expires: now.Add(cacheTTL)}
		}
		k.mu.Unlock()
	}
	k.touch(ctx, id, now)
	return true
}

// touch records last use at most once per lastUsedEvery per key. A failed
// write is not worth failing the request for.
func (k *Keys) touch(ctx context.Context, id string, now time.Time) {
	k.mu.Lock()
	last, seen := k.lastUsed[id]
	due := !seen || now.Sub(last) >= lastUsedEvery
	if due {
		k.lastUsed[id] = now
	}
	k.mu.Unlock()
	if !due {
		return
	}
	_, _ = k.db.ExecContext(context.WithoutCancel(ctx),
		`UPDATE api_keys SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL`, store.Millis(now), id)
}
