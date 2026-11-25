package auth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newKeys(t *testing.T) (*Keys, *store.DB, *clock) {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	k := NewKeys(db)
	c := &clock{t: time.UnixMilli(1_700_000_000_000)}
	k.now = c.now
	return k, db, c
}

func TestKeyLifecycle(t *testing.T) {
	ctx := t.Context()
	k, _, clk := newKeys(t)

	a, secretA, err := k.Create(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	b, secretB, err := k.Create(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secretA, "sk-ooa-") || len(secretA) < len("sk-ooa-")+43 {
		t.Fatalf("secret shape %q", secretA)
	}
	if a.Prefix != secretA[:11] || secretA == secretB {
		t.Fatalf("prefix %q", a.Prefix)
	}

	keys, err := k.List(ctx)
	if err != nil || len(keys) != 2 || keys[0].ID != b.ID || keys[1].ID != a.ID {
		t.Fatalf("List = %+v, %v", keys, err)
	}
	if n, _ := k.Count(ctx); n != 2 {
		t.Fatalf("Count = %d", n)
	}

	if !k.Authenticate(ctx, secretA) || k.Authenticate(ctx, secretA+"x") || k.Authenticate(ctx, "") || k.Authenticate(ctx, "sk-ooa-") {
		t.Fatal("authenticate results wrong")
	}

	if err := k.Revoke(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if k.Authenticate(ctx, secretA) {
		t.Fatal("revoked key still authenticates (cache not dropped)")
	}
	if !k.Authenticate(ctx, secretB) {
		t.Fatal("other key affected")
	}
	if keys, _ := k.List(ctx); len(keys) != 1 || keys[0].ID != b.ID {
		t.Fatalf("List after revoke = %+v", keys)
	}
	if n, _ := k.Count(ctx); n != 1 {
		t.Fatalf("Count = %d", n)
	}
	if err := k.Revoke(ctx, a.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	err = k.Revoke(ctx, "k_missing")
	if he := httpx.AsError(err); he.Status != http.StatusNotFound {
		t.Fatalf("unknown revoke = %v", err)
	}
}

func TestKeyNameValidation(t *testing.T) {
	k, _, _ := newKeys(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 65)} {
		_, _, err := k.Create(t.Context(), name)
		if he := httpx.AsError(err); he.Status != http.StatusUnprocessableEntity {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, _, err := k.Create(t.Context(), strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyHashStored(t *testing.T) {
	k, db, _ := newKeys(t)
	_, secret, err := k.Create(t.Context(), "k")
	if err != nil {
		t.Fatal(err)
	}
	var hash, prefix string
	if err := db.QueryRowContext(t.Context(), `SELECT hash, prefix FROM api_keys`).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != hashSecret(secret) || len(hash) != 64 || strings.Contains(hash, secret) || prefix == secret {
		t.Fatalf("hash=%q prefix=%q", hash, prefix)
	}
}

func TestLastUsedThrottled(t *testing.T) {
	ctx := t.Context()
	k, db, clk := newKeys(t)
	key, secret, _ := k.Create(ctx, "k")

	lastUsed := func() sql.NullInt64 {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT last_used_at FROM api_keys WHERE id = ?`, key.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if lastUsed().Valid {
		t.Fatal("last_used_at set before use")
	}
	start := clk.t
	k.Authenticate(ctx, secret)
	if got := lastUsed(); !got.Valid || got.Int64 != start.UnixMilli() {
		t.Fatalf("first use = %v", got)
	}
	clk.t = start.Add(30 * time.Second)
	k.Authenticate(ctx, secret)
	if got := lastUsed().Int64; got != start.UnixMilli() {
		t.Fatalf("written within a minute: %d", got)
	}
	clk.t = start.Add(61 * time.Second)
	k.Authenticate(ctx, secret)
	if got := lastUsed().Int64; got != clk.t.UnixMilli() {
		t.Fatalf("not refreshed after a minute: %d", got)
	}
}

func TestCacheExpiresAgainstDatabase(t *testing.T) {
	ctx := t.Context()
	k, db, clk := newKeys(t)
	key, secret, _ := k.Create(ctx, "k")
	k.Authenticate(ctx, secret)
	// Revoked behind the service's back, as another process would.
	if _, err := db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = 1 WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}
	if !k.Authenticate(ctx, secret) {
		t.Fatal("cache should still serve within the ttl")
	}
	clk.t = clk.t.Add(31 * time.Second)
	if k.Authenticate(ctx, secret) {
		t.Fatal("expired cache entry must be rechecked")
	}
}

func TestKeyJSON(t *testing.T) {
	k, _, _ := newKeys(t)
	key, _, _ := k.Create(t.Context(), "k")
	b, _ := json.Marshal(key)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["last_used_at"]; !ok || v != nil {
		t.Fatalf("last_used_at = %v (present %v), want null", v, ok)
	}
	if _, ok := m["created_at"].(float64); !ok {
		t.Fatalf("created_at = %v", m["created_at"])
	}
}
