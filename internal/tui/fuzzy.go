package tui

import (
	"sort"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
)

// Fuzzy matching ported from junegunn/fzf's FuzzyMatchV2 (MIT license,
// src/algo/algo.go): a Smith-Waterman-style alignment with the same score
// constants, character classes and bonuses, so results order like fzf and
// Telescope's fzf sorter: matches at word starts, after delimiters (/ : ,),
// at camelCase/number boundaries and in consecutive runs win; gaps cost.
// Smart case: a pattern with an upper-case letter matches case-sensitively.

const (
	scoreMatch        = 16
	scoreGapStart     = -3
	scoreGapExtension = -1

	bonusBoundary            = scoreMatch / 2
	bonusNonWord             = scoreMatch / 2
	bonusCamel123            = bonusBoundary + scoreGapExtension
	bonusConsecutive         = -(scoreGapStart + scoreGapExtension)
	bonusFirstCharMultiplier = 2
	bonusBoundaryWhite       = bonusBoundary + 2
	bonusBoundaryDelimiter   = bonusBoundary + 1
)

type charClass int

const (
	charWhite charClass = iota
	charNonWord
	charDelimiter
	charLower
	charUpper
	charLetter
	charNumber
)

const delimiterChars = "/,:;|"

func classOf(r rune) charClass {
	switch {
	case unicode.IsLower(r):
		return charLower
	case unicode.IsUpper(r):
		return charUpper
	case unicode.IsDigit(r):
		return charNumber
	case unicode.IsLetter(r):
		return charLetter
	case unicode.IsSpace(r):
		return charWhite
	case strings.ContainsRune(delimiterChars, r):
		return charDelimiter
	}
	return charNonWord
}

func bonusFor(prev, cur charClass) int {
	if cur > charNonWord {
		switch prev {
		case charWhite:
			return bonusBoundaryWhite
		case charDelimiter:
			return bonusBoundaryDelimiter
		case charNonWord:
			return bonusBoundary
		}
	}
	if prev == charLower && cur == charUpper || prev != charNumber && cur == charNumber {
		return bonusCamel123
	}
	switch cur {
	case charNonWord, charDelimiter:
		return bonusNonWord
	case charWhite:
		return bonusBoundaryWhite
	}
	return 0
}

func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// fuzzyMatch aligns pattern within text. ok is false when pattern is not a
// subsequence of text. pos are rune indices of the matched characters.
func fuzzyMatch(pattern, text string, withPos bool) (score int, pos []int, ok bool) {
	P := []rune(pattern)
	if len(P) == 0 {
		return 0, nil, true
	}
	orig := []rune(text)
	T := orig
	if !hasUpper(pattern) {
		T = []rune(strings.ToLower(text))
		if len(T) != len(orig) { // case folding changed length: compare per rune
			T = make([]rune, len(orig))
			for i, r := range orig {
				T[i] = unicode.ToLower(r)
			}
		}
	}
	M, N := len(P), len(T)
	if M > N {
		return 0, nil, false
	}
	// F[i]: first index where P[i] can match after P[0..i-1].
	F := make([]int, M)
	pi := 0
	for j := 0; j < N && pi < M; j++ {
		if T[j] == P[pi] {
			F[pi] = j
			pi++
		}
	}
	if pi < M {
		return 0, nil, false
	}
	// last index of the final pattern char bounds the search
	last := N - 1
	for ; last >= 0; last-- {
		if T[last] == P[M-1] {
			break
		}
	}
	B := make([]int, N)
	prev := charWhite
	for j := 0; j < N; j++ {
		c := classOf(orig[j])
		B[j] = bonusFor(prev, c)
		prev = c
	}
	H := make([]int, M*N)
	C := make([]int, M*N)
	maxScore, maxPos := -1, -1
	// first row
	{
		prevH, inGap := 0, false
		for j := F[0]; j <= last; j++ {
			if T[j] == P[0] {
				s := scoreMatch + B[j]*bonusFirstCharMultiplier
				H[j], C[j] = s, 1
				if M == 1 && s > maxScore {
					maxScore, maxPos = s, j
				}
				prevH, inGap = s, false
			} else {
				if inGap {
					prevH = max(prevH+scoreGapExtension, 0)
				} else {
					prevH = max(prevH+scoreGapStart, 0)
				}
				H[j], C[j] = prevH, 0
				inGap = true
			}
		}
	}
	for i := 1; i < M; i++ {
		row := i * N
		inGap := false
		for j := F[i]; j <= last; j++ {
			var s1, s2, consecutive int
			left := 0
			if j > F[i] {
				left = H[row+j-1]
			}
			if inGap {
				s2 = left + scoreGapExtension
			} else {
				s2 = left + scoreGapStart
			}
			if T[j] == P[i] {
				diag := H[row-N+j-1]
				s1 = diag + scoreMatch
				b := B[j]
				consecutive = C[row-N+j-1] + 1
				if consecutive > 1 {
					fb := B[j-consecutive+1]
					if b >= bonusBoundary && b > fb {
						consecutive = 1
					} else {
						b = max(b, max(bonusConsecutive, fb))
					}
				}
				if s1+b < s2 {
					s1 += B[j]
					consecutive = 0
				} else {
					s1 += b
				}
			}
			C[row+j] = consecutive
			inGap = s1 < s2
			s := max(max(s1, s2), 0)
			H[row+j] = s
			if i == M-1 && T[j] == P[i] && s > maxScore {
				maxScore, maxPos = s, j
			}
		}
	}
	if maxPos < 0 {
		return 0, nil, false
	}
	if withPos {
		i, j := M-1, maxPos
		preferMatch := true
		for j >= 0 {
			row := i * N
			s := H[row+j]
			var s1, s2 int
			if i > 0 && j >= F[i] && j > 0 {
				s1 = H[row-N+j-1]
			}
			if j > F[i] {
				s2 = H[row+j-1]
			}
			if T[j] == P[i] && s > s1 && (s > s2 || s == s2 && preferMatch) {
				pos = append(pos, j)
				if i == 0 {
					break
				}
				preferMatch = C[row+j] > 1 || row+N+j+1 < len(C) && C[row+N+j+1] > 0
				i--
				j--
				continue
			}
			preferMatch = C[row+j] > 1 || row+N+j+1 < len(C) && C[row+N+j+1] > 0
			j--
		}
		sort.Ints(pos)
	}
	return maxScore, pos, true
}

// matchTerms matches every space-separated term of query against the
// fields (field 0 is the primary text, highlighted). A term may match any
// field; matches in secondary fields count half. Positions are for field 0.
func matchTerms(query string, fields ...string) (score int, pos []int, ok bool) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return 0, nil, true
	}
	seen := map[int]bool{}
	for _, t := range terms {
		best, bestPos, found := 0, []int(nil), false
		for fi, f := range fields {
			s, p, ok := fuzzyMatch(t, f, fi == 0)
			if !ok {
				continue
			}
			if fi > 0 {
				s /= 2
			}
			if !found || s > best {
				best, found = s, true
				bestPos = nil
				if fi == 0 {
					bestPos = p
				}
			}
		}
		if !found {
			return 0, nil, false
		}
		score += best
		for _, p := range bestPos {
			if !seen[p] {
				seen[p] = true
				pos = append(pos, p)
			}
		}
	}
	sort.Ints(pos)
	return score, pos, true
}

// filterMatch is the stricter test used to filter lists in place: each
// term must match one field as a substring or as a tight fuzzy match
// (score at least 20 per pattern character: word starts and runs, not scattered letters), so short queries do not
// match scattered letters across long IDs and paths.
func filterMatch(query string, fields ...string) (pos []int, ok bool) {
	terms := strings.Fields(query)
	seen := map[int]bool{}
	for _, t := range terms {
		found := false
		for fi, f := range fields {
			s, p, ok := fuzzyMatch(t, f, fi == 0)
			if !ok {
				continue
			}
			lt, lf := t, f
			if !hasUpper(t) {
				lf = strings.ToLower(f)
			}
			if !strings.Contains(lf, lt) && s < 20*len([]rune(t)) {
				continue
			}
			found = true
			if fi == 0 {
				for _, x := range p {
					if !seen[x] {
						seen[x] = true
						pos = append(pos, x)
					}
				}
			}
			break
		}
		if !found {
			return nil, false
		}
	}
	sort.Ints(pos)
	return pos, true
}

func filterOK(query string, fields ...string) bool {
	_, ok := filterMatch(query, fields...)
	return ok
}

// filterPos returns highlight positions of the filter within s.
func (m *Model) filterPos(s string) []int {
	if m.filter == "" {
		return nil
	}
	var pos []int
	for _, t := range strings.Fields(m.filter) {
		if p, ok := filterMatch(t, s); ok {
			pos = append(pos, p...)
		}
	}
	return pos
}

// titleCell pads a row's title to w with filter matches highlighted and
// bold when selected.
func (m *Model) titleCell(sel bool, title string, w int) string {
	base := lipgloss.NewStyle()
	if sel {
		base = m.th.selText
	}
	return m.highlight(pad(title, w), m.filterPos(title), base)
}

// highlight renders s with the matched rune positions in the match style,
// layered on base (so a bold selected row stays bold).
func (m *Model) highlight(s string, pos []int, base lipgloss.Style) string {
	if len(pos) == 0 {
		return base.Render(s)
	}
	hl := m.th.match.Inherit(base)
	set := map[int]bool{}
	for _, p := range pos {
		set[p] = true
	}
	var b strings.Builder
	var run []rune
	runHL := false
	flush := func() {
		if len(run) == 0 {
			return
		}
		if runHL {
			b.WriteString(hl.Render(string(run)))
		} else {
			b.WriteString(base.Render(string(run)))
		}
		run = run[:0]
	}
	for i, r := range []rune(s) {
		h := set[i]
		if h != runHL {
			flush()
			runHL = h
		}
		run = append(run, r)
	}
	flush()
	return b.String()
}
