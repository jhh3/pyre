package hub

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Held (batched) answers (W-web-inbox, contracts/webinbox.go): the owner
// may hold an answer instead of sending it. Held answers live in ask_held,
// one row per ask (a new hold replaces the old one and restarts the quiet
// window). Maintenance (resolveAsks) sends the whole queue once
// contracts.AskBatchQuiet has passed since the newest hold, and sends a
// held answer early when its ask reaches its deadline, so the owner's
// choice wins over the default. Sending goes through answerAsks, the
// AnswerAsks path: one combined decision message per asking session.
//
// A decision of any kind (answer, deadline, withdrawal) deletes the ask's
// held row in the same transaction (decideAskTx -> dropHeldTx), so a held
// answer never outlives its ask and a flush that loses a race to another
// decision just reports the conflict.

var askHeldMigrations = []sqlitex.Migration{{Namespace: "hub_ask_held", Version: 1, Name: "ask_held", SQL: `
CREATE TABLE ask_held (
	ask_id TEXT PRIMARY KEY,
	option_id TEXT NOT NULL DEFAULT '',
	text TEXT NOT NULL DEFAULT '',
	held_by TEXT NOT NULL, -- principal JSON: the answer is sent as this owner principal
	held_at TEXT NOT NULL
);
CREATE INDEX ask_held_at ON ask_held(held_at);
`}}

var _ contracts.HeldAnswersAPI = (*Hub)(nil)

func requireOwner(ctx context.Context, what string) (contracts.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if !isOwner(p) {
		return p, contracts.Errorf(contracts.ErrForbidden, "only the owner %s", what)
	}
	return p, nil
}

// heldTx reads the queue, oldest hold first.
func (h *Hub) heldTx(ctx context.Context, q queryer) (contracts.HeldAnswers, error) {
	out := contracts.HeldAnswers{Items: []contracts.HeldAnswer{}}
	rows, err := q.QueryContext(ctx, `SELECT ask_id, option_id, items, text, held_at FROM ask_held ORDER BY held_at, ask_id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var newest time.Time
	for rows.Next() {
		var it contracts.HeldAnswer
		var at, items string
		if err := rows.Scan(&it.AskID, &it.OptionID, &items, &it.Text, &at); err != nil {
			return out, err
		}
		if items != "" {
			_ = persistence.FromJSON(items, &it.Items)
		}
		it.HeldAt = persistence.ParseTime(at)
		if it.HeldAt.After(newest) {
			newest = it.HeldAt
		}
		out.Items = append(out.Items, it)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > 0 {
		at := newest.Add(contracts.AskBatchQuiet)
		out.SendsAt = &at
	}
	return out, nil
}

func (h *Hub) emitHeldTx(ctx context.Context, tx *sql.Tx, askID string) (contracts.HeldAnswers, error) {
	held, err := h.heldTx(ctx, tx)
	if err != nil {
		return held, err
	}
	return held, h.emitPayload(ctx, tx, contracts.EvAskHeldChanged, contracts.ObjectRef{Kind: contracts.KindAsk, ID: askID}, "", nil, "hub", 0, held)
}

// dropHeldTx deletes a decided ask's held answer (and tells clients).
func (h *Hub) dropHeldTx(ctx context.Context, tx *sql.Tx, askID string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM ask_held WHERE ask_id=?`, askID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = h.emitHeldTx(ctx, tx, askID)
	}
	return err
}

// HoldAnswer implements asks.hold (owner only): validate the answer like
// AnswerAsks, then queue it, replacing an earlier held answer for the ask.
func (h *Hub) HoldAnswer(ctx context.Context, req contracts.HoldAnswerRequest) (contracts.HeldAnswers, error) {
	var out contracts.HeldAnswers
	p, err := requireOwner(ctx, "holds answers")
	if err != nil {
		return out, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpAsksHold, p, req, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getAsk(ctx, tx, strings.TrimSpace(req.AskID))
		if err != nil {
			return nil, err
		}
		if a.State.Decided() {
			return nil, contracts.Errorf(contracts.ErrConflict, "ask %s is already %s (%s)", a.AskID, a.State, askOutcome(a)).WithRef(contracts.KindAsk, a.AskID).WithDetail("state", string(a.State))
		}
		item, err := checkAnswer(a, req.AnswerAskItem)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ask_held(ask_id,option_id,items,text,held_by,held_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(ask_id) DO UPDATE SET option_id=excluded.option_id, items=excluded.items, text=excluded.text, held_by=excluded.held_by, held_at=excluded.held_at`,
			a.AskID, item.OptionID, heldItemsArg(item.Items), item.Text, persistence.JSON(p), persistence.Time(h.now())); err != nil {
			return nil, err
		}
		return h.emitHeldTx(ctx, tx, a.AskID)
	})
	return out, err
}

// UnholdAnswer implements asks.unhold (owner only). An ask with no held
// answer (already sent, dropped, or never held) is not_found.
func (h *Hub) UnholdAnswer(ctx context.Context, req contracts.UnholdAnswerRequest) (contracts.HeldAnswers, error) {
	var out contracts.HeldAnswers
	p, err := requireOwner(ctx, "takes back held answers")
	if err != nil {
		return out, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpAsksUnhold, p, req, &out, func(tx *sql.Tx) (any, error) {
		id := strings.TrimSpace(req.AskID)
		res, err := tx.ExecContext(ctx, `DELETE FROM ask_held WHERE ask_id=?`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, contracts.Errorf(contracts.ErrNotFound, "no held answer for %s: it was already sent, or the ask was decided another way", id).WithRef(contracts.KindAsk, id)
		}
		return h.emitHeldTx(ctx, tx, id)
	})
	return out, err
}

// ListHeldAnswers implements asks.held (owner only).
func (h *Hub) ListHeldAnswers(ctx context.Context, _ struct{}) (contracts.HeldAnswers, error) {
	if _, err := requireOwner(ctx, "sees held answers"); err != nil {
		return contracts.HeldAnswers{}, err
	}
	return h.heldTx(ctx, h.db)
}

// FlushHeldAnswers implements asks.flush (owner only): send the whole
// queue now. Each answer is sent as the owner principal that held it.
func (h *Hub) FlushHeldAnswers(ctx context.Context, req contracts.FlushHeldAnswersRequest) (contracts.AnswerAsksResponse, error) {
	if _, err := requireOwner(ctx, "sends held answers"); err != nil {
		return contracts.AnswerAsksResponse{Results: []contracts.AnswerAskResult{}}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.AnswerAsksResponse{Results: []contracts.AnswerAskResult{}}, err
	}
	return h.flushHeld(ctx, nil)
}

type heldRow struct {
	item   contracts.AnswerAskItem
	by     contracts.Principal
	heldAt string
}

// flushHeld sends held answers (all of them, or those for only), grouped
// by the principal that held them. Each item's operation ID is derived
// from the ask and the hold time, so a retry after a crash replays rather
// than answers twice. A row whose answer failed for a definitive reason
// (already decided, invalid) is deleted; one that failed transiently stays
// for the next pass.
func (h *Hub) flushHeld(ctx context.Context, only map[string]bool) (contracts.AnswerAsksResponse, error) {
	h.heldMu.Lock() // one flush at a time (maintenance and FlushHeldAnswers)
	defer h.heldMu.Unlock()
	out := contracts.AnswerAsksResponse{Results: []contracts.AnswerAskResult{}}
	rows, err := h.db.QueryContext(ctx, `SELECT ask_id, option_id, items, text, held_by, held_at FROM ask_held ORDER BY held_at, ask_id`)
	if err != nil {
		return out, err
	}
	var all []heldRow
	for rows.Next() {
		var r heldRow
		var by, items string
		if err := rows.Scan(&r.item.AskID, &r.item.OptionID, &items, &r.item.Text, &by, &r.heldAt); err != nil {
			rows.Close()
			return out, err
		}
		if items != "" {
			_ = persistence.FromJSON(items, &r.item.Items)
		}
		if only != nil && !only[r.item.AskID] {
			continue
		}
		_ = persistence.FromJSON(by, &r.by)
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	var order []string
	groups := map[string][]heldRow{}
	for _, r := range all {
		k := string(r.by.Kind) + ":" + r.by.ID
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	for _, k := range order {
		g := groups[k]
		items := make([]contracts.AnswerAskItem, len(g))
		for i, r := range g {
			items[i] = r.item
		}
		res := h.answerAsks(ctx, g[0].by, items, func(i int) string {
			return "op_askheld-" + g[i].item.AskID + "-" + strconv.FormatInt(persistence.ParseTime(g[i].heldAt).UnixNano(), 36)
		})
		for i, r := range res.Results {
			if r.Error != nil && !retryableSend(r.Error) {
				// Decided some other way meanwhile, or no longer valid.
				_ = h.tx(ctx, func(tx *sql.Tx) error {
					res, err := tx.ExecContext(ctx, `DELETE FROM ask_held WHERE ask_id=? AND held_at=?`, g[i].item.AskID, g[i].heldAt)
					if err != nil {
						return err
					}
					if n, _ := res.RowsAffected(); n > 0 {
						_, err = h.emitHeldTx(ctx, tx, g[i].item.AskID)
					}
					return err
				})
			}
		}
		out.Results = append(out.Results, res.Results...)
	}
	return out, nil
}

// sendHeldIfQuiet sends the queue once AskBatchQuiet has passed since the
// newest hold (maintenance).
func (h *Hub) sendHeldIfQuiet(ctx context.Context) error {
	var newest sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT MAX(held_at) FROM ask_held`).Scan(&newest); err != nil {
		return err
	}
	if !newest.Valid || newest.String == "" {
		return nil
	}
	if h.now().Before(persistence.ParseTime(newest.String).Add(contracts.AskBatchQuiet)) {
		return nil
	}
	_, err := h.flushHeld(ctx, nil)
	return err
}

// sendHeldForDue sends the held answers of asks whose deadline has passed,
// before the default is applied.
func (h *Hub) sendHeldForDue(ctx context.Context, due []contracts.Ask) error {
	if len(due) == 0 {
		return nil
	}
	args := make([]any, len(due))
	for i, a := range due {
		args[i] = a.AskID
	}
	rows, err := h.db.QueryContext(ctx, `SELECT ask_id FROM ask_held WHERE ask_id IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...)
	if err != nil {
		return err
	}
	only := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			only[id] = true
		}
	}
	rows.Close()
	if len(only) == 0 {
		return rows.Err()
	}
	_, err = h.flushHeld(ctx, only)
	return err
}
