package accounts

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ownLimitLookback bounds the fallback plans OwnLimit reads: a limit whose
// reset is further out than this is not trusted (the delivery hold's bound,
// decision I69).
const ownLimitLookback = 8 * 24 * time.Hour

// OwnLimit is the usage or rate limit the machine's own login for h (a
// harness whose login Pyre does not manage: agy) is at, from the newest
// own-login fallback plan on this node whose reset is still ahead
// (HandleLimitEvent, decision I71), nil when none. It travels in the
// node's harness inventory (HarnessInfo.Limit) so clients do not place new
// runs of h here until the reset (decision I72). A limit without a reset
// is not reported: nothing would say when it ends.
func (s *Service) OwnLimit(ctx context.Context, h contracts.Harness) *contracts.LimitEvent {
	if contracts.ManagedLogins(h) || h == "" {
		return nil
	}
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM accounts_fallback WHERE planned_at >= ? AND COALESCE(json_extract(body,'$.reason'),'') LIKE ?
		ORDER BY planned_at DESC LIMIT 50`, ft(now.Add(-ownLimitLookback)), OwnLoginLimitPrefix+": "+string(h)+" %")
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		var p contracts.FallbackPlan
		if rows.Scan(&body) != nil || json.Unmarshal([]byte(body), &p) != nil || !strings.HasPrefix(p.Reason, OwnLoginLimitPrefix) {
			continue
		}
		l := p.Limit
		if l == nil || l.ResetAt == nil || !l.ResetAt.After(now) || l.ResetAt.Sub(now) > ownLimitLookback {
			continue
		}
		if l.Kind != LimitUsage && l.Kind != LimitRate {
			continue
		}
		cp := *l
		t := l.ResetAt.UTC()
		cp.ResetAt = &t
		return &cp
	}
	return nil
}
