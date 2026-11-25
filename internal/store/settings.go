package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Setting keys. Every key lives here so two packages can never pick the same
// name for different things. Values are JSON; the comment names the Go type.
const (
	// SettingScheduler ({strategy, config}) selects the routing strategy.
	// Seeded from OOA_SCHEDULER when unset.
	SettingScheduler = "scheduler"
	// SettingOakestraAppID (string) caches the Oakestra application id of
	// OOA_APP_NAME so restarts do not need to look it up by name.
	SettingOakestraAppID = "oakestra_app_id"
)

// GetSetting decodes the JSON value stored under key into dst. It reports
// false, with a nil error, when the key is not set.
func GetSetting(ctx context.Context, q Querier, key string, dst any) (bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: get setting %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return false, fmt.Errorf("store: decode setting %q: %w", key, err)
	}
	return true, nil
}

// GetSettingAs is GetSetting for callers that prefer a typed result. It
// returns the zero T and false when the key is not set.
func GetSettingAs[T any](ctx context.Context, q Querier, key string) (value T, ok bool, err error) {
	var v T
	ok, err = GetSetting(ctx, q, key, &v)
	if err != nil || !ok {
		return value, false, err
	}
	return v, true, nil
}

// PutSetting stores v as JSON under key, replacing any previous value.
func PutSetting(ctx context.Context, q Querier, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode setting %q: %w", key, err)
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, string(raw))
	if err != nil {
		return fmt.Errorf("store: put setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes key. Deleting a missing key is not an error.
func DeleteSetting(ctx context.Context, q Querier, key string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	return nil
}
