package store

import (
	"crypto/rand"
	"strings"
	"time"
)

// ID prefixes in use. They only help humans reading logs and URLs; code must
// never parse an id or branch on its prefix.
const (
	PrefixModel      = "m"
	PrefixDeployment = "d"
	PrefixAPIKey     = "k"
)

const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// NewID returns "<prefix>_<26 chars>": a ULID-style 48-bit millisecond
// timestamp followed by 80 random bits, in lowercase Crockford base32.
// Later ids sort later as plain strings, which keeps SQLite indexes
// append-mostly. Deployment ids also become CSI volume ids, and the
// lowercase alphabet with a short prefix never produces the "pvc-" prefix
// that CSI drivers treat as a dynamically provisioned volume.
func NewID(prefix string) string {
	return NewIDAt(prefix, time.Now())
}

// NewIDAt is NewID with an explicit timestamp, for deterministic tests.
func NewIDAt(prefix string, t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	// crypto/rand.Read never returns an error since Go 1.24; it crashes the
	// program instead when the OS source fails.
	_, _ = rand.Read(b[6:])

	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + 26)
	if prefix != "" {
		sb.WriteString(prefix)
		sb.WriteByte('_')
	}
	sb.WriteString(encodeID(b))
	return sb.String()
}

// encodeID writes 128 bits as 26 base32 characters. The first character
// only carries 3 bits, as in the ULID spec.
func encodeID(b [16]byte) string {
	var out [26]byte
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = idAlphabet[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
