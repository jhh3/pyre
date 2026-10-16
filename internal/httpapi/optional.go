package httpapi

import "github.com/jhh3/pyre/internal/contracts"

// addOptionalOps registers additive operations that are not part of
// contracts.API; an implementation offers them by implementing the
// operation's interface (an API without it answers 404 for the path).
func addOptionalOps(m map[string]rpcFunc, a contracts.API) {
	if s, ok := a.(contracts.SearchAPI); ok {
		m[contracts.OpSearch] = unary(s.Search)
	}
	if u, ok := a.(contracts.AccountUpdateAPI); ok {
		m[contracts.OpUpdateAccount] = unary(u.UpdateAccount)
	}
	if t, ok := a.(contracts.AccountTokenAPI); ok {
		m[contracts.OpSetAccountToken] = unary(t.SetAccountToken)
		m[contracts.OpRemoveAccountToken] = unary(t.RemoveAccountToken)
		m[contracts.OpListAccountTokens] = unary(t.ListAccountTokens)
	}
	if r, ok := a.(contracts.ReposAPI); ok { // W-quicklaunch
		m[contracts.OpListRepos] = unary(r.ListRepos)
		m[contracts.OpSessionWorktrees] = unary(r.SessionWorktrees)
	}
	if r, ok := a.(contracts.RepoCatalogAPI); ok { // W-newsession
		m[contracts.OpRepoCatalog] = unary(r.RepoCatalog)
	}
	if au, ok := a.(contracts.AuditAPI); ok { // W-audit
		m[contracts.OpListAudit] = unary(au.ListAudit)
	}
	if r, ok := a.(contracts.MessageReadAPI); ok { // W-delivery: read = delivered
		m[contracts.OpMarkMessagesRead] = unary(r.MarkMessagesRead)
	}
	if w, ok := a.(contracts.InboxWaitAPI); ok { // W-localwait: node-local doorbell wait
		m[contracts.OpInboxWait] = unary(w.InboxWait)
	}
	if s, ok := a.(contracts.SessionBatchAPI); ok { // W-delta
		m[contracts.OpGetManySessions] = unary(s.GetManySessions)
	}
	if k, ok := a.(contracts.AsksAPI); ok { // W-asks
		m[contracts.OpAsksCreate] = unary(k.CreateAsk)
		m[contracts.OpAsksList] = unary(k.ListAsks)
		m[contracts.OpAsksGet] = unary(k.GetAsk)
		m[contracts.OpAsksAnswer] = unary(k.AnswerAsks)
		m[contracts.OpAsksWithdraw] = unary(k.WithdrawAsk)
	}
	if k, ok := a.(contracts.HeldAnswersAPI); ok { // W-web-inbox: held (batched) answers
		m[contracts.OpAsksHold] = unary(k.HoldAnswer)
		m[contracts.OpAsksUnhold] = unary(k.UnholdAnswer)
		m[contracts.OpAsksHeld] = unary(k.ListHeldAnswers)
		m[contracts.OpAsksFlush] = unary(k.FlushHeldAnswers)
	}
	if pg, ok := a.(contracts.ProjectPageAPI); ok { // W-web-inbox: project pages
		m[contracts.OpProjectsSetPage] = unary(pg.SetProjectPage)
		m[contracts.OpProjectsGetPage] = unary(pg.GetProjectPage)
	}
	if f, ok := a.(contracts.AccountTokenFetcher); ok {
		m[contracts.OpFetchAccountToken] = unary(f.FetchAccountToken)
	}
}
