package sanitize

import (
	"regexp"
	"sync/atomic"
)

// Regular expressions used by the guard. Kept in one place so the streaming and
// non-streaming paths cannot diverge.
var (
	openThinkAtStartRe = mustRe(`(?i)^<thinking\s*>?`)
	closeThinkRe       = mustRe(`(?i)</thinking\s*>`)
)

func mustRe(expr string) *regexp.Regexp {
	return regexp.MustCompile(expr)
}

func indexCloseThink(s string) int {
	loc := closeThinkRe.FindStringIndex(s)
	if loc == nil {
		return -1
	}
	return loc[0]
}

// Counters for the /api sanitize summary and for log lines.
var (
	repairedCount atomic.Int64
	rejectedCount atomic.Int64
)

// Stats is a snapshot of the sanitizer counters.
type Stats struct {
	Repaired int64 `json:"repaired"`
	Rejected int64 `json:"rejected"`
}

// Snapshot returns the current counters.
func Snapshot() Stats {
	return Stats{Repaired: repairedCount.Load(), Rejected: rejectedCount.Load()}
}
