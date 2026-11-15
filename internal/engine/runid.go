package engine

import (
	"crypto/rand"
	"math/big"
	"strconv"
	"time"
)

const (
	runIDPrefix    = "vern"
	runIDSuffixLen = 8
	runIDAlphabet  = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// RunID identifies one scoring run. It is lowercase [a-z0-9] only so it can
// be used as an Elasticsearch data stream namespace, and it encodes the
// creation time so stale runs can be purged.
type RunID string

// NewRunID returns "vern" + base36(unix seconds) + 8 random [a-z0-9] chars.
func NewRunID(now time.Time) RunID {
	suffix := make([]byte, runIDSuffixLen)
	max := big.NewInt(int64(len(runIDAlphabet)))
	for i := range suffix {
		n, err := rand.Int(rand.Reader, max) // uniform, no modulo bias
		if err != nil {
			panic("engine: crypto/rand failed: " + err.Error())
		}
		suffix[i] = runIDAlphabet[n.Int64()]
	}
	return RunID(runIDPrefix + strconv.FormatInt(now.Unix(), 36) + string(suffix))
}

// ParseRunID validates s and returns it as a RunID.
func ParseRunID(s string) (RunID, bool) {
	if _, ok := parseRunIDTime(s); !ok {
		return "", false
	}
	return RunID(s), true
}

// CreatedAt returns the creation time encoded in the ID (second precision),
// or the zero time if the ID is malformed.
func (id RunID) CreatedAt() time.Time {
	secs, ok := parseRunIDTime(string(id))
	if !ok {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func parseRunIDTime(s string) (int64, bool) {
	if len(s) <= len(runIDPrefix)+runIDSuffixLen || s[:len(runIDPrefix)] != runIDPrefix {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return 0, false
		}
	}
	secs, err := strconv.ParseInt(s[len(runIDPrefix):len(s)-runIDSuffixLen], 36, 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return secs, true
}
