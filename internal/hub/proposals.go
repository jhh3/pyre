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
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Proposals (contracts/standing_grants.go): a session never records the
// owner's go-ahead or a project fact itself; it proposes one, and the
// proposal is an ordinary owner ask (CreateAsk; a child's goes to its
// parent) whose title, context and options (approve / reject, no default)
// the hub writes. When the owner answers approve, the hub writes the
// standing grant in the answer's transaction, or appends the fact to the
// project's fact sheet right after it (retried by maintenance), quoting
// only his own answer text and citing the ask ID.

var proposalMigrations = []sqlitex.Migration{{Namespace: "hub_proposals", Version: 1, Name: "ask_proposals", SQL: `
CREATE TABLE ask_proposals (
	ask_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL, -- grant | fact
	payload TEXT NOT NULL, -- AskProposal JSON (kind, grant, fact)
	state TEXT NOT NULL, -- pending | approved | applied | rejected | failed
	result_id TEXT NOT NULL DEFAULT '', -- grant ID or fact sheet revision ID
	error TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE INDEX ask_proposals_state ON ask_proposals(state, kind);
`}}

// factsPrincipal writes approved facts to the fact sheet (the documents
// guard allows the owner and this principal only).
var factsPrincipal = contracts.FactSheetPrincipal

const (
	// proposalDeadline is a proposal's deadline when the request has none.
	proposalDeadline = 24 * time.Hour
	proposalMaxWhy   = 8000
)

// factSheetIntro opens a fact sheet the hub creates.
const factSheetIntro = "# " + contracts.FactSheetTitle + "\n\nSettled facts for this project. Each line was approved by the owner in his inbox (his words are quoted when he typed any) and cites the ask. Pyre puts this sheet into every session started in the project. Sessions propose facts with propose_fact; the owner edits this document directly.\n\n"

// prepareProposal turns a proposal request into the ask the owner sees:
// the hub's title, structured context and options, the session's Context
// kept as its reason. Only sessions propose (the owner creates grants and
// edits the fact sheet directly).
func (h *Hub) prepareProposal(ctx context.Context, p contracts.Principal, asker contracts.Session, req contracts.CreateAskRequest) (contracts.CreateAskRequest, error) {
	if isOwner(p) || p.Kind != contracts.PrincipalSession || asker.SessionID == "" {
		return req, contracts.Errorf(contracts.ErrInvalid, "proposals are for sessions: the owner creates grants directly (`pyre grant add`) and edits the fact sheet")
	}
	pr := contracts.AskProposal{Kind: contracts.ProposalKind(strings.TrimSpace(string(req.Proposal.Kind)))}
	why := strings.TrimSpace(req.Context)
	if utf8.RuneCountInString(why) > proposalMaxWhy {
		return req, contracts.Errorf(contracts.ErrInvalid, "the reason (context) is limited to %d characters", proposalMaxWhy)
	}
	if why == "" {
		why = "(no reason given)"
	}
	from := fmt.Sprintf("session %q (%s)", oneLine(asker.Title, 80), asker.SessionID)
	var title, topic string
	var b strings.Builder
	switch pr.Kind {
	case contracts.ProposalGrant:
		if req.Proposal.Grant == nil {
			return req, contracts.Errorf(contracts.ErrInvalid, "a grant proposal needs grant (action, option)")
		}
		g := *req.Proposal.Grant
		g.Action = NormalizeGrantAction(g.Action, contracts.GrantMaxAction)
		g.Option = strings.Join(strings.Fields(g.Option), " ")
		g.Condition = strings.Join(strings.Fields(g.Condition), " ")
		g.Scope = contracts.GrantScope(strings.TrimSpace(string(g.Scope)))
		g.SessionID = strings.TrimSpace(g.SessionID)
		if err := checkGrantFields(g.Action, g.Option, g.Condition, ""); err != nil {
			return req, err
		}
		if g.ExpiresInDays == 0 {
			g.ExpiresInDays = int(contracts.GrantDefaultTTL / (24 * time.Hour))
		}
		if maxDays := int(contracts.GrantMaxTTL / (24 * time.Hour)); g.ExpiresInDays < 1 || g.ExpiresInDays > maxDays {
			return req, contracts.Errorf(contracts.ErrInvalid, "expires_in_days is 1 to %d (default 7)", maxDays)
		}
		if g.Scope == "" {
			g.Scope = contracts.GrantScopeSession
			if asker.ProjectID != nil && g.SessionID == "" {
				g.Scope = contracts.GrantScopeProject
			}
		}
		var holders string
		switch g.Scope {
		case contracts.GrantScopeProject:
			if asker.ProjectID == nil {
				return req, contracts.Errorf(contracts.ErrInvalid, "this session is not in a project: propose scope session (this session and the sessions it starts)")
			}
			g.SessionID = ""
			holders = "every session in project " + h.projectLabel(ctx, *asker.ProjectID)
		case contracts.GrantScopeSession:
			if g.SessionID == "" {
				g.SessionID = asker.SessionID
			}
			root, err := h.getSession(ctx, h.db, g.SessionID)
			if err != nil {
				return req, err
			}
			if ok, err := h.inChain(ctx, h.db, root.SessionID, asker.SessionID); err != nil || !ok {
				if err != nil {
					return req, err
				}
				return req, contracts.Errorf(contracts.ErrForbidden, "a session proposes grants only for itself or the sessions it started").WithRef(contracts.KindSession, root.SessionID)
			}
			holders = fmt.Sprintf("session %q (%s) and every session it starts", oneLine(root.Title, 80), root.SessionID)
		default:
			return req, contracts.Errorf(contracts.ErrInvalid, "scope must be project or session")
		}
		pr.Grant = &g
		title = fmt.Sprintf("Remember this go-ahead? %s → %s", g.Action, g.Option)
		// Scope, holder and expiry are part of the decision: proposals that
		// differ in any of them are separate asks, never merged.
		holder := g.SessionID
		if g.Scope == contracts.GrantScopeProject && asker.ProjectID != nil {
			holder = *asker.ProjectID
		}
		topic = fmt.Sprintf("proposal-grant-%s-%s-%s-%s-%dd", g.Action, NormalizeTopicKey(g.Option), g.Scope, NormalizeTopicKey(holder), g.ExpiresInDays)
		fmt.Fprintf(&b, "**Proposed standing go-ahead** from %s. Nothing is recorded unless you approve.\n\n", from)
		fmt.Fprintf(&b, "- Action: `%s` (only asks that name exactly this action)\n", g.Action)
		fmt.Fprintf(&b, "- Picks option: %q (only asks that offer it, have a default and are not marked irreversible)\n", g.Option)
		fmt.Fprintf(&b, "- Holds for: %s\n", holders)
		fmt.Fprintf(&b, "- Expires: %d days after you approve (revoke it any time on the project page or with `pyre grant revoke`)\n", g.ExpiresInDays)
		if g.Condition != "" {
			fmt.Fprintf(&b, "- Note (the session's words; Pyre never checks it): %s\n", g.Condition)
		}
		b.WriteString("\nApprove: Pyre records the grant with this ask's ID; anything you type with your answer is saved as your words. You get an FYI each time it answers an ask. Reject, or no answer, records nothing. It never covers production deploys, deleting data, spending money, force-pushes, public releases or account changes, an ask on a topic decided in the last 7 days, or a reopened one.\n\n")
	case contracts.ProposalFact:
		fact := strings.Join(strings.Fields(req.Proposal.Fact), " ")
		switch {
		case fact == "":
			return req, contracts.Errorf(contracts.ErrInvalid, "fact is required: the settled fact, one line")
		case utf8.RuneCountInString(fact) > contracts.FactMaxText:
			return req, contracts.Errorf(contracts.ErrInvalid, "fact is limited to %d characters", contracts.FactMaxText)
		case asker.ProjectID == nil:
			return req, contracts.Errorf(contracts.ErrInvalid, "this session is not in a project; a fact belongs to a project's fact sheet")
		}
		pr.Fact = fact
		sum := sha256.Sum256([]byte(strings.ToLower(fact)))
		title = "Add to the fact sheet? " + fact
		topic = "proposal-fact-" + hex.EncodeToString(sum[:])[:12]
		fmt.Fprintf(&b, "**Proposed project fact** for the fact sheet of project %s, from %s. Nothing is added unless you approve.\n\n> %s\n\n", h.projectLabel(ctx, *asker.ProjectID), from, fact)
		b.WriteString("Approve: Pyre appends it to the fact sheet (document `facts`), which every new session in the project reads at launch, citing this ask; anything you type with your answer is quoted as your words. Reject, or no answer, adds nothing.\n\n")
	default:
		return req, contracts.Errorf(contracts.ErrInvalid, "proposal kind must be grant or fact")
	}
	b.WriteString("**Why the session proposes it** (its words):\n\n" + why)
	def := strings.TrimSpace(req.DefaultOption)
	if def != "" && def != contracts.ProposalReject {
		return req, contracts.Errorf(contracts.ErrInvalid, "a proposal has no default (or %q): it is never approved without the owner", contracts.ProposalReject)
	}
	if len(req.Items) > 0 {
		return req, contracts.Errorf(contracts.ErrInvalid, "a proposal has no items")
	}
	deadline := req.DeadlineSeconds
	if deadline == 0 {
		deadline = int(proposalDeadline / time.Second)
	}
	if n := utf8.RuneCountInString(title); n > contracts.AskMaxTitle {
		title = string([]rune(title)[:contracts.AskMaxTitle-1]) + "…"
	}
	return contracts.CreateAskRequest{
		Mutation: req.Mutation, TopicKey: topic, Title: title, Context: b.String(),
		Options: []contracts.AskOption{
			{ID: contracts.ProposalApprove, Label: "Approve", Detail: "Pyre records it, citing this ask; your typed words, if any, are saved as yours"},
			{ID: contracts.ProposalReject, Label: "Reject", Detail: "Nothing is recorded"},
		},
		DefaultOption: def, DeadlineSeconds: deadline, ProjectID: asker.ProjectID,
		Proposal: &pr, Reopen: req.Reopen, ConfirmNew: true,
	}, nil
}

// projectLabel is "<name> (<id>)", or the ID when the project is unknown.
func (h *Hub) projectLabel(ctx context.Context, id string) string {
	if pr, err := h.GetProject(h.systemCtx(ctx), contracts.GetRequest{ID: id}); err == nil && pr.Name != "" {
		return fmt.Sprintf("%q (%s)", pr.Name, id)
	}
	return id
}

func (h *Hub) insertProposalTx(ctx context.Context, tx *sql.Tx, askID string, pr contracts.AskProposal, now time.Time) error {
	pr.State, pr.ResultID, pr.Error = "", "", ""
	_, err := tx.ExecContext(ctx, `INSERT INTO ask_proposals(ask_id,kind,payload,state,updated_at) VALUES(?,?,?,?,?)`,
		askID, string(pr.Kind), persistence.JSON(pr), string(contracts.AskProposalPending), persistence.Time(now))
	return err
}

const proposalCols = `ask_id,payload,state,result_id,error`

func scanProposal(sc interface{ Scan(...any) error }) (string, contracts.AskProposal, error) {
	var id, payload, state string
	var pr contracts.AskProposal
	if err := sc.Scan(&id, &payload, &state, &pr.ResultID, &pr.Error); err != nil {
		return id, pr, err
	}
	_ = persistence.FromJSON(payload, &pr)
	pr.State = contracts.AskProposalState(state)
	return id, pr, nil
}

// getProposal returns an ask's proposal (nil when it is not one).
func (h *Hub) getProposal(ctx context.Context, q queryer, askID string) (*contracts.AskProposal, error) {
	_, pr, err := scanProposal(q.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM ask_proposals WHERE ask_id=?`, askID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// fillProposals sets Proposal on the asks that are proposals.
func (h *Hub) fillProposals(ctx context.Context, asks []contracts.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	args := make([]any, len(asks))
	for i, a := range asks {
		args[i] = a.AskID
	}
	rows, err := h.db.QueryContext(ctx, `SELECT `+proposalCols+` FROM ask_proposals WHERE ask_id IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string]contracts.AskProposal{}
	for rows.Next() {
		id, pr, err := scanProposal(rows)
		if err != nil {
			return err
		}
		by[id] = pr
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range asks {
		if pr, ok := by[asks[i].AskID]; ok {
			asks[i].Proposal = &pr
		}
	}
	return nil
}

func (h *Hub) setProposalTx(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, askID string, state contracts.AskProposalState, resultID, errText string) error {
	_, err := q.ExecContext(ctx, `UPDATE ask_proposals SET state=?, result_id=?, error=?, updated_at=? WHERE ask_id=?`,
		string(state), resultID, errText, persistence.Time(h.now()), askID)
	return err
}

// settleProposalTx records the owner's answer to a proposal in the
// answer's transaction: approve writes the grant now (or marks the fact
// for the fact sheet); any other answer writes nothing.
func (h *Hub) settleProposalTx(ctx context.Context, tx *sql.Tx, a contracts.Ask, p contracts.Principal) (contracts.Ask, error) {
	pr := a.Proposal
	if pr == nil || pr.State != contracts.AskProposalPending || !a.State.Decided() {
		return a, nil
	}
	approved := a.State == contracts.AskAnswered && a.Answer != nil && a.Answer.OptionID == contracts.ProposalApprove && isOwner(p)
	var err error
	switch {
	case !approved:
		err = h.setProposalTx(ctx, tx, a.AskID, contracts.AskProposalRejected, "", "")
	case pr.Kind == contracts.ProposalFact:
		err = h.setProposalTx(ctx, tx, a.AskID, contracts.AskProposalApproved, "", "")
	case pr.Kind == contracts.ProposalGrant && pr.Grant != nil:
		var g contracts.StandingGrant
		g, err = h.approvedGrant(ctx, tx, a, *pr.Grant, p)
		if err == nil {
			g, err = h.insertGrantTx(ctx, tx, g)
		}
		if err == nil {
			err = h.setProposalTx(ctx, tx, a.AskID, contracts.AskProposalApplied, g.GrantID, "")
		} else if e := contracts.AsError(err); e != nil && e.Code != contracts.ErrInternal {
			// Not grantable any more (e.g. the session is gone): the
			// answer stands, nothing is recorded.
			err = h.setProposalTx(ctx, tx, a.AskID, contracts.AskProposalFailed, "", e.Message)
		}
	default:
		err = h.setProposalTx(ctx, tx, a.AskID, contracts.AskProposalFailed, "", "malformed proposal")
	}
	if err != nil {
		return a, err
	}
	a.Proposal, err = h.getProposal(ctx, tx, a.AskID)
	return a, err
}

// approvedGrant is the grant an approved proposal records: the owner's
// answer text as Quote (nothing the session wrote), the ask as AskID.
func (h *Hub) approvedGrant(ctx context.Context, q queryer, a contracts.Ask, gp contracts.GrantProposal, p contracts.Principal) (contracts.StandingGrant, error) {
	quote := strings.TrimSpace(a.Answer.Text)
	if utf8.RuneCountInString(quote) > contracts.GrantMaxQuote {
		quote = string([]rune(quote)[:contracts.GrantMaxQuote])
	}
	g := contracts.StandingGrant{
		Scope: gp.Scope, SessionID: gp.SessionID, ProjectID: a.ProjectID, Action: gp.Action, Option: gp.Option, Condition: gp.Condition,
		Quote: quote, AskID: a.AskID, ProposedBy: &contracts.Principal{Kind: contracts.PrincipalSession, ID: a.SessionID},
		RecordedBy: p, ExpiresAt: h.now().Add(time.Duration(gp.ExpiresInDays) * 24 * time.Hour),
	}
	if err := checkGrantFields(g.Action, g.Option, g.Condition, ""); err != nil {
		return g, err
	}
	switch g.Scope {
	case contracts.GrantScopeProject:
		g.SessionID = ""
		if g.ProjectID == nil {
			return g, contracts.Errorf(contracts.ErrInvalid, "the proposal's project is gone")
		}
	case contracts.GrantScopeSession:
		s, err := h.getSession(ctx, q, g.SessionID)
		if err != nil {
			return g, err
		}
		g.ProjectID = s.ProjectID
	default:
		return g, contracts.Errorf(contracts.ErrInvalid, "scope must be project or session")
	}
	return g, nil
}

// applyApprovedFacts appends the facts of just-approved proposals among
// asks to their fact sheets (a failure is retried by maintenance).
func (h *Hub) applyApprovedFacts(ctx context.Context, asks []contracts.Ask) {
	for i, a := range asks {
		if a.Proposal == nil || a.Proposal.Kind != contracts.ProposalFact || a.Proposal.State != contracts.AskProposalApproved {
			continue
		}
		_ = h.applyFact(ctx, a.AskID)
		if pr, err := h.getProposal(ctx, h.db, a.AskID); err == nil {
			asks[i].Proposal = pr
		}
	}
}

// retryApprovedFacts retries approved facts not yet on their fact sheet.
func (h *Hub) retryApprovedFacts(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT ask_id FROM ask_proposals WHERE state=? AND kind=? ORDER BY updated_at LIMIT 50`,
		string(contracts.AskProposalApproved), string(contracts.ProposalFact))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var errs []error
	for _, id := range ids {
		errs = append(errs, h.applyFact(ctx, id))
	}
	return errors.Join(errs...)
}

// factLineFor is an approved fact's line: the owner's words when he typed
// any, else the approved text labeled as an approval (never as his words).
func factLineFor(fact, answer, askID string, at time.Time) string {
	fact = strings.Join(strings.Fields(fact), " ")
	answer = strings.Join(strings.Fields(answer), " ")
	when := at.In(pacific).Format("Jan 2 2006 3:04 PM MST")
	if answer != "" {
		return fmt.Sprintf("- %s — owner: %q (approved %s, %s)\n", fact, answer, askID, when)
	}
	return fmt.Sprintf("- %s — owner approved proposal: %q (approved %s, %s)\n", fact, fact, askID, when)
}

// applyFact appends an approved fact to its project's fact sheet as the
// facts principal (creating the sheet on first use). Idempotent: a sheet
// already citing the ask counts as written. A definitive refusal marks the
// proposal failed; anything else is retried by maintenance.
func (h *Hub) applyFact(ctx context.Context, askID string) error {
	a, err := h.getAskFull(ctx, h.db, askID)
	if err != nil {
		return err
	}
	pr := a.Proposal
	if pr == nil || pr.Kind != contracts.ProposalFact || pr.State != contracts.AskProposalApproved || a.Answer == nil {
		return nil
	}
	fail := func(msg string) error { return h.setProposalTx(ctx, h.db, askID, contracts.AskProposalFailed, "", msg) }
	if a.ProjectID == nil || *a.ProjectID == "" {
		return fail("the proposal has no project")
	}
	docs := h.cfg.Documents
	if docs == nil {
		return fail("this hub has no document store")
	}
	pid := *a.ProjectID
	fctx := contracts.WithPrincipal(ctx, factsPrincipal)
	line := factLineFor(pr.Fact, a.Answer.Text, askID, a.Answer.At)
	summary := "fact (approved " + askID + "): " + oneLine(pr.Fact, 60)
	refused := func(err error) error {
		if retryableSend(err) {
			return err // maintenance retries
		}
		return fail(err.Error())
	}
	for attempt := 0; attempt < 4; attempt++ {
		cur, err := docs.GetDocument(fctx, contracts.GetDocumentRequest{ProjectID: pid, Slug: contracts.FactSheetSlug})
		if contracts.IsCode(err, contracts.ErrNotFound) {
			d, err := docs.CreateDocument(fctx, contracts.CreateDocumentRequest{Mutation: contracts.Mutation{OperationID: "op_fact-" + askID + "-create"},
				ProjectID: &pid, Title: contracts.FactSheetTitle, Slug: contracts.FactSheetSlug, Content: factSheetIntro + line, Summary: summary})
			if contracts.IsCode(err, contracts.ErrConflict) {
				continue // created meanwhile: append to it
			}
			if err != nil {
				return refused(err)
			}
			return h.setProposalTx(ctx, h.db, askID, contracts.AskProposalApplied, d.Revision.RevisionID, "")
		}
		if err != nil {
			return refused(err)
		}
		content := cur.Revision.Content
		if strings.Contains(content, askID) {
			return h.setProposalTx(ctx, h.db, askID, contracts.AskProposalApplied, cur.Revision.RevisionID, "")
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		d, err := docs.ProposeRevision(fctx, contracts.ProposeRevisionRequest{Mutation: contracts.Mutation{OperationID: "op_fact-" + askID + "-" + cur.Revision.RevisionID},
			DocumentID: cur.Document.DocumentID, BaseRevisionID: cur.Revision.RevisionID, Content: content + line, Summary: summary})
		if contracts.IsCode(err, contracts.ErrConflict) {
			continue // the sheet moved: append to the new head
		}
		if err != nil {
			return refused(err)
		}
		return h.setProposalTx(ctx, h.db, askID, contracts.AskProposalApplied, d.Revision.RevisionID, "")
	}
	return contracts.Errorf(contracts.ErrConflict, "the fact sheet kept changing; retrying later").WithRef(contracts.KindAsk, askID)
}

// proposalOutcome is what the hub did with a decided proposal, for the
// decision message ("" when the ask is not a proposal).
func proposalOutcome(a contracts.Ask) string {
	pr := a.Proposal
	if pr == nil {
		return ""
	}
	switch pr.State {
	case contracts.AskProposalApplied:
		if pr.Kind == contracts.ProposalGrant {
			return "; Pyre recorded standing grant " + pr.ResultID + " (do not record it yourself)"
		}
		return "; Pyre added the fact to the project's fact sheet"
	case contracts.AskProposalApproved:
		return "; Pyre is adding the fact to the project's fact sheet"
	case contracts.AskProposalFailed:
		return "; approved, but Pyre could not record it: " + oneLine(pr.Error, 200)
	case contracts.AskProposalRejected:
		return "; nothing was recorded"
	}
	if a.State.Decided() {
		return "; nothing was recorded"
	}
	return ""
}
