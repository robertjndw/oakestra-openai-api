// Package auth holds the two credentials the addon checks: API keys for the
// public /v1 gateway, stored as hashes in SQLite, and dashboard tokens for
// the admin API, which are validated by asking the Oakestra system manager.
package auth
