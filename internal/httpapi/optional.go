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
	if au, ok := a.(contracts.AuditAPI); ok { // W-audit
		m[contracts.OpListAudit] = unary(au.ListAudit)
	}
	if f, ok := a.(contracts.AccountTokenFetcher); ok {
		m[contracts.OpFetchAccountToken] = unary(f.FetchAccountToken)
	}
}
