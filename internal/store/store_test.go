package store

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesSchema(t *testing.T) {
	db := openTest(t)
	for _, table := range []string{"models", "deployments", "api_keys", "settings"} {
		var name string
		err := db.QueryRowContext(t.Context(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestOpenPragmas(t *testing.T) {
	db := openTest(t)
	var mode string
	if err := db.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := range 2 {
		db, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		_ = db.Close()
	}
}

func insertModel(t *testing.T, db *DB, id, name string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO models (id, name, reference, engine_image, memory_mb, created_at, updated_at)
		 VALUES (?, ?, 'registry.example/m:1', 'vllm/vllm-openai:v0.29.0', 1024, 0, 0)`, id, name)
	if err != nil {
		t.Fatal(err)
	}
}

func insertDeployment(t *testing.T, db *DB, id, modelID, node, ip string, port int) error {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO deployments (id, model_id, cluster_name, node_name, node_ip, host_port,
		   service_name, engine_key, state, state_since, created_at, updated_at)
		 VALUES (?, ?, 'cluster1', ?, ?, ?, 'svc', 'k', 'pending', 0, 0, 0)`,
		id, modelID, node, ip, port)
	return err
}

func TestSchemaDefaults(t *testing.T) {
	db := openTest(t)
	insertModel(t, db, "m_1", "qwen")
	var args, labels string
	var command sql.NullString
	var port, vgpus, auto int
	err := db.QueryRowContext(t.Context(),
		`SELECT engine_args, command, container_port, vgpus, auto, labels FROM models WHERE id = 'm_1'`).
		Scan(&args, &command, &port, &vgpus, &auto, &labels)
	if err != nil {
		t.Fatal(err)
	}
	if args != "[]" || command.Valid || port != 8000 || vgpus != 0 || auto != 1 || labels != "{}" {
		t.Errorf("defaults: args %q command %v port %d vgpus %d auto %d labels %q", args, command, port, vgpus, auto, labels)
	}
}

func TestSchemaConstraints(t *testing.T) {
	db := openTest(t)
	insertModel(t, db, "m_1", "qwen")
	insertModel(t, db, "m_2", "llama")

	if err := insertDeployment(t, db, "d_x", "m_missing", "node1", "10.0.0.1", 18000); err == nil {
		t.Error("deployment of a missing model accepted; foreign keys are off")
	}
	if err := insertDeployment(t, db, "d_1", "m_1", "node1", "10.0.0.1", 18000); err != nil {
		t.Fatal(err)
	}
	if err := insertDeployment(t, db, "d_2", "m_1", "node1", "10.0.0.1", 18001); err == nil {
		t.Error("second deployment of the same model on the same node accepted")
	}
	if err := insertDeployment(t, db, "d_3", "m_2", "node2", "10.0.0.1", 18000); err == nil {
		t.Error("two deployments on the same ip and host port accepted")
	}
	if err := insertDeployment(t, db, "d_4", "m_2", "node2", "10.0.0.2", 18000); err != nil {
		t.Errorf("same port on another ip: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM models WHERE id = 'm_1'`); err == nil {
		t.Error("model with a deployment deleted")
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO models (id, name, reference, engine_image, memory_mb, created_at, updated_at)
		VALUES ('m_3', 'qwen', 'r', 'i', 1, 0, 0)`); err == nil {
		t.Error("duplicate model name accepted")
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	boom := errors.New("boom")
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if err := PutSetting(ctx, tx, "k", 1); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want boom", err)
	}
	var v int
	ok, err := GetSetting(ctx, db, "k", &v)
	if err != nil || ok {
		t.Fatalf("setting survived rollback: ok=%v err=%v", ok, err)
	}
}

func TestTxRollsBackOnPanic(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		_ = db.Tx(ctx, func(tx *sql.Tx) error {
			_ = PutSetting(ctx, tx, "k", 1)
			panic("boom")
		})
	}()
	var v int
	if ok, _ := GetSetting(ctx, db, "k", &v); ok {
		t.Fatal("setting survived a panicking transaction")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	type sched struct {
		Strategy string `json:"strategy"`
	}
	if err := PutSetting(ctx, db, SettingScheduler, sched{Strategy: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := PutSetting(ctx, db, SettingScheduler, sched{Strategy: "b"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetSettingAs[sched](ctx, db, SettingScheduler)
	if err != nil || !ok || got.Strategy != "b" {
		t.Fatalf("GetSettingAs = %+v, %v, %v", got, ok, err)
	}
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingScheduler).Scan(&raw); err != nil || raw != `{"strategy":"b"}` {
		t.Fatalf("stored value %q, %v", raw, err)
	}

	if err := DeleteSetting(ctx, db, SettingScheduler); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := GetSettingAs[sched](ctx, db, SettingScheduler); ok || err != nil || got.Strategy != "" {
		t.Fatalf("after delete: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestConcurrentWriters(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			errs <- db.Tx(ctx, func(tx *sql.Tx) error {
				var cur int
				if _, err := GetSetting(ctx, tx, "counter", &cur); err != nil {
					return err
				}
				return PutSetting(ctx, tx, "counter", cur+1)
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	got, _, err := GetSettingAs[int](ctx, db, "counter")
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("counter = %d, want %d (lost update: transactions are not serialised)", got, n)
	}
}

func TestNewID(t *testing.T) {
	id := NewID(PrefixDeployment)
	if !strings.HasPrefix(id, "d_") || len(id) != 2+26 {
		t.Fatalf("id %q", id)
	}
	for _, r := range id[2:] {
		if !strings.ContainsRune(idAlphabet, r) {
			t.Fatalf("id %q contains %q outside the alphabet", id, r)
		}
	}
	base := time.UnixMilli(1_700_000_000_000)
	prev := NewIDAt("m", base)
	for i := 1; i < 500; i++ {
		next := NewIDAt("m", base.Add(time.Duration(i)*time.Millisecond))
		if next <= prev {
			t.Fatalf("id at +%dms (%s) does not sort after %s", i, next, prev)
		}
		prev = next
	}
	seen := make(map[string]bool)
	for range 5000 {
		id := NewIDAt("", base)
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestMillisRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond).UTC()
	if got := FromMillis(Millis(now)); !got.Equal(now) {
		t.Errorf("round trip = %v, want %v", got, now)
	}
	if v := NullMillis(nil); v.Valid {
		t.Error("NullMillis(nil) is valid")
	}
	if got := FromNullMillis(NullMillis(&now)); got == nil || !got.Equal(now) {
		t.Errorf("null round trip = %v, want %v", got, now)
	}
}
