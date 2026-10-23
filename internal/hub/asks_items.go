package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Batched asks and ask actions (W-grants, contracts/asks.go). A batched
// ask carries Items ("approve these N merges"): its options apply to each
// item and the owner may answer item by item (AskAnswer.Items; an item left
// out takes OptionID). An ask's Action names the go-ahead it wants; a
// standing grant held by the asker that matches it exactly
// (standing_grants.go: grantForAsk) answers the ask when it is created, so
// the owner is not asked again (he gets an FYI instead).

var askItemMigrations = []sqlitex.Migration{{Namespace: "hub_ask_items", Version: 1, Name: "ask_items_action_grant", SQL: `
ALTER TABLE asks ADD COLUMN action TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN items TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN grant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE ask_held ADD COLUMN items TEXT NOT NULL DEFAULT '';
`}, {Namespace: "hub_ask_items", Version: 2, Name: "ask_irreversible", SQL: `
ALTER TABLE asks ADD COLUMN irreversible INTEGER NOT NULL DEFAULT 0;
`}}

// askMaxItemURL bounds an item's URL.
const askMaxItemURL = 2000

// normalizeAskItems validates and trims the batch items and the action of
// a create request.
func normalizeAskItems(in *askInput) error {
	in.Action = NormalizeGrantAction(in.Action, contracts.AskMaxAction)
	if len(in.Items) == 0 {
		in.Items = nil
		return nil
	}
	if len(in.Items) > contracts.AskMaxItems {
		return contracts.Errorf(contracts.ErrInvalid, "a batched ask has at most %d items (got %d); split it", contracts.AskMaxItems, len(in.Items))
	}
	items := make([]contracts.AskItem, 0, len(in.Items))
	seen := map[string]bool{}
	for i, it := range in.Items {
		it.ID = strings.TrimSpace(it.ID)
		it.Label = strings.Join(strings.Fields(it.Label), " ")
		it.Detail = strings.Join(strings.Fields(it.Detail), " ")
		it.URL = strings.TrimSpace(it.URL)
		switch {
		case it.ID == "":
			return contracts.Errorf(contracts.ErrInvalid, "item %d has no id (a short stable word, e.g. \"pr-12\")", i+1)
		case len(it.ID) > askMaxOptionID || strings.ContainsFunc(it.ID, func(r rune) bool { return r == ' ' || r == ',' || r == '=' }):
			return contracts.Errorf(contracts.ErrInvalid, "item id %q must be a short word (≤%d bytes, no spaces, commas or '=')", it.ID, askMaxOptionID)
		case seen[strings.ToLower(it.ID)]:
			return contracts.Errorf(contracts.ErrInvalid, "item id %q is used twice", it.ID)
		case it.Label == "":
			return contracts.Errorf(contracts.ErrInvalid, "item %q has no label", it.ID)
		case utf8.RuneCountInString(it.Label) > contracts.AskMaxOptionText || utf8.RuneCountInString(it.Detail) > contracts.AskMaxOptionText:
			return contracts.Errorf(contracts.ErrInvalid, "item %q label and detail are limited to %d characters each", it.ID, contracts.AskMaxOptionText)
		case len(it.URL) > askMaxItemURL:
			return contracts.Errorf(contracts.ErrInvalid, "item %q url is limited to %d bytes", it.ID, askMaxItemURL)
		}
		seen[strings.ToLower(it.ID)] = true
		items = append(items, it)
	}
	in.Items = items
	return nil
}

// askItem finds an item by ID (case-insensitive).
func askItem(a contracts.Ask, id string) (contracts.AskItem, bool) {
	for _, it := range a.Items {
		if strings.EqualFold(it.ID, id) {
			return it, true
		}
	}
	return contracts.AskItem{}, false
}

// checkAnswerItems validates per-item answers against a batched ask and
// returns them keyed by the canonical item and option IDs. Items left out
// take optionID; without one every item must be answered.
func checkAnswerItems(a contracts.Ask, items map[string]string, optionID string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(a.Items) == 0 {
		return nil, contracts.Errorf(contracts.ErrInvalid, "%s is not a batched ask: answer it with one option", a.AskID).WithRef(contracts.KindAsk, a.AskID)
	}
	out := make(map[string]string, len(items))
	for id, opt := range items {
		it, ok := askItem(a, strings.TrimSpace(id))
		if !ok {
			return nil, contracts.Errorf(contracts.ErrInvalid, "%q is not an item of %s", id, a.AskID).WithRef(contracts.KindAsk, a.AskID)
		}
		o, ok := a.Option(strings.TrimSpace(opt))
		if !ok {
			return nil, contracts.Errorf(contracts.ErrInvalid, "%q (for item %s) is not an option of %s", opt, it.ID, a.AskID).WithRef(contracts.KindAsk, a.AskID)
		}
		out[it.ID] = o.ID
	}
	if optionID == "" {
		var missing []string
		for _, it := range a.Items {
			if _, ok := out[it.ID]; !ok {
				missing = append(missing, it.ID)
			}
		}
		if len(missing) > 0 {
			return nil, contracts.Errorf(contracts.ErrInvalid, "answer every item or give an option for the rest (unanswered: %s)", strings.Join(missing, ", ")).WithRef(contracts.KindAsk, a.AskID)
		}
	}
	return out, nil
}

// askItemOption is the option chosen for one item of a decided ask.
func askItemOption(a contracts.Ask, itemID string) string {
	if a.Answer == nil {
		return ""
	}
	if o, ok := a.Answer.Items[itemID]; ok {
		return o
	}
	return a.Answer.OptionID
}

// errGrantTopicBlocked rolls back a grant answer whose topic is open or
// was decided recently (the ask then takes the ordinary path).
var errGrantTopicBlocked = errors.New("grant: topic open or recently decided")

// askTopicBlockedTx reports whether a topic in scope has an open ask or one
// decided in the last askDecidedWindow other than by a grant: never
// grant-answered (the owner's decision, or the open question, stands). An
// earlier grant answer does not block: the grant is re-checked live.
func (h *Hub) askTopicBlockedTx(ctx context.Context, tx *sql.Tx, scope, topic string, now time.Time) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks WHERE scope=? AND topic_key=? AND (state=? OR (decided_at>=? AND grant_id=''))`,
		scope, topic, string(contracts.AskOpen), persistence.Time(now.Add(-askDecidedWindow))).Scan(&n)
	return n > 0, err
}

// grantAsk answers a session's ask with a standing grant it holds (exact
// match, grantForAsk): stored already answered (by the grants principal,
// with the grant's option), no owner message; the asker gets the decision
// at once and the owner an FYI (the deriver, from the ask event). ok is
// false when no grant answers it, including when its topic is open or was
// decided recently (checked first, in the transaction).
func (h *Hub) grantAsk(ctx context.Context, p contracts.Principal, asker contracts.Session, req contracts.CreateAskRequest, in askInput) (contracts.CreateAskResponse, bool, error) {
	var out contracts.CreateAskResponse
	g, opt, ok, err := h.grantForAsk(ctx, h.db, asker, in)
	if err != nil || !ok {
		return out, false, err
	}
	now := h.now()
	project := asker.ProjectID
	err = h.mutate(ctx, in.OperationID, contracts.OpAsksCreate, p, req, &out, func(tx *sql.Tx) (any, error) {
		blocked, err := h.askTopicBlockedTx(ctx, tx, askScope(project), in.TopicKey, now)
		if err != nil {
			return nil, err
		}
		if blocked {
			return nil, errGrantTopicBlocked
		}
		return h.grantAskTx(ctx, tx, asker.SessionID, project, in, g, opt, now)
	})
	if errors.Is(err, errGrantTopicBlocked) {
		return out, false, nil
	}
	if err == nil && out.Ask != nil && out.Ask.State.Decided() {
		if sent := h.sendDecisions(ctx, []contracts.Ask{*out.Ask}, grantSystem); sent[out.Ask.AskID] != "" {
			out.Ask.DecisionMessageID = sent[out.Ask.AskID]
		}
	}
	return out, true, err
}

func (h *Hub) grantAskTx(ctx context.Context, tx *sql.Tx, sessionID string, project *string, in askInput, g contracts.StandingGrant, opt string, now time.Time) (contracts.CreateAskResponse, error) {
	// Revoked or expired since it was read: the ask takes the ordinary path.
	res, err := tx.ExecContext(ctx, `UPDATE standing_grants SET used_count=used_count+1, last_used_at=? WHERE grant_id=? AND revoked_at IS NULL AND expires_at>?`,
		persistence.Time(now), g.GrantID, persistence.Time(now))
	if err != nil {
		return contracts.CreateAskResponse{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return contracts.CreateAskResponse{}, errGrantTopicBlocked
	}
	a := contracts.Ask{
		AskID: contracts.NewID(contracts.PrefixAsk), TopicKey: in.TopicKey, ProjectID: project, SessionID: sessionID,
		Title: in.Title, Context: in.Context, Options: in.Options, DefaultOption: in.DefaultOption, Urgent: in.Urgent,
		Items: in.Items, Action: in.Action, GrantID: g.GrantID, State: contracts.AskAnswered, Count: 1, CreatedAt: now, UpdatedAt: now,
		Answer: &contracts.AskAnswer{OptionID: opt, Text: grantText(g), By: grantSystem, At: now},
	}
	a.ThreadID = a.AskID
	if _, err := tx.ExecContext(ctx, `INSERT INTO asks(ask_id,topic_key,scope,project_id,session_id,title,context,options,default_option,urgent,deadline_rule,state,answer,count,thread_id,norm_text,action,items,grant_id,decision_pending,decided_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'',?,?,1,?,?,?,?,?,1,?,?,?)`,
		a.AskID, a.TopicKey, askScope(a.ProjectID), persistence.ProjectArg(a.ProjectID), a.SessionID, a.Title, a.Context, persistence.JSON(a.Options), a.DefaultOption,
		boolInt(a.Urgent), string(a.State), persistence.JSON(a.Answer), a.ThreadID, askNormText(a.Title, a.Options), a.Action, askItemsArg(a.Items), a.GrantID,
		persistence.Time(now), persistence.Time(now), persistence.Time(now)); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asks_fts(title,context,options,ask_id) VALUES(?,?,?,?)`, a.Title, a.Context, askOptionsText(a.Options), a.AskID); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	if err := h.emitAsk(ctx, tx, a); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	return contracts.CreateAskResponse{Status: contracts.AskGranted, Ask: &a,
		Detail: fmt.Sprintf("answered at once by %s; the owner was not asked (he gets an FYI). Go ahead: %s. The decision also arrives as a Pyre message (ask %s)",
			grantText(g), askOptionLabel(a, opt), a.AskID)}, nil
}

// askItemsArg is the stored form of an ask's items ("" when none).
func askItemsArg(items []contracts.AskItem) string {
	if len(items) == 0 {
		return ""
	}
	return persistence.JSON(items)
}

// heldItemsArg is the stored form of a held answer's per-item choices.
func heldItemsArg(items map[string]string) string {
	if len(items) == 0 {
		return ""
	}
	return persistence.JSON(items)
}
