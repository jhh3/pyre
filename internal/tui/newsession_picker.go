package tui

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// The folder picker of the new-session panel: "📁 Browse folders…" (or a
// path typed in Where) opens it on the chosen machine, browsing that
// machine's filesystem through dir.list. The input is a path: Tab
// completes its last segment against the folder's listing (a unique
// prefix in full, else the common part, listing the choices); ~ expands
// to the machine's home, / at the start of a segment goes to the root.
// ↑/↓ move, → or Enter on a folder goes in, ← (or Backspace on an empty
// segment) goes up. Enter on "✓ Use this folder" chooses the folder; ^s
// or ^⏎ chooses it and starts. With an empty segment the list also offers
// home, the machine's worktree root and its recent folders. Only folders
// show; dot-folders only when the segment starts with ".". Each listing
// is fetched once per panel (a spinner while it loads); Esc returns to
// the Where list.

const browseKey = "browse"

// Picker row kinds.
const (
	pkUse = iota
	pkGo
	pkDir
)

type nsPicker struct {
	node  string
	input string // a folder and an optional last segment: "~/src/ap", "~/", "/", "C:/Users/"
	hl    string // highlighted row key ("" = the first match, else Use)
	moved bool   // the owner moved the highlight (↑/↓): ^s takes it
	note  string // Tab's choices, or why it did nothing
	// win: the machine is Windows (its platform, or a drive-letter home).
	// Paths are kept with forward slashes there; "/" goes to the drive root.
	win   bool
	drive string // the drive root "/" goes to on Windows ("C:/")
}

type pkRow struct {
	kind  int
	key   string
	label string
	path  string
	note  string
	isNew bool // pkUse: the folder does not exist yet (created first)
	warn  bool // pkUse: note is a warning
}

// isDrive reports a Windows drive path: "C:", "C:/…" or `C:\…`.
func isDrive(s string) bool {
	if len(s) < 2 || s[1] != ':' {
		return false
	}
	c := s[0] | 0x20
	return c >= 'a' && c <= 'z' && (len(s) == 2 || s[2] == '/' || s[2] == '\\')
}

// cleanDir normalises a folder: "~", "~/x", "/x", "C:/x"; roots "/" and
// "C:/" keep their slash.
func cleanDir(d string) string {
	switch {
	case d == "" || d == "/":
		return "/"
	case d == "~":
		return "~"
	case strings.HasPrefix(d, "~/"):
		if c := path.Clean("/" + d[2:]); c != "/" {
			return "~" + c
		}
		return "~"
	case isDrive(d):
		return d[:2] + path.Clean("/"+strings.TrimLeft(d[2:], "/"))
	}
	return path.Clean(d)
}

// pickerSplit is the input's folder and last segment.
func pickerSplit(s string) (dir, seg string) {
	switch {
	case s == "" || s == "~":
		return "~", ""
	case isDrive(s) && len(s) == 2:
		return s + "/", ""
	case !strings.Contains(s, "/"):
		return "~", s
	}
	i := strings.LastIndexByte(s, '/')
	return cleanDir(s[:i+1]), s[i+1:]
}

// pickerDirInput is the input for being in folder dir.
func pickerDirInput(dir string) string {
	if strings.HasSuffix(dir, "/") {
		return dir
	}
	return dir + "/"
}

// joinDir appends a child folder name.
func joinDir(dir, name string) string { return pickerDirInput(dir) + name }

// parentDir is dir's parent and dir's own name; ok is false at a root.
func parentDir(dir string) (parent, name string, ok bool) {
	dir = cleanDir(dir)
	if dir == "/" || dir == "~" || (isDrive(dir) && len(dir) == 3) {
		return dir, "", false
	}
	i := strings.LastIndexByte(dir, '/')
	switch {
	case i < 0:
		return dir, "", false
	case i == 0:
		return "/", dir[1:], true
	case isDrive(dir) && i == 2:
		return dir[:3], dir[3:], true
	}
	return dir[:i], dir[i+1:], true
}

// pickKey is a listing's cache key; dot-folders are a separate listing,
// fetched only for a segment starting with "." (so they never crowd the
// node's listing cap).
func (q *nsPanel) pickKey(node, dir string, hidden bool) string {
	k := "nsdir:" + q.instance + ":" + node + ":" + dir
	if hidden {
		k += ":hidden"
	}
	return k
}

// dropPickerCache forgets every listing of an earlier panel (each panel
// caches its own, for its lifetime).
func (m *Model) dropPickerCache(keep string) {
	for k := range m.loads {
		if strings.HasPrefix(k, "nsdir:") && !strings.HasPrefix(k, "nsdir:"+keep+":") {
			delete(m.loads, k)
		}
	}
}

// openPicker opens the picker on the plan's machine at spec ("" = home).
// On this machine ./ and ../ resolve against the TUI's directory; another
// machine has none, so they start at its home.
func (q *nsPanel) openPicker(m *Model, p nsPlan, spec string) tea.Cmd {
	local := p.nodeID != "" && p.nodeID == m.opts.ClientNodeID
	input := "~/"
	switch s := strings.TrimSpace(spec); {
	case s == "":
	case isDrive(s):
		input = strings.ReplaceAll(s, `\`, "/")
		if len(input) == 2 {
			input += "/"
		}
	default:
		cwd, home := q.cwd, q.home
		if !local {
			cwd, home = "/", ""
			switch {
			case s == "." || s == "..":
				s = "~"
			case strings.HasPrefix(s, "./"):
				s = "~/" + s[2:]
			case strings.HasPrefix(s, "../"):
				s = "~/" + s[3:]
			}
		}
		pth, dir := repoindex.NormPath(s, cwd, home)
		input = pth
		if dir || s == "." || s == ".." || pth == "~" || strings.HasSuffix(s, "/.") || strings.HasSuffix(s, "/..") {
			input = pickerDirInput(pth)
		}
	}
	q.picker = &nsPicker{node: p.nodeID, input: input}
	q.focus = nsWhere
	q.syncPicker(m)
	return q.loadPicker(m)
}

// syncPicker refreshes what the picker knows about its machine (Windows,
// its drive root).
func (q *nsPanel) syncPicker(m *Model) {
	b := q.picker
	home, _ := q.pickerHome(m)
	n := m.node(b.node)
	b.win = (n != nil && n.Platform == "windows") || isDrive(home) || isDrive(b.input)
	b.drive = "C:/"
	switch {
	case isDrive(b.input):
		b.drive = strings.ToUpper(b.input[:1]) + ":/"
	case isDrive(home):
		b.drive = strings.ToUpper(home[:1]) + ":/"
	}
}

// listing is the cached dir.list of folder dir (dot-folders too when
// hidden) on the picker's machine.
func (q *nsPanel) listing(m *Model, dir string, hidden bool) (contracts.DirListResult, *loadState) {
	return get[contracts.DirListResult](m, q.pickKey(q.picker.node, dir, hidden))
}

// current is the listing the input selects from.
func (q *nsPanel) current(m *Model) (dir, seg string, res contracts.DirListResult, st *loadState) {
	dir, seg = pickerSplit(q.picker.input)
	res, st = q.listing(m, dir, strings.HasPrefix(seg, "."))
	return dir, seg, res, st
}

// pickerLoading: no listing yet, and one is (or will be) on its way,
// including a retry after a failure.
func pickerLoading(st *loadState) bool {
	return st == nil || (!st.have && (st.err == nil || st.inflight))
}

// listFailed: the listing never landed and its last attempt failed.
func listFailed(st *loadState) bool { return st != nil && !st.have && st.err != nil && !st.inflight }

// notThere: the node said the folder does not exist (evidence for "new").
func notThere(err error) bool {
	return contracts.IsCode(err, contracts.ErrInvalid) && strings.Contains(errText(err), "does not exist")
}

// transient: a failure worth retrying (a timeout, an unknown outcome, an
// unreachable node), not an answer about the folder.
func transient(err error) bool {
	return err != nil && !contracts.IsCode(err, contracts.ErrInvalid) && !contracts.IsCode(err, contracts.ErrForbidden)
}

func dirOf(input string) string {
	d, _ := pickerSplit(input)
	return d
}

// pickerOffline: the picker's machine cannot be listed (nothing loads).
func (q *nsPanel) pickerOffline(m *Model) bool {
	n := m.node(q.picker.node)
	return n == nil || n.Connection.State != contracts.ConnConnected
}

// pickerKnown reports whether Use can be taken: the listing landed or
// failed (then Use starts there unverified), or the machine is offline
// (starting refuses).
func (q *nsPanel) pickerKnown(m *Model) bool {
	_, _, _, st := q.current(m)
	return !pickerLoading(st) || q.pickerOffline(m)
}

// pickerWaiting: nothing to show yet but something may change by itself
// (the machine reconnects, a transient failure clears): keep polling.
func (q *nsPanel) pickerWaiting(m *Model) bool {
	if q.pickerOffline(m) {
		return true
	}
	_, _, _, st := q.current(m)
	return listFailed(st) && transient(st.err)
}

// loadPicker fetches the current folder's listing (and home's, for its
// path and shortcuts) unless this panel has it; a spinner turns meanwhile.
// An offline machine is polled (slowly) until it returns.
func (q *nsPanel) loadPicker(m *Model) tea.Cmd {
	b := q.picker
	if b == nil {
		return nil
	}
	if q.pickerOffline(m) {
		if !q.spinning {
			q.spinning = true
			return q.spinAfter(time.Second)
		}
		return nil
	}
	dir, seg := pickerSplit(b.input)
	type want struct {
		dir    string
		hidden bool
	}
	wants := []want{{dir, false}, {"~", false}}
	if strings.HasPrefix(seg, ".") {
		wants = append(wants, want{dir, true})
	}
	var cmds []tea.Cmd
	for _, w := range wants {
		api, nid, w := m.api, b.node, w
		cmds = append(cmds, m.ensure(q.pickKey(nid, w.dir, w.hidden), 0, func(ctx context.Context) (any, error) {
			var out contracts.DirListResult
			err := nsInvoke(ctx, api, nid, contracts.NewID(contracts.PrefixOperation), contracts.ActionDirList, contracts.DirListParams{Path: w.dir, Hidden: w.hidden}, &out, 5)
			return out, err
		}))
	}
	if _, _, _, st := q.current(m); !q.spinning && (pickerLoading(st) || q.pickerWaiting(m)) {
		q.spinning = true
		d := 120 * time.Millisecond
		if !pickerLoading(st) {
			d = time.Second // a transient failure: retry
		}
		cmds = append(cmds, q.spinAfter(d))
	}
	return tea.Batch(cmds...)
}

type nsSpinMsg struct{ q *nsPanel }

func (q *nsPanel) spinAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return nsSpinMsg{q: q} })
}

// onSpin is the picker's tick: animate while loading, relist when the
// machine reconnects or a transient failure may have cleared.
func (q *nsPanel) onSpin(m *Model) tea.Cmd {
	q.spinning = false
	if q.picker == nil || m.overlay != q {
		return nil
	}
	if q.pickerOffline(m) {
		return q.loadPicker(m) // polls on
	}
	// ensure retries a failed listing once its failure is 2s old.
	return q.loadPicker(m)
}

// pickerHome is the machine's home and worktree root, from any listing
// (forward slashes on Windows).
func (q *nsPanel) pickerHome(m *Model) (home, root string) {
	dir, _ := pickerSplit(q.picker.input)
	for _, d := range []string{dir, "~"} {
		res, st := q.listing(m, d, false)
		if st == nil || !st.have {
			continue
		}
		if home == "" {
			home = res.Home
			if home == "" && d == "~" && (strings.HasPrefix(res.Path, "/") || isDrive(res.Path)) {
				home = res.Path // an older pyred: "~" listed as its absolute path
			}
		}
		if root == "" {
			root = res.WorktreeRoot
		}
	}
	if root == "" && q.picker.node == m.opts.ClientNodeID {
		root = os.Getenv("PYRE_WORKTREE_ROOT")
	}
	if isDrive(home) {
		home = strings.ReplaceAll(home, `\`, "/")
	}
	if isDrive(root) {
		root = strings.ReplaceAll(root, `\`, "/")
	}
	return home, root
}

// pickerMatches are the folder's children the segment selects: prefix
// matches (any case), dot-folders only for a segment starting with ".".
func pickerMatches(dirs []string, seg string) []string {
	ls := strings.ToLower(seg)
	var out []string
	for _, d := range dirs {
		if strings.HasPrefix(d, ".") && !strings.HasPrefix(seg, ".") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(d), ls) {
			out = append(out, d)
		}
	}
	return out
}

// pickerRows: Use this folder, shortcuts (empty segment), the matches.
func (q *nsPanel) pickerRows(m *Model) []pkRow {
	b := q.picker
	dir, seg, res, st := q.current(m)
	use := pkRow{kind: pkUse, key: "use", label: "✓ Use this folder", path: dir}
	// New only on evidence: a listing without the name, or the node saying
	// the folder does not exist. While its listing loads it is unknown (Use
	// waits); a listing that failed otherwise (a timeout, no permission)
	// says nothing about it: Use starts there unverified. A failed reload
	// keeps the earlier listing.
	failed := listFailed(st)
	gone := failed && notThere(st.err)
	switch {
	case seg != "":
		use.path = joinDir(dir, seg)
		use.label = "✓ Use " + use.path
		found := false
		for _, d := range res.Dirs {
			found = found || d == seg
		}
		use.isNew = gone || (st != nil && st.have && !found)
	default:
		use.isNew = gone && dir != "~" && dir != "/" && !(isDrive(dir) && len(dir) == 3)
	}
	switch {
	case use.isNew:
		use.note = "new folder, created first"
	case failed:
		use.note, use.warn = "not verified (the listing failed): Use starts there", true
	}
	rows := []pkRow{use}
	if seg == "" {
		home, root := q.pickerHome(m)
		seen := map[string]bool{dir: true}
		add := func(label, p string) {
			if p == "" || seen[p] || seen[tildeUnder(p, home)] {
				return
			}
			seen[p] = true
			rows = append(rows, pkRow{kind: pkGo, key: "go:" + p, label: label, path: p})
		}
		add("⌂ home", "~")
		if root != "" {
			add("⑂ worktrees", tildeUnder(root, home))
		}
		n := 0
		for _, r := range m.nsRecent(nil, b.node) {
			if n >= 3 {
				break
			}
			if p := tildeUnder(r.path, home); !seen[p] && !seen[r.path] {
				add("↺ "+r.name, p)
				n++
			}
		}
	}
	for _, d := range pickerMatches(res.Dirs, seg) {
		rows = append(rows, pkRow{kind: pkDir, key: "dir:" + d, label: d, path: joinDir(dir, d)})
	}
	return rows
}

// tildeUnder writes p with ~ when it lies in home.
func tildeUnder(p, home string) string {
	if home != "" && home != "/" {
		h := strings.TrimSuffix(home, "/")
		if p == h {
			return "~"
		}
		if strings.HasPrefix(p, h+"/") {
			return "~" + p[len(h):]
		}
	}
	return p
}

// pickerSel is the highlighted row: the one named, else the first match
// for a typed segment, else Use.
func (q *nsPanel) pickerSel(rows []pkRow) int {
	b := q.picker
	for i, r := range rows {
		if r.key == b.hl {
			return i
		}
	}
	if _, seg := pickerSplit(b.input); seg != "" {
		for i, r := range rows {
			if r.kind == pkDir {
				return i
			}
		}
	}
	return 0
}

// pickerTarget is what ^s / Start takes: the highlighted row when the
// owner moved to it or it is exactly what was typed, else Use (the typed
// path, created when missing).
func (q *nsPanel) pickerTarget(rows []pkRow, sel int) pkRow {
	r := rows[sel]
	_, seg := pickerSplit(q.picker.input)
	if r.kind != pkUse && (q.picker.moved || (r.kind == pkDir && strings.EqualFold(r.label, seg))) {
		return r
	}
	return rows[0]
}

// pickerGo moves into folder p.
func (b *nsPicker) goTo(p string) {
	b.input, b.hl, b.moved, b.note = pickerDirInput(p), "", false, ""
}

// pickerUp goes to the parent folder, the one left highlighted (from ~:
// the parent of the machine's home). Roots stay.
func (q *nsPanel) pickerUp(m *Model) {
	b := q.picker
	dir, _ := pickerSplit(b.input)
	from := dir
	if dir == "~" {
		home, _ := q.pickerHome(m)
		if home == "" {
			return
		}
		from = home
	}
	parent, name, ok := parentDir(from)
	if !ok {
		b.input, b.note = pickerDirInput(dir), ""
		return
	}
	b.input, b.hl, b.moved, b.note = pickerDirInput(parent), "dir:"+name, false, ""
}

// pickerComplete is Tab: a unique match is entered; several extend the
// segment to their common prefix and are listed.
func (q *nsPanel) pickerComplete(m *Model) {
	b := q.picker
	dir, seg, res, st := q.current(m)
	if st == nil || !st.have {
		b.note = "still listing " + dir + "…"
		return
	}
	ms := pickerMatches(res.Dirs, seg)
	switch len(ms) {
	case 0:
		b.note = "no folder here starts with “" + seg + "”"
		if seg == "" {
			b.note = "no folders in " + dir
		}
		if strings.HasPrefix(seg, ".") && !res.Hidden {
			b.note += " (this machine's pyred does not list dot-folders: type the name and /)"
		}
		return
	case 1:
		b.goTo(joinDir(dir, ms[0]))
		return
	}
	lcp := ms[0]
	for _, s := range ms[1:] {
		for !strings.HasPrefix(strings.ToLower(s), strings.ToLower(lcp)) {
			lcp = lcp[:len(lcp)-1]
		}
	}
	if len(lcp) > len(seg) {
		b.input, b.hl, b.moved = pickerDirInput(dir)+lcp, "", false
	}
	shown := ms
	if len(shown) > 6 {
		shown = shown[:6]
	}
	b.note = strconv.Itoa(len(ms)) + " folders: " + strings.Join(shown, "  ")
	if len(ms) > len(shown) {
		b.note += "  …"
	}
}

// typeText adds typed text to the input: ~ or / at the start of a
// segment jump to home or the root (the drive root on Windows, where a
// backslash is a slash).
func (b *nsPicker) typeText(s string) {
	for _, r := range s {
		if r == '\\' && b.win {
			r = '/'
		}
		_, seg := pickerSplit(b.input)
		switch {
		case r == '~' && seg == "":
			b.input = "~/"
		case r == '/' && seg == "" && b.input != "~/":
			b.input = "/"
			if b.win {
				b.input = b.drive
			}
		case r == '/' && seg == "":
			// "~/" just typed: the slash is already there.
		case r == ':' && b.win && len(seg) == 1 && isDrive(seg+":"):
			b.input = strings.ToUpper(seg) + ":/" // C: is that drive's root
		default:
			b.input += string(r)
		}
	}
	b.hl, b.moved, b.note = "", false, ""
}

// pickerPaste adds pasted text: an escaped or quoted path is unescaped,
// except on Windows (or a drive path), where backslashes separate.
func (q *nsPanel) pickerPaste(m *Model, text string) {
	q.syncPicker(m)
	line := strings.NewReplacer("\r", "", "\n", "").Replace(text)
	switch t := strings.TrimSpace(line); {
	case q.picker.win || isDrive(t):
		line = strings.ReplaceAll(t, `\`, "/")
		if isDrive(line) {
			q.picker.win = true
			q.picker.input = ""
			if len(line) == 2 {
				line += "/"
			}
		}
	case strings.ContainsAny(line, `\'"`):
		tg, rest, _ := repoindex.SplitTarget(line)
		line = strings.Join(append([]string{tg}, rest...), " ")
	}
	if q.picker.input == "" {
		q.picker.input, q.picker.hl, q.picker.moved, q.picker.note = line, "", false, ""
		return
	}
	q.picker.typeText(line)
}

// choosePicked sets Where to the folder on the picker's machine.
func (q *nsPanel) choosePicked(m *Model, r pkRow) tea.Cmd {
	b := q.picker
	q.picker = nil
	q.machine, q.folder, q.folderNew = b.node, r.path, r.isNew
	q.query, q.hlKey, q.cloning, q.extra = "", nsPickedKey, false, nil
	q.focus = nsAgent
	q.errLine = ""
	return q.load(m)
}

// pickerKeyMsg handles every key while the picker is open.
func (q *nsPanel) pickerKeyMsg(m *Model, k tea.KeyPressMsg) tea.Cmd {
	b := q.picker
	key := k.String()
	rows := q.pickerRows(m)
	sel := q.pickerSel(rows)
	move := func(d int) {
		b.hl, b.moved, b.note = rows[min(max(sel+d, 0), len(rows)-1)].key, true, ""
	}
	q.syncPicker(m)
	switch key {
	case "esc":
		q.picker = nil
		return nil
	case "ctrl+c":
		m.overlay = nil
		return nil
	case "ctrl+s", "ctrl+enter":
		return q.pickerStart(m)
	case "tab":
		q.pickerComplete(m)
	case "shift+tab":
		q.picker = nil
		q.move(-1)
		return nil
	case "up", "ctrl+p", "ctrl+k":
		move(-1)
	case "down", "ctrl+n", "ctrl+j":
		move(1)
	case "pgup":
		move(-8)
	case "pgdown":
		move(8)
	case "right":
		if r := rows[sel]; r.kind != pkUse {
			b.goTo(r.path)
		}
	case "enter":
		if r := rows[sel]; r.kind == pkUse {
			if !q.pickerKnown(m) {
				b.note = "still listing " + dirOf(b.input) + "…: try again in a moment"
				return q.loadPicker(m)
			}
			return q.choosePicked(m, r)
		}
		b.goTo(rows[sel].path)
	case "left":
		q.pickerUp(m)
	case "backspace":
		if _, seg := pickerSplit(b.input); seg == "" {
			q.pickerUp(m)
		} else {
			r := []rune(b.input)
			b.input, b.hl, b.moved, b.note = string(r[:len(r)-1]), "", false, ""
		}
	case "ctrl+u", "ctrl+w":
		if dir, seg := pickerSplit(b.input); seg != "" {
			b.goTo(dir)
		} else if key == "ctrl+w" {
			q.pickerUp(m)
		}
	case "space":
		b.typeText(" ")
	default:
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			b.typeText(k.Text)
		}
	}
	return q.loadPicker(m)
}

// pickerStart is ^s / ^⏎ / Start while the picker is open: use its
// target (pickerTarget) and start.
func (q *nsPanel) pickerStart(m *Model) tea.Cmd {
	rows := q.pickerRows(m)
	r := q.pickerTarget(rows, q.pickerSel(rows))
	if r.kind == pkUse && !q.pickerKnown(m) {
		q.picker.note = "still listing " + dirOf(q.picker.input) + "…: try again in a moment"
		return q.loadPicker(m)
	}
	cmd := q.choosePicked(m, r)
	return tea.Batch(cmd, q.start(m, q.plan(m)))
}

func (q *nsPanel) pickerClick(m *Model, val string) tea.Cmd {
	if q.picker == nil {
		return nil
	}
	rows := q.pickerRows(m)
	i, _ := strconv.Atoi(val)
	if i < 0 || i >= len(rows) {
		return nil
	}
	if rows[i].kind == pkUse {
		if !q.pickerKnown(m) {
			q.picker.note = "still listing " + dirOf(q.picker.input) + "…: try again in a moment"
			return q.loadPicker(m)
		}
		return q.choosePicked(m, rows[i])
	}
	q.picker.goTo(rows[i].path)
	return q.loadPicker(m)
}

func (q *nsPanel) pickerHints() []hint {
	return []hint{{"tab", "complete"}, {"↑↓", "move"}, {"→", "in"}, {"←", "up"}, {"enter", "in / use"}, {"^s", "use + start"}, {"esc", "back"}}
}

// pickerInput is the Where line while the picker is open.
func (q *nsPanel) pickerInput(m *Model) string {
	return m.th.dim.Render("📁 ") + q.picker.input + m.th.accent.Render("▏") + m.th.dim.Render("  on "+m.nodeName(q.picker.node))
}

// pickerLines renders the picker in the Where list's place.
func (q *nsPanel) pickerLines(m *Model, width_, listH int) []string {
	b := q.picker
	dir, seg, res, st := q.current(m)
	rows := q.pickerRows(m)
	sel := q.pickerSel(rows)
	head := m.th.label.Render("in ") + m.th.key.Render(dir) + m.th.dim.Render(" on "+m.nodeName(b.node))
	if home, _ := q.pickerHome(m); home != "" && strings.HasPrefix(dir, "~") {
		head += m.th.faint.Render("  (~ is " + home + ")")
	}
	list := []string{trunc(head, width_)}
	selLine := 0
	labelW := 20
	for i, r := range rows {
		on := i == sel
		var s string
		switch r.kind {
		case pkUse:
			s = m.th.key.Render(r.label)
			if seg == "" {
				s += "  " + m.th.faint.Render(r.path)
			}
			switch {
			case r.isNew:
				s += "  " + m.th.tone(toneWarn, r.note)
			case r.note != "":
				s += "  " + m.th.dim.Render(r.note)
			}
		case pkGo:
			home, _ := q.pickerHome(m)
			s = pad(styleIf(on, m.th.selText).Render(trunc(r.label, labelW-1)), labelW) + m.th.faint.Render(tildeUnder(r.path, home))
		default:
			var pos []int
			for j := range []rune(seg) {
				pos = append(pos, j)
			}
			s = m.highlight(r.label, pos, styleIf(on, m.th.selText)) + m.th.faint.Render("/")
		}
		if on {
			selLine = len(list)
		}
		list = append(list, m.mark(zoneDef{kind: zoneOverlay, name: "pick:" + strconv.Itoa(i)}, m.rowSel(on && q.focus == nsWhere, trunc(m.cursor(on)+s, width_), width_)))
	}
	var tail string
	switch {
	case b.note != "":
		tail = m.th.dim.Render("  " + b.note)
	case q.pickerOffline(m):
		tail = m.th.tone(toneWarn, "  "+m.nodeName(b.node)+" is offline: its folders cannot be listed")
	case pickerLoading(st):
		frame := gridSpinner[int(time.Now().UnixMilli()/120)%len(gridSpinner)]
		tail = m.th.dim.Render("  " + frame + " listing " + dir + "…")
	case listFailed(st) && transient(st.err):
		tail = m.th.tone(toneWarn, "  cannot list "+dir+": "+errText(st.err)+" · retrying")
	case st != nil && st.err != nil && !st.inflight && !st.have:
		tail = m.th.tone(toneWarn, "  cannot list "+dir+": "+errText(st.err)+" (← goes up)")
	case strings.HasPrefix(seg, ".") && !res.Hidden:
		tail = m.th.dim.Render("  this machine's pyred does not list dot-folders (upgrade it): type the name and /")
	case len(res.Dirs) == 0:
		tail = m.th.dim.Render("  no folders in " + dir)
	case len(pickerMatches(res.Dirs, seg)) == 0 && seg != "":
		tail = m.th.dim.Render("  no folder starts with “" + seg + "”: ✓ Use creates it")
	}
	if tail != "" {
		list = append(list, trunc(tail, width_))
	}
	if len(list) > listH {
		top := 0
		if selLine >= listH-1 {
			top = selLine - listH + 2
		}
		top = min(top, len(list)-listH)
		list = append([]string{list[0]}, list[top+1:top+listH]...)
	}
	for len(list) < listH {
		list = append(list, "")
	}
	return list
}
