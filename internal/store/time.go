package store

import (
	"database/sql"
	"time"
)

// Millis converts t to the unix-millisecond integer stored in the schema.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// FromMillis converts a stored unix-millisecond value back to a UTC time.
func FromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// NullMillis converts an optional time for a nullable column: nil or the
// zero time become SQL NULL.
func NullMillis(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

// FromNullMillis is the inverse of NullMillis.
func FromNullMillis(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := FromMillis(v.Int64)
	return &t
}
