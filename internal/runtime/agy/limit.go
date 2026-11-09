package agy

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// agy reports a quota failure only in the Stop hook (the transcript gets no
// step for it) and on screen (agy 1.4.7, server, 2026-10-25):
//
//	{"event":"Stop","termination":"ERROR","fully_idle":true,"error":"generating and executing:
//	 RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription
//	 to increase your limits. Resets in 13h19m46s."}
//	⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 13h20m52s.
//
// Every turn on that login fails at once until the reset, so the run
// reports it as a usage limit (Observation.LimitEvent): the parent reads
// "stopped on a usage limit", not "finished", and Pyre holds deliveries
// until the reset.
var (
	limitQuotaRe = regexp.MustCompile(`(?i)(quota (?:reached|exceeded|exhausted)|exceeded your current quota|out of quota)`)
	limitRateRe  = regexp.MustCompile(`(?i)(RESOURCE_EXHAUSTED|code 429|rate limit|too many requests)`)
	limitResetRe = regexp.MustCompile(`(?i)resets in\s+((?:\d+\s*d\s*)?(?:\d+\s*h\s*)?(?:\d+\s*m\s*)?(?:\d+\s*s)?)`)
	limitPartRe  = regexp.MustCompile(`(\d+)\s*([dhms])`)
)

// stopLimit is the limit a Stop hook names (nil: none).
func stopLimit(r hookRecord) *contracts.LimitEvent {
	if r.Event != "Stop" || r.Termination != "ERROR" || r.Error == "" {
		return nil
	}
	kind := ""
	switch {
	case limitQuotaRe.MatchString(r.Error):
		kind = "usage_limit"
	case limitRateRe.MatchString(r.Error):
		kind = "rate_limited"
	default:
		return nil
	}
	at := r.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	le := &contracts.LimitEvent{Kind: kind, Detail: limitDetail(r.Error), At: at}
	if d, ok := resetIn(r.Error); ok {
		t := at.Add(d).UTC()
		le.ResetAt = &t
	}
	return le
}

// resetIn reads "Resets in 13h19m46s" (days allowed: "1d2h").
func resetIn(s string) (time.Duration, bool) {
	m := limitResetRe.FindStringSubmatch(s)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return 0, false
	}
	var d time.Duration
	for _, p := range limitPartRe.FindAllStringSubmatch(m[1], -1) {
		n, err := strconv.Atoi(p[1])
		if err != nil {
			return 0, false
		}
		unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second}[strings.ToLower(p[2])]
		d += time.Duration(n) * unit
	}
	return d, d > 0
}

// limitDetail is the provider's own sentence, without agy's error chain
// ("generating and executing: RESOURCE_EXHAUSTED (code 429): ").
func limitDetail(s string) string {
	if i := strings.LastIndex(s, "): "); i >= 0 {
		s = s[i+3:]
	} else if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return clip(oneLine(s), 200)
}
