package hub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/search"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Asks (W-asks): structured questions from top-level sessions to the owner
// (contracts/asks.go). One open ask per (scope, topic key); a repeat merges
// into it, a recently decided topic answers with the decision, and a
// similar open ask under another topic is offered before a new one is made.
//
// The ask row commits first; the owner message that carries it (thread =
// ask ID, sender = the asking session) and the decision message back to
// the asker are sent right after commit through the message store with
// stable operation IDs. A send that failed is retried by maintenance
// (resolveAsks) after askRepairGrace. The owner's inbox item (attention
// kind "ask", group ask:<id>) is derived from ask.changed events by the
// attention deriver; the carrying message raises no separate item.

var askMigrations = []sqlitex.Migration{{Namespace: "hub_asks", Version: 1, Name: "asks", SQL: `
CREATE TABLE asks (
	ask_id TEXT PRIMARY KEY,
	topic_key TEXT NOT NULL,
	scope TEXT NOT NULL, -- project_id, or 'owner' when the ask has no project
	project_id TEXT,
	session_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	context TEXT NOT NULL,
	options TEXT NOT NULL,
	default_option TEXT NOT NULL DEFAULT '',
	urgent INTEGER NOT NULL DEFAULT 0,
	deadline_at TEXT,
	deadline_rule TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	answer TEXT,
	count INTEGER NOT NULL DEFAULT 1,
	superseded_by TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT '',
	message_pending INTEGER NOT NULL DEFAULT 0, -- owner message not yet stored
	thread_id TEXT NOT NULL DEFAULT '',
	attention_id TEXT NOT NULL DEFAULT '',
	decision_message_id TEXT NOT NULL DEFAULT '',
	decision_pending INTEGER NOT NULL DEFAULT 0, -- decision message not yet sent to the asker
	norm_text TEXT NOT NULL DEFAULT '',
	decided_at TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX asks_open_topic ON asks(scope, topic_key) WHERE state='open';
CREATE INDEX asks_deadline ON asks(state, deadline_at);
CREATE INDEX asks_session ON asks(session_id, state);
CREATE INDEX asks_topic ON asks(scope, topic_key, updated_at);
CREATE INDEX asks_updated ON asks(updated_at, ask_id);
CREATE INDEX asks_message_pending ON asks(created_at) WHERE message_pending=1;
CREATE INDEX asks_decision_pending ON asks(decided_at) WHERE decision_pending=1;
CREATE TABLE ask_updates (
	ask_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	text TEXT NOT NULL,
	at TEXT NOT NULL,
	PRIMARY KEY (ask_id, seq)
);
CREATE VIRTUAL TABLE asks_fts USING fts5(title, context, options, ask_id UNINDEXED, tokenize='porter unicode61');
`}}

var _ contracts.AsksAPI = (*Hub)(nil)

const (
	askScopeOwner = "owner"
	// askDecidedWindow: a topic decided this recently answers a repeat
	// with the decision instead of asking again.
	askDecidedWindow = 7 * 24 * time.Hour
	// askRepairGrace: a message still unsent this long after its commit is
	// sent by maintenance (the request that committed it gave up).
	askRepairGrace = time.Minute
	// askMaxDeadline bounds DeadlineSeconds.
	askMaxDeadline   = 30 * 24 * time.Hour
	askMaxOptionID   = 32
	askMaxBatch      = 100
	askSimilarCutoff = 0.6
)

// askSystem answers asks at their deadline and withdraws them on archive.
var askSystem = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "asks"}

const askCols = `ask_id,topic_key,project_id,session_id,title,context,options,default_option,urgent,deadline_at,deadline_rule,state,answer,count,superseded_by,message_id,thread_id,attention_id,decision_message_id,created_at,updated_at`

func scanAsk(sc interface{ Scan(...any) error }) (contracts.Ask, error) {
	var a contracts.Ask
	var project, deadline, answer sql.NullString
	var options, state, created, updated string
	var urgent int
	if err := sc.Scan(&a.AskID, &a.TopicKey, &project, &a.SessionID, &a.Title, &a.Context, &options, &a.DefaultOption, &urgent, &deadline,
		&a.DeadlineRule, &state, &answer, &a.Count, &a.SupersededBy, &a.MessageID, &a.ThreadID, &a.AttentionID, &a.DecisionMessageID, &created, &updated); err != nil {
		return a, err
	}
	a.ProjectID = persistence.ProjectPtr(project)
	_ = persistence.FromJSON(options, &a.Options)
	a.Urgent = urgent != 0
	a.DeadlineAt = persistence.ParseTimePtr(deadline)
	a.State = contracts.AskState(state)
	if answer.Valid && answer.String != "" && answer.String != "null" {
		var ans contracts.AskAnswer
		if persistence.FromJSON(answer.String, &ans) == nil {
			a.Answer = &ans
		}
	}
	a.CreatedAt = persistence.ParseTime(created)
	a.UpdatedAt = persistence.ParseTime(updated)
	return a, nil
}

func (h *Hub) getAsk(ctx context.Context, q queryer, id string) (contracts.Ask, error) {
	a, err := scanAsk(q.QueryRowContext(ctx, `SELECT `+askCols+` FROM asks WHERE ask_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, contracts.Errorf(contracts.ErrNotFound, "ask not found").WithRef(contracts.KindAsk, id)
	}
	return a, err
}

func (h *Hub) askUpdates(ctx context.Context, q queryer, id string) ([]contracts.AskUpdate, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, session_id, text, at FROM ask_updates WHERE ask_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.AskUpdate
	for rows.Next() {
		var u contracts.AskUpdate
		var at string
		if err := rows.Scan(&u.Seq, &u.SessionID, &u.Text, &at); err != nil {
			return nil, err
		}
		u.At = persistence.ParseTime(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (h *Hub) emitAsk(ctx context.Context, tx *sql.Tx, a contracts.Ask) error {
	return h.emitPayload(ctx, tx, contracts.EvAskChanged, contracts.ObjectRef{Kind: contracts.KindAsk, ID: a.AskID}, a.SessionID, a.ProjectID, "hub", 0, a)
}

func askScope(project *string) string {
	if project == nil || *project == "" {
		return askScopeOwner
	}
	return *project
}

// ---- asks.create ----

// CreateAsk implements asks.create (rules in contracts/asks.go).
func (h *Hub) CreateAsk(ctx context.Context, req contracts.CreateAskRequest) (contracts.CreateAskResponse, error) {
	var out contracts.CreateAskResponse
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	in, err := normalizeAsk(req)
	if err != nil {
		return out, err
	}
	var asker contracts.Session
	switch {
	case isOwner(p):
	case p.Kind == contracts.PrincipalSession:
		s, err := h.getSession(ctx, h.db, p.ID)
		if err != nil {
			return out, err
		}
		if parent := s.Parent(); parent != "" {
			return h.redirectAsk(ctx, p, s, parent, in)
		}
		asker = s
	default:
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner and top-level sessions create asks")
	}
	if in.ProjectID == nil && asker.ProjectID != nil {
		in.ProjectID = asker.ProjectID
	}
	now := h.now()
	in.deadline, in.rule = askDeadline(now, in.DeadlineSeconds, in.Urgent, h.askQuietPrefs(ctx))
	err = h.mutate(ctx, req.OperationID, contracts.OpAsksCreate, p, req, &out, func(tx *sql.Tx) (any, error) {
		return h.createAskTx(ctx, tx, asker.SessionID, in, now)
	})
	if err == nil && out.Status == contracts.AskCreated && out.Ask != nil && out.Ask.MessageID != "" {
		h.sendAskMessage(ctx, *out.Ask)
	}
	return out, err
}

// askInput is a validated, normalized CreateAskRequest plus the computed
// deadline.
type askInput struct {
	contracts.CreateAskRequest
	deadline time.Time
	rule     string
}

func normalizeAsk(req contracts.CreateAskRequest) (askInput, error) {
	in := askInput{CreateAskRequest: req}
	in.TopicKey = NormalizeTopicKey(req.TopicKey)
	if in.TopicKey == "" {
		return in, contracts.Errorf(contracts.ErrInvalid, "topic_key is required (a short slug naming the decision, e.g. \"deploy-window\")")
	}
	in.Title = strings.Join(strings.Fields(req.Title), " ")
	if in.Title == "" {
		return in, contracts.Errorf(contracts.ErrInvalid, "title is required")
	}
	if n := utf8.RuneCountInString(in.Title); n > contracts.AskMaxTitle {
		return in, contracts.Errorf(contracts.ErrInvalid, "title is %d characters; limit %d", n, contracts.AskMaxTitle)
	}
	in.Context = strings.TrimSpace(req.Context)
	if in.Context == "" {
		return in, contracts.Errorf(contracts.ErrInvalid, "context is required. %s", contracts.AskContextGuide)
	}
	if n := utf8.RuneCountInString(in.Context); n > contracts.AskMaxContext {
		return in, contracts.Errorf(contracts.ErrInvalid, "context is %d characters; limit %d", n, contracts.AskMaxContext)
	}
	if n := len(req.Options); n < contracts.AskMinOptions || n > contracts.AskMaxOptions {
		return in, contracts.Errorf(contracts.ErrInvalid, "an ask needs %d to %d options (got %d)", contracts.AskMinOptions, contracts.AskMaxOptions, n)
	}
	in.Options = make([]contracts.AskOption, 0, len(req.Options))
	seen := map[string]bool{}
	for i, o := range req.Options {
		o.ID = strings.TrimSpace(o.ID)
		o.Label = strings.Join(strings.Fields(o.Label), " ")
		o.Detail = strings.Join(strings.Fields(o.Detail), " ")
		switch {
		case o.ID == "":
			return in, contracts.Errorf(contracts.ErrInvalid, "option %d has no id", i+1)
		case len(o.ID) > askMaxOptionID || strings.ContainsFunc(o.ID, func(r rune) bool { return r == ' ' || r == ',' || r == '=' }):
			return in, contracts.Errorf(contracts.ErrInvalid, "option id %q must be a short word (≤%d bytes, no spaces, commas or '=')", o.ID, askMaxOptionID)
		case seen[strings.ToLower(o.ID)]:
			return in, contracts.Errorf(contracts.ErrInvalid, "option id %q is used twice", o.ID)
		case o.Label == "":
			return in, contracts.Errorf(contracts.ErrInvalid, "option %q has no label", o.ID)
		case utf8.RuneCountInString(o.Label) > contracts.AskMaxOptionText || utf8.RuneCountInString(o.Detail) > contracts.AskMaxOptionText:
			return in, contracts.Errorf(contracts.ErrInvalid, "option %q label and detail are limited to %d characters each", o.ID, contracts.AskMaxOptionText)
		}
		seen[strings.ToLower(o.ID)] = true
		in.Options = append(in.Options, o)
	}
	in.DefaultOption = strings.TrimSpace(req.DefaultOption)
	if in.DefaultOption != "" {
		if _, ok := (contracts.Ask{Options: in.Options}).Option(in.DefaultOption); !ok {
			return in, contracts.Errorf(contracts.ErrInvalid, "default_option %q is not one of the option ids", in.DefaultOption)
		}
	}
	if req.DeadlineSeconds < 0 || time.Duration(req.DeadlineSeconds)*time.Second > askMaxDeadline {
		return in, contracts.Errorf(contracts.ErrInvalid, "deadline_seconds must be between 0 (default: 1 hour) and %d", int(askMaxDeadline/time.Second))
	}
	if req.ProjectID != nil && *req.ProjectID == "" {
		in.ProjectID = nil
	}
	return in, nil
}

// askQuality enforces the context minimum and an option Detail for a new
// ask (contracts.AskMinContext).
func askQuality(context string, options []contracts.AskOption) error {
	if n := utf8.RuneCountInString(context); n < contracts.AskMinContext {
		return contracts.Errorf(contracts.ErrInvalid, "context is %d characters; a new ask needs at least %d so the owner can decide without opening your session. %s (up to %d characters)",
			n, contracts.AskMinContext, contracts.AskContextGuide, contracts.AskMaxContext)
	}
	for _, o := range options {
		if o.Detail == "" {
			return contracts.Errorf(contracts.ErrInvalid, "option %q has no detail: say in one line what happens if the owner picks it (every option needs one)", o.ID)
		}
	}
	return nil
}

// NormalizeTopicKey lowercases a topic key and turns every run of other
// characters into one '-' (at most contracts.AskMaxTopicKey bytes).
func NormalizeTopicKey(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if isWordRune(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	out := b.String()
	for len(out) > contracts.AskMaxTopicKey {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return strings.TrimRight(out, "-")
}

func (h *Hub) createAskTx(ctx context.Context, tx *sql.Tx, sessionID string, in askInput, now time.Time) (contracts.CreateAskResponse, error) {
	scope := askScope(in.ProjectID)
	// Same topic open: merge as an update; no new owner message.
	open, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM asks WHERE scope=? AND topic_key=? AND state=?`, scope, in.TopicKey, string(contracts.AskOpen)))
	switch {
	case err == nil:
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM ask_updates WHERE ask_id=?`, open.AskID).Scan(&seq); err != nil {
			return contracts.CreateAskResponse{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ask_updates(ask_id,seq,session_id,text,at) VALUES(?,?,?,?,?)`,
			open.AskID, seq, sessionID, in.Context, persistence.Time(now)); err != nil {
			return contracts.CreateAskResponse{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE asks SET count=count+1, updated_at=? WHERE ask_id=?`, persistence.Time(now), open.AskID); err != nil {
			return contracts.CreateAskResponse{}, err
		}
		a, err := h.getAskFull(ctx, tx, open.AskID)
		if err != nil {
			return contracts.CreateAskResponse{}, err
		}
		if err := h.emitAsk(ctx, tx, a); err != nil {
			return contracts.CreateAskResponse{}, err
		}
		return contracts.CreateAskResponse{Status: contracts.AskMerged, Ask: &a,
			Detail: fmt.Sprintf("an ask on topic %q is already open (%s); your context was added to it as update %d — the owner is not notified again", in.TopicKey, a.AskID, seq)}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return contracts.CreateAskResponse{}, err
	}
	// Same topic decided recently: return the decision.
	if !in.Reopen {
		d, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askCols+` FROM asks WHERE scope=? AND topic_key=? AND state IN (?,?,?) AND decided_at>=? ORDER BY decided_at DESC LIMIT 1`,
			scope, in.TopicKey, string(contracts.AskAnswered), string(contracts.AskDefaulted), string(contracts.AskExpired), persistence.Time(now.Add(-askDecidedWindow))))
		switch {
		case err == nil:
			return contracts.CreateAskResponse{Status: contracts.AskAlreadyDecided, Ask: &d,
				Detail: fmt.Sprintf("topic %q was decided %s: %s. Act on that; to ask again because something changed, resend with reopen and say what changed in context", in.TopicKey, askClock(d.UpdatedAt), askOutcome(d))}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return contracts.CreateAskResponse{}, err
		}
	}
	// A new ask must stand on its own (a merge is a follow-up and may be
	// short).
	if err := askQuality(in.Context, in.Options); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	norm := askNormText(in.Title, in.Options)
	if !in.ConfirmNew {
		similar, err := h.similarAsksTx(ctx, tx, scope, in.Title, in.Context, norm)
		if err != nil {
			return contracts.CreateAskResponse{}, err
		}
		if len(similar) > 0 {
			ids := make([]string, len(similar))
			for i, s := range similar {
				ids[i] = fmt.Sprintf("%s (topic %q)", s.AskID, s.TopicKey)
			}
			return contracts.CreateAskResponse{Status: contracts.AskPossibleDuplicate, Candidates: similar,
				Detail: "similar open ask: " + strings.Join(ids, ", ") + ". Reuse its topic_key to add to it, or resend with confirm_new if this is a different decision"}, nil
		}
	}
	a := contracts.Ask{
		AskID: contracts.NewID(contracts.PrefixAsk), TopicKey: in.TopicKey, ProjectID: in.ProjectID, SessionID: sessionID,
		Title: in.Title, Context: in.Context, Options: in.Options, DefaultOption: in.DefaultOption, Urgent: in.Urgent,
		DeadlineRule: in.rule, State: contracts.AskOpen, Count: 1, CreatedAt: now, UpdatedAt: now,
	}
	dl := in.deadline
	a.DeadlineAt = &dl
	a.ThreadID = a.AskID
	if sessionID != "" { // the owner's own asks are not mailed to the owner
		a.MessageID = contracts.NewID(contracts.PrefixMessage)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asks(ask_id,topic_key,scope,project_id,session_id,title,context,options,default_option,urgent,deadline_at,deadline_rule,state,count,message_id,message_pending,thread_id,norm_text,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?)`,
		a.AskID, a.TopicKey, scope, persistence.ProjectArg(a.ProjectID), a.SessionID, a.Title, a.Context, persistence.JSON(a.Options), a.DefaultOption,
		boolInt(a.Urgent), persistence.TimePtr(a.DeadlineAt), a.DeadlineRule, string(a.State), a.MessageID, boolInt(a.MessageID != ""), a.ThreadID, norm,
		persistence.Time(now), persistence.Time(now)); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO asks_fts(title,context,options,ask_id) VALUES(?,?,?,?)`, a.Title, a.Context, askOptionsText(a.Options), a.AskID); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	if err := h.emitAsk(ctx, tx, a); err != nil {
		return contracts.CreateAskResponse{}, err
	}
	return contracts.CreateAskResponse{Status: contracts.AskCreated, Ask: &a, MessageID: a.MessageID}, nil
}

func (h *Hub) getAskFull(ctx context.Context, q queryer, id string) (contracts.Ask, error) {
	a, err := h.getAsk(ctx, q, id)
	if err != nil {
		return a, err
	}
	a.Updates, err = h.askUpdates(ctx, q, id)
	return a, err
}

// similarAsksTx finds open asks in scope that look like the same decision:
// FTS5 BM25 candidates over title and context, kept when the trigram
// Jaccard similarity of title+options is at least askSimilarCutoff.
func (h *Hub) similarAsksTx(ctx context.Context, tx *sql.Tx, scope, title, context, norm string) ([]contracts.Ask, error) {
	match := askMatchAny(title + " " + context)
	if match == "" {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.ask_id, a.norm_text FROM asks_fts JOIN asks a ON a.ask_id=asks_fts.ask_id
		WHERE asks_fts MATCH ? AND a.state=? AND a.scope=? ORDER BY bm25(asks_fts) LIMIT 20`, match, string(contracts.AskOpen), scope)
	if err != nil {
		return nil, err
	}
	var ids []string
	mine := trigrams(norm)
	for rows.Next() {
		var id, other string
		if err := rows.Scan(&id, &other); err != nil {
			rows.Close()
			return nil, err
		}
		if jaccard(mine, trigrams(other)) >= askSimilarCutoff {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]contracts.Ask, 0, len(ids))
	for _, id := range ids {
		a, err := h.getAsk(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// redirectAsk forwards a child session's ask to its parent as an ordinary
// queue message; nothing reaches the owner.
func (h *Hub) redirectAsk(ctx context.Context, p contracts.Principal, child contracts.Session, parentID string, in askInput) (contracts.CreateAskResponse, error) {
	parent, err := h.getSession(ctx, h.db, parentID)
	if err != nil {
		return contracts.CreateAskResponse{}, err
	}
	text := "[pyre ask from your child session — only top-level sessions ask the owner; decide it yourself or ask the owner from your session]\n\n" +
		renderAskBody(contracts.Ask{TopicKey: in.TopicKey, Title: in.Title, Context: in.Context, Options: in.Options, DefaultOption: in.DefaultOption})
	mv, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, p), contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: in.OperationID + ":ask-redirect"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: parent.SessionID},
		ProjectID: child.ProjectID,
		Mode:      contracts.ModeQueue,
		Body:      contracts.MessageBody{Text: text},
	})
	if err != nil {
		return contracts.CreateAskResponse{}, err
	}
	return contracts.CreateAskResponse{Status: contracts.AskRedirected, MessageID: mv.Envelope.MessageID,
		Detail: fmt.Sprintf("you are not a top-level session: the ask went to your parent %q (%s) as message %s; wait for its reply", parent.Title, parent.SessionID, mv.Envelope.MessageID)}, nil
}

// sendAskMessage stores the owner message carrying an open ask (stable
// operation and message IDs, so a repeat is a no-op).
func (h *Hub) sendAskMessage(ctx context.Context, a contracts.Ask) {
	if a.MessageID == "" || a.SessionID == "" {
		return
	}
	_, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSession, ID: a.SessionID}), contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: "op_ask-" + a.AskID},
		MessageID: a.MessageID,
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"},
		ProjectID: a.ProjectID,
		ThreadID:  a.ThreadID,
		Mode:      contracts.ModeQueue,
		Body:      contracts.MessageBody{Text: "[pyre ask] " + renderAskBody(a)},
	})
	if err != nil && retryableSend(err) {
		return // maintenance retries
	}
	_, _ = h.db.ExecContext(ctx, `UPDATE asks SET message_pending=0 WHERE ask_id=?`, a.AskID)
}

// retryableSend reports whether a failed internal send should be retried
// later (anything but a definitive refusal).
func retryableSend(err error) bool {
	for _, c := range []contracts.ErrorCode{contracts.ErrInvalid, contracts.ErrNotFound, contracts.ErrForbidden, contracts.ErrConflict, contracts.ErrPayloadTooLarge} {
		if contracts.IsCode(err, c) {
			return false
		}
	}
	return true
}

// ---- asks.list / asks.get ----

// ListAsks implements asks.list: newest change first.
func (h *Hub) ListAsks(ctx context.Context, req contracts.ListAsksRequest) (contracts.Page[contracts.Ask], error) {
	out := contracts.Page[contracts.Ask]{Items: []contracts.Ask{}, SnapshotAt: h.now()}
	if _, err := principal(ctx); err != nil {
		return out, err
	}
	cur, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return out, err
	}
	var where []string
	var args []any
	if len(req.States) > 0 {
		where = append(where, `state IN (?`+strings.Repeat(",?", len(req.States)-1)+`)`)
		for _, s := range req.States {
			args = append(args, string(s))
		}
	}
	if req.ProjectID != nil {
		where = append(where, `scope=?`)
		args = append(args, askScope(req.ProjectID))
	}
	if req.SessionID != "" {
		where = append(where, `session_id=?`)
		args = append(args, req.SessionID)
	}
	if req.TopicKey != "" {
		where = append(where, `topic_key=?`)
		args = append(args, NormalizeTopicKey(req.TopicKey))
	}
	if q := strings.TrimSpace(req.Query); q != "" {
		match := search.FTSQuery(q)
		if match == "" {
			return out, contracts.Errorf(contracts.ErrInvalid, "query has no words")
		}
		where = append(where, `ask_id IN (SELECT ask_id FROM asks_fts WHERE asks_fts MATCH ?)`)
		args = append(args, match)
	}
	if req.Since != nil {
		where = append(where, `(state=? OR decided_at>=?)`)
		args = append(args, string(contracts.AskOpen), persistence.Time(*req.Since))
	}
	if cur.K != "" {
		where = append(where, `(updated_at<? OR (updated_at=? AND ask_id<?))`)
		args = append(args, cur.K, cur.K, cur.I)
	}
	q := `SELECT ` + askCols + ` FROM asks`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, q+` ORDER BY updated_at DESC, ask_id DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, a)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > limit {
		last := out.Items[limit-1]
		out.Items = out.Items[:limit]
		out.NextCursor = persistence.EncodeCursor(persistence.PageCursor{K: persistence.Time(last.UpdatedAt), I: last.AskID})
	}
	rows.Close()
	// Merged follow-ups are part of the decision; inbox views show them.
	for i := range out.Items {
		if out.Items[i].Count > 1 {
			if out.Items[i].Updates, err = h.askUpdates(ctx, h.db, out.Items[i].AskID); err != nil {
				return out, err
			}
		}
	}
	h.fillAskAttention(ctx, out.Items)
	return out, nil
}

// GetAsk implements asks.get (with merged updates).
func (h *Hub) GetAsk(ctx context.Context, req contracts.GetRequest) (contracts.Ask, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Ask{}, err
	}
	a, err := h.getAskFull(ctx, h.db, req.ID)
	if err != nil {
		return a, err
	}
	one := []contracts.Ask{a}
	h.fillAskAttention(ctx, one)
	return one[0], nil
}

// fillAskAttention sets AttentionID from the owner's inbox item for each
// ask (group ask:<id>), when the attention store shares this database.
func (h *Hub) fillAskAttention(ctx context.Context, asks []contracts.Ask) {
	if len(asks) == 0 {
		return
	}
	args := make([]any, 0, len(asks))
	for _, a := range asks {
		if a.AttentionID == "" {
			args = append(args, askAttentionGroup(a.AskID))
		}
	}
	if len(args) == 0 {
		return
	}
	rows, err := h.db.QueryContext(ctx, `SELECT group_key, attention_id FROM attention_items WHERE group_key IN (?`+strings.Repeat(",?", len(args)-1)+`) ORDER BY created_at`, args...)
	if err != nil {
		return // no attention store on this database
	}
	defer rows.Close()
	ids := map[string]string{}
	for rows.Next() {
		var g, id string
		if rows.Scan(&g, &id) == nil {
			ids[g] = id
		}
	}
	for i := range asks {
		if asks[i].AttentionID == "" {
			asks[i].AttentionID = ids[askAttentionGroup(asks[i].AskID)]
		}
	}
}

func askAttentionGroup(askID string) string { return "ask:" + askID }

// ---- asks.answer ----

// AnswerAsks implements asks.answer (owner only). Each item commits on its
// own (operation ID <op>:<index>); the decisions then go to each asking
// session as one combined message.
func (h *Hub) AnswerAsks(ctx context.Context, req contracts.AnswerAsksRequest) (contracts.AnswerAsksResponse, error) {
	out := contracts.AnswerAsksResponse{Results: []contracts.AnswerAskResult{}}
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if !isOwner(p) {
		return out, contracts.Errorf(contracts.ErrForbidden, "only the owner answers asks")
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	if len(req.Answers) == 0 || len(req.Answers) > askMaxBatch {
		return out, contracts.Errorf(contracts.ErrInvalid, "answer 1 to %d asks per call (got %d)", askMaxBatch, len(req.Answers))
	}
	return h.answerAsks(ctx, p, req.Answers, func(i int) string { return fmt.Sprintf("%s:%d", req.OperationID, i) }), nil
}

// answerAsks commits each answer on its own (operation ID opFor(i)) and
// sends the decisions, one combined message per asking session. AnswerAsks
// and the held-answer flush (asks_hold.go) share it.
func (h *Hub) answerAsks(ctx context.Context, p contracts.Principal, items []contracts.AnswerAskItem, opFor func(i int) string) contracts.AnswerAsksResponse {
	out := contracts.AnswerAsksResponse{Results: []contracts.AnswerAskResult{}}
	var decided []contracts.Ask
	for i, item := range items {
		var a contracts.Ask
		err := h.mutate(ctx, opFor(i), contracts.OpAsksAnswer, p, item, &a, func(tx *sql.Tx) (any, error) {
			return h.answerAskTx(ctx, tx, p, item)
		})
		if err != nil {
			e := contracts.AsError(err)
			if e == nil {
				e = contracts.Errorf(contracts.ErrInternal, "%v", err)
			}
			out.Results = append(out.Results, contracts.AnswerAskResult{AskID: item.AskID, Error: e})
			continue
		}
		decided = append(decided, a)
		out.Results = append(out.Results, contracts.AnswerAskResult{AskID: item.AskID, Ask: &a})
	}
	sent := h.sendDecisions(ctx, decided, p)
	for i := range out.Results {
		if a := out.Results[i].Ask; a != nil && sent[a.AskID] != "" {
			a.DecisionMessageID = sent[a.AskID]
		}
	}
	return out
}

// checkAnswer validates an answer against its ask and returns it trimmed
// (answers and held answers).
func checkAnswer(a contracts.Ask, item contracts.AnswerAskItem) (contracts.AnswerAskItem, error) {
	item.AskID = a.AskID
	item.OptionID = strings.TrimSpace(item.OptionID)
	item.Text = strings.TrimSpace(item.Text)
	if item.OptionID != "" {
		o, ok := a.Option(item.OptionID)
		if !ok {
			return item, contracts.Errorf(contracts.ErrInvalid, "%q is not an option of %s", item.OptionID, a.AskID).WithRef(contracts.KindAsk, a.AskID)
		}
		item.OptionID = o.ID
	} else if item.Text == "" {
		return item, contracts.Errorf(contracts.ErrInvalid, "choose an option or give a text answer").WithRef(contracts.KindAsk, a.AskID)
	}
	if utf8.RuneCountInString(item.Text) > contracts.AskMaxContext {
		return item, contracts.Errorf(contracts.ErrInvalid, "answer text is limited to %d characters", contracts.AskMaxContext)
	}
	return item, nil
}

func (h *Hub) answerAskTx(ctx context.Context, tx *sql.Tx, p contracts.Principal, item contracts.AnswerAskItem) (contracts.Ask, error) {
	a, err := h.getAsk(ctx, tx, item.AskID)
	if err != nil {
		return a, err
	}
	if item, err = checkAnswer(a, item); err != nil {
		return a, err
	}
	return h.decideAskTx(ctx, tx, a, contracts.AskAnswered, contracts.AskAnswer{OptionID: item.OptionID, Text: item.Text, By: p, At: h.now()}, "")
}

// decideAskTx moves an open ask to a final state (first commit wins: an ask
// already decided is a conflict).
func (h *Hub) decideAskTx(ctx context.Context, tx *sql.Tx, a contracts.Ask, state contracts.AskState, ans contracts.AskAnswer, supersededBy string) (contracts.Ask, error) {
	now := h.now()
	notify := a.SessionID != "" && (state == contracts.AskAnswered || state == contracts.AskDefaulted || state == contracts.AskExpired ||
		(state == contracts.AskWithdrawn || state == contracts.AskSuperseded) && ans.By.Kind == contracts.PrincipalOwner)
	res, err := tx.ExecContext(ctx, `UPDATE asks SET state=?, answer=?, superseded_by=?, decided_at=?, updated_at=?, decision_pending=? WHERE ask_id=? AND state=?`,
		string(state), persistence.JSON(ans), supersededBy, persistence.Time(now), persistence.Time(now), boolInt(notify), a.AskID, string(contracts.AskOpen))
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := h.getAsk(ctx, tx, a.AskID)
		if err != nil {
			return a, err
		}
		return a, contracts.Errorf(contracts.ErrConflict, "ask %s is already %s (%s)", a.AskID, cur.State, askOutcome(cur)).WithRef(contracts.KindAsk, a.AskID).WithDetail("state", string(cur.State))
	}
	a, err = h.getAskFull(ctx, tx, a.AskID)
	if err != nil {
		return a, err
	}
	if err := h.dropHeldTx(ctx, tx, a.AskID); err != nil { // asks_hold.go
		return a, err
	}
	return a, h.emitAsk(ctx, tx, a)
}

// ---- asks.withdraw ----

// WithdrawAsk implements asks.withdraw (the asker or the owner).
func (h *Hub) WithdrawAsk(ctx context.Context, req contracts.WithdrawAskRequest) (contracts.Ask, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.Ask{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.Ask{}, err
	}
	if req.SupersededBy != "" && !contracts.ValidID(req.SupersededBy, contracts.PrefixAsk) {
		return contracts.Ask{}, contracts.Errorf(contracts.ErrInvalid, "superseded_by %q is not an ask id", req.SupersededBy)
	}
	var out contracts.Ask
	err = h.mutate(ctx, req.OperationID, contracts.OpAsksWithdraw, p, req, &out, func(tx *sql.Tx) (any, error) {
		a, err := h.getAsk(ctx, tx, req.AskID)
		if err != nil {
			return nil, err
		}
		if !isOwner(p) && !(p.Kind == contracts.PrincipalSession && p.ID == a.SessionID) {
			return nil, contracts.Errorf(contracts.ErrForbidden, "only the owner or the asking session withdraws an ask").WithRef(contracts.KindAsk, a.AskID)
		}
		state := contracts.AskWithdrawn
		if req.SupersededBy != "" {
			if req.SupersededBy == a.AskID {
				return nil, contracts.Errorf(contracts.ErrInvalid, "an ask cannot supersede itself")
			}
			if _, err := h.getAsk(ctx, tx, req.SupersededBy); err != nil {
				return nil, err
			}
			state = contracts.AskSuperseded
		}
		return h.decideAskTx(ctx, tx, a, state, contracts.AskAnswer{Text: strings.TrimSpace(req.Reason), By: p, At: h.now()}, req.SupersededBy)
	})
	if err == nil && isOwner(p) {
		if sent := h.sendDecisions(ctx, []contracts.Ask{out}, p); sent[out.AskID] != "" {
			out.DecisionMessageID = sent[out.AskID]
		}
	}
	return out, err
}

// withdrawSessionAsksTx withdraws an archived session's open asks.
func (h *Hub) withdrawSessionAsksTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+askCols+` FROM asks WHERE session_id=? AND state=?`, sessionID, string(contracts.AskOpen))
	if err != nil {
		return err
	}
	var open []contracts.Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			rows.Close()
			return err
		}
		open = append(open, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range open {
		if _, err := h.decideAskTx(ctx, tx, a, contracts.AskWithdrawn, contracts.AskAnswer{Text: "the asking session was archived", By: askSystem, At: h.now()}, ""); err != nil {
			return err
		}
	}
	return nil
}

// ---- deadlines and repair (maintenance) ----

// resolveAsks applies defaults to asks past their deadline (or expires
// them), then sends owner and decision messages that an earlier request
// committed but could not send.
func (h *Hub) resolveAsks(ctx context.Context) error {
	var errs []error
	if err := h.sendHeldIfQuiet(ctx); err != nil { // asks_hold.go: the batch's quiet window passed
		errs = append(errs, err)
	}
	now := h.now()
	rows, err := h.db.QueryContext(ctx, `SELECT `+askCols+` FROM asks WHERE state=? AND deadline_at IS NOT NULL AND deadline_at<=? ORDER BY deadline_at LIMIT 200`,
		string(contracts.AskOpen), persistence.Time(now))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var due []contracts.Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			rows.Close()
			return err
		}
		due = append(due, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The owner's held answer wins over the default: send it first (the
	// default below then conflicts and is skipped).
	if err := h.sendHeldForDue(ctx, due); err != nil {
		errs = append(errs, err)
	}
	var decided []contracts.Ask
	for _, a := range due {
		state, ans := contracts.AskExpired, contracts.AskAnswer{By: askSystem, ByDefault: true, At: now}
		if a.DefaultOption != "" {
			state, ans.OptionID = contracts.AskDefaulted, a.DefaultOption
		}
		var d contracts.Ask
		err := h.tx(ctx, func(tx *sql.Tx) error {
			var err error
			d, err = h.decideAskTx(ctx, tx, a, state, ans, "")
			return err
		})
		switch {
		case contracts.IsCode(err, contracts.ErrConflict):
			continue // answered meanwhile: the answer won
		case err != nil:
			errs = append(errs, err)
			continue
		}
		decided = append(decided, d)
	}
	h.sendDecisions(ctx, decided, askSystem)
	errs = append(errs, h.repairAskMessages(ctx, now))
	return errors.Join(errs...)
}

func (h *Hub) repairAskMessages(ctx context.Context, now time.Time) error {
	cut := persistence.Time(now.Add(-askRepairGrace))
	load := func(q string, args ...any) ([]contracts.Ask, error) {
		rows, err := h.db.QueryContext(ctx, `SELECT `+askCols+` FROM asks WHERE `+q+` LIMIT 100`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []contracts.Ask
		for rows.Next() {
			a, err := scanAsk(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, rows.Err()
	}
	owed, err := load(`message_pending=1 AND created_at<=?`, cut)
	if err != nil {
		return err
	}
	for _, a := range owed {
		h.sendAskMessage(ctx, a)
	}
	undelivered, err := load(`decision_pending=1 AND decided_at<=? ORDER BY decided_at`, cut)
	if err != nil {
		return err
	}
	h.sendDecisions(ctx, undelivered, askSystem)
	return nil
}

// sendDecisions tells each asking session the outcome of its decided asks
// in one combined queue message per session, replying to the (first) ask's
// owner message. It returns the decision message ID per ask.
func (h *Hub) sendDecisions(ctx context.Context, asks []contracts.Ask, sender contracts.Principal) map[string]string {
	sent := map[string]string{}
	var order []string
	groups := map[string][]contracts.Ask{}
	for _, a := range asks {
		if a.SessionID == "" || !a.State.Decided() {
			continue
		}
		if _, ok := groups[a.SessionID]; !ok {
			order = append(order, a.SessionID)
		}
		groups[a.SessionID] = append(groups[a.SessionID], a)
	}
	for _, sid := range order {
		g := groups[sid]
		ids := make([]any, 0, len(g))
		for _, a := range g {
			ids = append(ids, a.AskID)
		}
		settle := func(messageID string) {
			_, _ = h.db.ExecContext(ctx, `UPDATE asks SET decision_pending=0, decision_message_id=? WHERE ask_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`,
				append([]any{messageID}, ids...)...)
		}
		s, err := h.getSession(ctx, h.db, sid)
		if err != nil || s.Archived {
			settle("") // nobody to tell
			continue
		}
		hash := sha256.New()
		for _, a := range g {
			hash.Write([]byte(a.AskID + ":" + string(a.State) + ";"))
		}
		mv, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, sender), contracts.SendMessageRequest{
			Mutation:  contracts.Mutation{OperationID: "op_askdecision-" + hex.EncodeToString(hash.Sum(nil))[:32]},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid},
			ProjectID: g[0].ProjectID,
			InReplyTo: g[0].MessageID,
			Mode:      contracts.ModeQueue,
			Body:      contracts.MessageBody{Text: renderDecisions(g)},
		})
		if err != nil {
			if !retryableSend(err) {
				settle("")
			}
			continue // maintenance retries
		}
		settle(mv.Envelope.MessageID)
		h.ownerActive(sender, sid) // the owner answering is the owner messaging it (auto-resolves are not)
		for _, a := range g {
			sent[a.AskID] = mv.Envelope.MessageID
		}
	}
	return sent
}
