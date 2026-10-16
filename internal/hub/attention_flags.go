package hub

import (
	"context"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/attention/sessionflag"
	"github.com/jhh3/pyre/internal/contracts"
)

// Session attention flags (v1.3, W-attn): the owner engaging with a session
// answers its messages to the owner and its own "needs you" flag, and
// attention titles name sessions by title instead of ID at render time.

// engageKinds are answered by the owner typing into or messaging a
// session; readKinds by the owner viewing its message. Both answer its
// "waiting on you" (waiting.go). Approvals and questions resolve only
// through their own answer.
var (
	engageKinds = []contracts.AttentionKind{contracts.AttnMessage, contracts.AttnFlag, contracts.AttnWaiting}
	readKinds   = []contracts.AttentionKind{contracts.AttnMessage, contracts.AttnWaiting}
)

// ownerEngager is implemented by the attention store.
type ownerEngager interface {
	OwnerEngaged(ctx context.Context, sessionID, summary string, kinds ...contracts.AttentionKind) (int, error)
}

// engageState de-duplicates in-flight engagement updates per session so
// fast typing never queues more than one.
type engageState struct {
	mu       sync.Mutex
	inflight map[string]bool
}

// engageTimeout bounds one background engagement update.
const engageTimeout = 15 * time.Second

// ownerEngaged records, off the caller's path, that the owner engaged with
// a session. Only a direct owner principal counts: a session acting on the
// owner's behalf is not the owner looking at it.
func (h *Hub) ownerEngaged(p contracts.Principal, sessionID, summary string, kinds ...contracts.AttentionKind) {
	if p.Kind != contracts.PrincipalOwner || sessionID == "" || h.cfg.Attention == nil {
		return
	}
	e, ok := h.cfg.Attention.(ownerEngager)
	if !ok {
		return
	}
	key := sessionID + "|" + summary
	h.engage.mu.Lock()
	if h.engage.inflight[key] {
		h.engage.mu.Unlock()
		return
	}
	if h.engage.inflight == nil {
		h.engage.inflight = map[string]bool{}
	}
	h.engage.inflight[key] = true
	h.engage.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			h.engage.mu.Lock()
			delete(h.engage.inflight, key)
			h.engage.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), engageTimeout)
		defer cancel()
		go func() {
			select {
			case <-h.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, _ = e.OwnerEngaged(h.systemCtx(ctx), sessionID, summary, kinds...)
	}()
}

// runSession returns the session of a run ("" when unknown).
func (h *Hub) runSession(ctx context.Context, runID string) string {
	run, err := h.getRun(ctx, h.db, runID)
	if err != nil {
		return ""
	}
	return run.SessionID
}

// resolveAttentionTitles names sessions by their current title in item
// titles (render time, so renames apply; Android and the CLI both read
// these). Unknown sessions keep the stored title.
func (h *Hub) resolveAttentionTitles(ctx context.Context, items []contracts.AttentionItem) {
	titles := map[string]string{}
	for i := range items {
		sid := items[i].SessionID
		if sid == "" {
			continue
		}
		t, ok := titles[sid]
		if !ok {
			if s, err := h.getSession(ctx, h.db, sid); err == nil {
				t = s.Title
			}
			titles[sid] = t
		}
		items[i].Title = sessionflag.ResolveTitle(items[i].Title, sid, t)
	}
}
