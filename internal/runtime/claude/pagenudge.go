package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/contracts"
)

// Page nudge (W-web-inbox): a project's lead keeps the project page (the
// owner's main view of the project in the web app) current. When a session
// in a project did work this turn and has not updated the page itself
// (update_project_page, `pyre project page set`) or been reminded in
// pageStaleAfter, the Stop hook blocks the stop once with PageNudgeText,
// provided the hub says it leads the project now. "Updated" is this
// session's own evidence in events.jsonl: a lead whose page another session
// keeps current is reminded at most every pageStaleAfter and told to check
// first. Never when the owner typed this turn's prompt (he is in the chat),
// never in a turn a Stop hook already continued (stop_hook_active: at most
// one nudge per turn, the ask nudge first), never for a session that does
// not lead its project.
//
// Who leads is the hub's live answer (LaunchContext.Role, the rule the page
// write itself checks), not the launch-time $PYRE_PROJECT_LEAD: a parent
// that joins the project after starting this session leads it from then on
// (iss_…). The answer is cached in the run's state
// dir for pageRoleTTL; when the hub cannot answer (no socket or token, the
// node's daemon unreachable, a node-cache copy, an older hub), there is no
// nudge.
//
// The page, not the session, has to be stale, and only one lead is asked
// (I63, iss_…): a coordinator outside a project
// that starts a build lead and a bounded helper in it makes both leads.
// The hub's role names the page owner among the live leads
// (ProjectRole.PageOwner) and the page's last write by anyone
// (PageUpdatedAt); only the page owner is nudged, and only when nobody
// wrote the page or its status card in pageStaleAfter. A hub before I63
// sends neither: a lead then counts as the page owner and only its own
// evidence counts, as before.

// PageNudgeText is the Stop-hook block reason. Its opening words are
// matched by the hub (hub/asks_chat.go) to tell it from the owner typing.
const PageNudgeText = "You lead this project and did work this turn, but its page (the owner's main view " +
	"of the project in the web app) has not been updated in 3 hours. Check it with mcp__pyre__get_project_page; if it is missing or stale, " +
	"rewrite it with mcp__pyre__update_project_page (goal, current status, results with links, open decisions, what's next) and set its status card " +
	"(status: progress, phase, in_flight, ready_for_owner, shipped, blocked). " +
	"If it is current, just end your turn."

// evPageNudged records that the Stop hook blocked a stop with PageNudgeText.
const evPageNudged = "PyreStopPageNudge"

// envProjectID is the session's project at launch (set by pyred).
const envProjectID = "PYRE_PROJECT_ID"

// pageStaleAfter is how long after the session's last page update (or
// reminder) it is reminded again.
const pageStaleAfter = 3 * time.Hour

func pageNudge(stateDir string, rec hookRecord, p map[string]any, now time.Time) []byte {
	if !inProject() || p["stop_hook_active"] == true {
		return nil
	}
	recs, first, err := tailEvents(stateDir)
	if err != nil {
		return nil
	}
	start := -1
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Event == "UserPromptSubmit" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil // no turn start in view
	}
	worked := false
	for _, r := range recs[start+1:] {
		switch {
		case r.Event == evAskNudged || r.Event == evPageNudged || r.Event == evStopInjected:
			return nil // one nudge per turn
		case r.Event == "PreToolUse" && r.ToolName != "" && !strings.HasPrefix(r.ToolName, "mcp__pyre__"):
			worked = true
		}
	}
	if !worked {
		return nil
	}
	for _, r := range recs {
		if (r.Event == evPageNudged || updatedPage(r)) && now.Sub(r.At) < pageStaleAfter {
			return nil
		}
	}
	if ownerTyped(stateDir, recs, start, first, now) {
		return nil
	}
	if !ownsStalePage(stateDir, now) {
		return nil
	}
	events := filepath.Join(stateDir, fileEvents)
	_ = appendJSONL(events, rec)
	_ = appendJSONL(events, hookRecord{ID: newEventID(), At: now.UTC(), Event: evPageNudged})
	out, _ := json.Marshal(map[string]any{"decision": "block", "reason": PageNudgeText})
	return out
}

// inProject: this session was launched in a project (one moved into a
// project later is reminded from its next launch). Whether it keeps the
// project's page is ownsStalePage's question.
func inProject() bool {
	return os.Getenv(envProjectID) != ""
}

// filePageRole caches the hub's answer to "does this session keep its
// project's page, and when was the page last written" (pageRoleCache) in
// the run's state dir.
const filePageRole = "page-role.json"

// pageRoleTTL is how long a hub answer is reused; pageRoleRetry how long
// "the hub could not answer" is, so Stops do not each wait on a down hub.
// An answer that would nudge is reused only for pageRoleRetry: another
// lead may have written the page since.
const (
	pageRoleTTL   = 10 * time.Minute
	pageRoleRetry = time.Minute
)

// pageRoleTimeout bounds the hub call inside the Stop hook (15 s budget).
const pageRoleTimeout = 3 * time.Second

type pageRoleCache struct {
	At    time.Time `json:"at"`
	Known bool      `json:"known"` // the hub answered
	Lead  bool      `json:"lead"`
	// PageOwner: this session keeps the page among the project's leads
	// (equal to Lead from a hub before I63). Read as Lead when absent (a
	// cache written before I63).
	PageOwner *bool `json:"page_owner,omitempty"`
	// PageUpdatedAt is the page's last write by anyone (nil: never, or a
	// hub before I63).
	PageUpdatedAt *time.Time `json:"page_updated_at,omitempty"`
}

// nudges reports whether c, the hub's answer, says to remind this session
// at now: it keeps the page and nobody wrote it in pageStaleAfter.
func (c pageRoleCache) nudges(now time.Time) bool {
	owner := c.Lead
	if c.PageOwner != nil {
		owner = c.Lead && *c.PageOwner
	}
	return c.Known && owner && (c.PageUpdatedAt == nil || now.Sub(*c.PageUpdatedAt) >= pageStaleAfter)
}

// ownsStalePage reports whether the hub says this session keeps its
// project's page now and the page is stale, from the cache when fresh.
// Unknown reads as no.
func ownsStalePage(stateDir string, now time.Time) bool {
	path := filepath.Join(stateDir, filePageRole)
	if b, err := os.ReadFile(path); err == nil {
		var c pageRoleCache
		if json.Unmarshal(b, &c) == nil {
			ttl := pageRoleRetry
			if c.Known && !c.nudges(now) {
				ttl = pageRoleTTL
			}
			if age := now.Sub(c.At); age >= 0 && age < ttl {
				return c.nudges(now)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), pageRoleTimeout)
	defer cancel()
	c := pageRoleCache{At: now.UTC()}
	if role, err := hubProjectRole(ctx); err == nil && role != nil {
		c.Known, c.Lead, c.PageUpdatedAt = true, role.Lead, role.PageUpdatedAt
		if role.PageOwnerSessionID != "" { // a hub before I63 names no page owner
			owner := role.PageOwner
			c.PageOwner = &owner
		}
	}
	if b, err := json.Marshal(c); err == nil {
		_ = writeFileAtomic(path, b)
	}
	return c.nudges(now)
}

// hubProjectRole asks the hub for this session's live role in its project
// (get_context's Role) through the run's own node daemon socket and session
// token, never a client config or another credential. A nil role means the
// hub does not report roles (older hub); a copy the node served from its
// cache because the hub is down is no answer. Tests replace it.
var hubProjectRole = func(ctx context.Context) (*contracts.ProjectRole, error) {
	sock, tok := os.Getenv(clientconn.EnvSocket), os.Getenv(clientconn.EnvSessionToken)
	if sock == "" || tok == "" {
		return nil, errors.New("no node socket or session token")
	}
	cl := client.New("http://pyred.local", tok, client.Options{HTTPClient: clientconn.UnixSocketHTTPClient(sock),
		Timeout: pageRoleTimeout, MaxAttempts: 1, UserAgent: "pyred-hook/" + buildinfo.Version})
	cached := false
	ctx = client.WithFreshnessSink(ctx, func(string, contracts.Freshness) { cached = true })
	lc, err := cl.GetLaunchContext(ctx, contracts.ContextRequest{})
	if err != nil {
		return nil, err
	}
	if cached {
		return nil, errors.New("served from the node cache: the hub is unreachable")
	}
	return lc.Role, nil
}

// updatedPage reports a tool call in r that wrote the project page.
func updatedPage(r hookRecord) bool {
	if r.Event != "PostToolUse" {
		return false
	}
	if r.ToolName == "mcp__pyre__update_project_page" {
		return true
	}
	if r.ToolName != "Bash" {
		return false
	}
	ti, _ := r.payload()["tool_input"].(map[string]any)
	cmd := " " + strings.Join(strings.Fields(str(ti, "command")), " ") + " "
	return strings.Contains(cmd, " project page set ") || strings.Contains(cmd, " projects page set ")
}
