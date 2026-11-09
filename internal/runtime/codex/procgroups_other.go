//go:build !linux && !darwin

package codex

// groupMembers is not available here (on Windows Codex runs under
// termhost, whose stop is unchanged): automatic stops kill what explicit
// ones do.
func groupMembers(pgids []int) (map[int][]procMember, bool) { return nil, false }
