package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// viewID names a screen.
type viewID int

const (
	viewSessions viewID = iota
	viewProjects
	viewDigest // "While you were away" (W-digest); the Inbox view is gone (W-attn)
	viewProject
	viewArtifact
	viewDoc
	viewMessages
	viewMachines
	viewAccounts
	viewConnections
	viewEnvs
	viewLaunch
	viewReader
	viewGrid
	viewActivity
)

func (v viewID) topLevel() bool {
	return v == viewSessions || v == viewProjects || v == viewGrid
}

func (v viewID) name() string {
	return [...]string{"Sessions", "Projects", "While you were away", "Project", "Artifact", "Document", "Messages",
		"Machines", "Accounts", "Connections", "Environments & updates", "Launch", "Transcript", "Grid", "Activity"}[v]
}

// loaderFn performs one bounded API read.
type loaderFn func(ctx context.Context) (any, error)

// loadState tracks one cached, asynchronously loaded value.
type loadState struct {
	fn       loaderFn
	val      any
	have     bool
	at       time.Time // clock time of the last successful load
	err      error
	errAt    time.Time
	inflight bool
	started  time.Time // wall time the in-flight load started (latency, not a fact age)
	dirty    bool      // reload once the in-flight load completes
	stale    bool      // invalidated by an event; reload when next needed
	seq      int
}

type loadedMsg struct {
	key string
	seq int
	val any
	err error
}

type resultMsg struct {
	val  any
	err  error
	then func(m *Model, val any, err error) tea.Cmd
}

type refreshTickMsg struct{}
type debounceMsg struct{}
type previewDueMsg struct{ token int }
type clearToastMsg struct{ token int }

type toast struct {
	text  string
	tone  tone
	token int
}

// Model is the root Bubble Tea model. It is driven only through Update;
// commands never mutate it from other goroutines.
type Model struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    contracts.API
	opts   Options
	th     theme

	width, height int

	view viewID
	back []viewID

	loads map[string]*loadState

	// lists keyed by view/context
	sessList listState
	projList listState
	lists    map[string]*listState
	sessTop  int // scroll offset of the grouped session list (display lines)

	filter     string // committed session/project filter
	filtering  bool
	filterIn   textinput.Model
	overlay    overlay
	toast      *toast
	toastSeq   int
	previewTok int

	// event handling
	ev evState

	// navigation context
	curProject  string
	projTab     int
	curArtifact string
	curDoc      string
	curDocRev   string
	docScroll   int
	docRevs     bool
	curMsgSess  string
	mgmtList    listState

	// interaction memory: stable operation IDs per logical operation so a
	// repeated keypress or retry never becomes a new operation.
	opIDs map[string]string
	busy  map[string]bool

	launch   *launchState
	stats    Stats
	draftMap map[string]string

	answerResult map[string]string
	pendingFocus string // session to select once it appears (after launch)

	defaultAttach bool

	// keyboard
	km         *Keymap
	pending    []string // keys of an unfinished sequence (leader, g…)
	pendingTok int
	whichKey   bool // leader popup visible
	focus      int  // 0: list pane, 1: detail pane
	detailOff  int  // detail pane scroll offset when focused
	cmdHist    []string
	msgHist    []string
	reader     readerState
	boxAt      *boxGeom // geometry of the panel drawn by the current overlay (render only)

	// mouse: regions marked while rendering, and where the last frame drew them
	zdefs     []zoneDef
	frameDefs []zoneDef
	zhits     []zoneHit
	termDrag  string // session whose terminal got the last mouse press (until release)
	// mouse selection and copy (termselect.go)
	termPress     *termPress
	lastTermClick lastClick
	framePress    *framePress
	frameSel      cellSel
	lastFrame     string // the last frame drawn, markers stripped

	// first-launch tips bar (dismissed state lives in Options.StateFile)
	showTips bool

	// Sessions list: ended (exited/failed) sessions are hidden unless
	// showEnded (remembered in Options.StateFile); archiveAfterStop holds
	// sessions to archive once their stop is observed.
	showEnded        bool
	archiveAfterStop map[string]bool
	wtOffer          *wtCleanup // the last archive's worktree offer, opened with W (worktrees.go)
	// showUsage adds the memory/CPU column to the Sessions list (u).
	showUsage bool

	// embedded session terminals (embed.go)
	term *termMgr
	// switching (switch.go): the session whose terminal has the keyboard
	// before its lease is taken (focus followed a switch), and the viewed
	// sessions for alt+left and the switcher's recent-first order.
	termLazy          string
	viewCur, viewPrev string
	mru               []string

	// mission-control grid of live tiles (grid.go)
	grid gridState
	// Sessions list grouping and folded groups (grouping.go)
	grouping groupingState
	tree     treeState // the Sessions list's session trees (tree.go)
	// Sessions view: sidenav, table or grid (sessions_modes.go)
	sessView sessViewState
	dg       digestState // "while you were away" panel (digest.go)

	act activityState // Activity filter (activity.go)
}

// Stats exposes counters for tests and diagnostics.
type Stats struct {
	Events       int // events received from the watcher
	Refreshes    int // debounced refresh passes triggered by events
	Loads        int // API loads started
	DroppedLoads int // stale responses discarded
}

// New builds a model. Run uses it; tests drive it directly.
func New(ctx context.Context, api contracts.API, opts Options) *Model {
	defaultAttach := opts.Attach == nil
	opts = opts.withDefaults()
	cctx, cancel := context.WithCancel(ctx)
	fi := textinput.New()
	fi.Prompt = "/"
	fi.Placeholder = "filter sessions and projects"
	fi.CharLimit = 120
	m := &Model{
		ctx: cctx, cancel: cancel, api: api, opts: opts,
		th:    initialTheme(opts),
		width: 100, height: 30,
		loads:    map[string]*loadState{},
		lists:    map[string]*listState{},
		opIDs:    map[string]string{},
		busy:     map[string]bool{},
		filterIn: fi,

		archiveAfterStop: map[string]bool{},

		defaultAttach: defaultAttach,
	}
	m.loadTUIState()
	m.km = opts.Keymap
	if m.km == nil {
		km, err := LoadKeymap(opts.KeysFile)
		m.km = km
		if err != nil {
			m.toast = &toast{text: "keys.toml ignored: " + err.Error(), tone: toneWarn}
		} else if len(km.Warnings) > 0 {
			m.toast = &toast{text: "keys.toml: " + km.Warnings[0] + " (pyre keys shows all)", tone: toneWarn}
		}
	}
	return m
}

// Close ends embedded terminals (releasing a held input lease) and cancels
// background work (watchers and in-flight loads).
func (m *Model) Close() {
	m.digestClose()
	m.closeTerms()
	m.closeGridTiles()
	m.cancel()
}

// ---- accessors for tests ----

// View name of the active screen.
func (m *Model) ActiveView() string { return m.view.name() }

// SelectedSession is the selected session ID in the Sessions list.
func (m *Model) SelectedSession() string { return m.sessList.sel }

// SelectedProject is the selected project ID in the Projects list.
func (m *Model) SelectedProject() string { return m.projList.sel }

// Filter is the committed filter text.
func (m *Model) Filter() string { return m.filter }

// Stats returns event/refresh counters.
func (m *Model) Stats() Stats { return m.stats }

// OverlayOpen reports whether a modal overlay is shown.
func (m *Model) OverlayOpen() bool { return m.overlay != nil }

// Loading reports whether any load is in flight.
func (m *Model) Loading() bool {
	for _, st := range m.loads {
		if st.inflight {
			return true
		}
	}
	return false
}

// Loaded reports whether key has a successful value.
func (m *Model) Loaded(key string) bool {
	st := m.loads[key]
	return st != nil && st.have
}

func (m *Model) now() time.Time { return m.opts.Clock.Now() }

// ---- tea.Model ----

func (m *Model) Init() tea.Cmd {
	cmds := m.loadBase(false)
	cmds = append(cmds, m.scheduleRefresh(), m.startWatch(), tea.RequestBackgroundColor, m.digestInit())
	return tea.Batch(cmds...)
}

func (m *Model) scheduleRefresh() tea.Cmd {
	if m.opts.RefreshInterval < 0 {
		return nil
	}
	return tea.Tick(m.opts.RefreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// baseKeys are always kept fresh: they drive the header, rows and badges.
var baseKeys = []string{"sessions", "nodes", "attention", "projects", "accounts"}

func (m *Model) loadBase(force bool) []tea.Cmd {
	var cmds []tea.Cmd
	for _, k := range baseKeys {
		if force {
			cmds = append(cmds, m.reload(k, m.baseLoader(k)))
		} else {
			cmds = append(cmds, m.ensure(k, 0, m.baseLoader(k)))
		}
	}
	return cmds
}

func (m *Model) baseLoader(key string) loaderFn {
	api := m.api
	switch key {
	case "sessions":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.SessionView], error) {
				return api.ListSessions(ctx, contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}
	case "nodes":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Node], error) {
				return api.ListNodes(ctx, contracts.ListNodesRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}
	case "attention":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.AttentionItem], error) {
				return api.ListAttention(ctx, contracts.ListAttentionRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200},
					States: []contracts.AttentionState{contracts.AttnOpen, contracts.AttnRead}})
			})
		}
	case "projects":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Project], error) {
				return api.ListProjects(ctx, contracts.ListProjectsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}
	case "accounts":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.AccountView], error) {
				return api.ListAccounts(ctx, contracts.ListAccountsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}
	case "connections":
		return func(ctx context.Context) (any, error) {
			return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Connection], error) {
				return api.ListConnections(ctx, contracts.ListConnectionsRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
			})
		}
	}
	return nil
}

// all follows NextCursor (bounded) and concatenates items.
func all[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (contracts.Page[T], error)) ([]T, error) {
	var out []T
	cur := ""
	for i := 0; i < 50; i++ {
		p, err := fetch(ctx, cur)
		if err != nil {
			return out, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" || p.NextCursor == cur {
			return out, nil
		}
		cur = p.NextCursor
	}
	return out, nil
}

// ensure loads key if it has never loaded, was invalidated, or is older
// than maxAge (0 = never age out). It never blocks.
func (m *Model) ensure(key string, maxAge time.Duration, fn loaderFn) tea.Cmd {
	st := m.loads[key]
	if st == nil {
		st = &loadState{}
		m.loads[key] = st
	}
	if fn != nil {
		st.fn = fn
	}
	if st.inflight || st.fn == nil {
		return nil
	}
	now := m.now()
	if st.have && !st.stale && (maxAge == 0 || now.Sub(st.at) < maxAge) {
		return nil
	}
	if !st.have && st.err != nil && !st.stale && now.Sub(st.errAt) < 2*time.Second {
		return nil
	}
	return m.start(key, st)
}

// reload forces a load of key (coalescing with an in-flight one).
func (m *Model) reload(key string, fn loaderFn) tea.Cmd {
	st := m.loads[key]
	if st == nil {
		st = &loadState{}
		m.loads[key] = st
	}
	if fn != nil {
		st.fn = fn
	}
	if st.fn == nil {
		return nil
	}
	if st.inflight {
		st.dirty = true
		return nil
	}
	return m.start(key, st)
}

func (m *Model) start(key string, st *loadState) tea.Cmd {
	if !st.inflight && (st.err == nil || st.started.IsZero()) {
		// A retry after a failure keeps counting from the first attempt.
		st.started = time.Now()
	}
	st.inflight = true
	st.dirty = false
	st.seq++
	seq, fn, parent, timeout := st.seq, st.fn, m.ctx, m.opts.LoadTimeout
	m.stats.Loads++
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		v, err := fn(ctx)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		return loadedMsg{key: key, seq: seq, val: v, err: err}
	}
}

func (m *Model) onLoaded(msg loadedMsg) tea.Cmd {
	st := m.loads[msg.key]
	if st == nil || msg.seq != st.seq {
		m.stats.DroppedLoads++
		return nil
	}
	st.inflight = false
	if msg.err != nil {
		st.err = msg.err
		st.errAt = m.now()
	} else {
		st.val, st.have, st.err, st.at, st.stale = msg.val, true, nil, m.now(), false
	}
	var cmds []tea.Cmd
	if st.dirty {
		began := st.started
		cmds = append(cmds, m.start(msg.key, st))
		if msg.err != nil {
			st.started = began // still no answer: keep counting from the first try
		}
	}
	cmds = append(cmds, m.afterData(msg.key))
	return tea.Batch(cmds...)
}

// afterData recomputes list contents (preserving selection) after a load.
func (m *Model) afterData(key string) tea.Cmd {
	switch key {
	case "sessions", "nodes", "projects":
		before := m.sessList.sel
		m.syncSessions()
		m.syncProjects()
		if m.pendingFocus != "" && m.sessList.selectID(m.pendingFocus) {
			m.pendingFocus = ""
		}
		var cmds []tea.Cmd
		if key == "sessions" {
			cmds = append(cmds, m.archivePending())
		}
		if m.sessList.sel != before {
			cmds = append(cmds, m.schedulePreview())
		}
		return tea.Batch(cmds...)
	default:
		// Opening an artifact before its versions arrive: load the
		// selected version's content once they do.
		if m.view == viewArtifact && key == "art:"+m.curArtifact+":versions" {
			if v, ok := m.selectedVersion(); ok && previewable(v) {
				return m.loadContent(v, false)
			}
		}
	}
	return nil
}

func get[T any](m *Model, key string) (T, *loadState) {
	var zero T
	st := m.loads[key]
	if st == nil || !st.have {
		return zero, st
	}
	v, _ := st.val.(T)
	return v, st
}

// do runs a mutation or one-off call asynchronously and hands the result
// to then inside Update.
func (m *Model) do(fn func(ctx context.Context) (any, error), then func(m *Model, val any, err error) tea.Cmd) tea.Cmd {
	parent, timeout := m.ctx, m.opts.LoadTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		v, err := fn(ctx)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = contracts.Errorf(contracts.ErrOutcomeIndeterminate, "no response within %s; outcome unknown", timeout)
		}
		return resultMsg{val: v, err: err, then: then}
	}
}

// opID returns the stable operation ID for a logical operation key,
// creating it on first use. Repeating the same logical operation reuses
// it, so the hub deduplicates instead of acting twice.
func (m *Model) opID(key string) string {
	if id, ok := m.opIDs[key]; ok {
		return id
	}
	id := contracts.NewID(contracts.PrefixOperation)
	m.opIDs[key] = id
	return id
}

// OperationID exposes the stable operation id for a logical key (tests).
func (m *Model) OperationID(key string) (string, bool) {
	id, ok := m.opIDs[key]
	return id, ok
}

func (m *Model) setToast(text string, tn tone) tea.Cmd {
	m.toastSeq++
	m.toast = &toast{text: text, tone: tn, token: m.toastSeq}
	tok := m.toastSeq
	return tea.Tick(8*time.Second, func(time.Time) tea.Msg { return clearToastMsg{token: tok} })
}

func (m *Model) errToast(prefix string, err error) tea.Cmd {
	return m.setToast(prefix+": "+m.humanIDs(errText(err)), toneBad)
}

// errText renders an API error with its stable code.
func errText(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		s := ce.Message
		if ce.Code != "" {
			s = string(ce.Code) + ": " + s
		}
		return s
	}
	return err.Error()
}

// Update implements tea.Model. After every message the embedded terminals
// are reconciled with the focus (see reconcileTerms).
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.update(msg)
	if rc := m.reconcileTerms(); rc != nil {
		cmd = tea.Batch(cmd, rc)
	}
	if gc := m.gridAfter(); gc != nil {
		cmd = tea.Batch(cmd, gc)
	}
	m.tableAfter() // sessions_modes.go
	return m, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case termFrameMsg:
		return m, m.onTermFrame()
	case gridFrameMsg, gridAttachMsg, gridPulseMsg, gridSettleMsg, gridProbeTickMsg, gridSizeMsg:
		return m, m.onGridMsg(msg)
	case termAttachMsg:
		return m, m.onTermAttach(msg)
	case leaseTickMsg:
		return m, m.onLeaseTick(msg)
	case termRetryMsg:
		return m, m.onTermRetry(msg)
	case wheelHoldMsg:
		return m, m.onWheelHold(msg)
	case tea.PasteMsg:
		if tv := m.focusedTerm(); tv != nil {
			return m, m.termPaste(tv, msg.Content)
		}
		// Pasting while the session's terminal is on screen (list focused)
		// types into it, like a click then paste.
		if m.view == viewSessions && m.overlay == nil && !m.filtering && !m.tableShown() {
			if s, ok := m.selectedSession(); ok {
				if emb, _ := m.embeddable(s); emb {
					cmd := m.focusTerm()
					if tv := m.focusedTerm(); tv != nil {
						return m, tea.Batch(cmd, m.termPaste(tv, msg.Content))
					}
					return m, cmd
				}
			}
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.ColorProfileMsg, tea.BackgroundColorMsg:
		m.onThemeMsg(msg)
		return m, nil
	case seqTimeoutMsg:
		return m, m.onSeqTimeout(msg)
	case whichKeyMsg:
		if msg.tok == m.pendingTok && len(m.pending) > 0 {
			m.whichKey = true
		}
		return m, nil
	case loadedMsg:
		return m, m.onLoaded(msg)
	case resultMsg:
		if msg.then != nil {
			return m, msg.then(m, msg.val, msg.err)
		}
		return m, nil
	case refreshTickMsg:
		return m, tea.Batch(m.refreshVisible(false), m.scheduleRefresh())
	case debounceMsg:
		return m, m.flushEvents()
	case eventsMsg:
		return m, m.onEvents(msg)
	case watchEndedMsg:
		return m, m.onWatchEnded(msg)
	case watchRestartMsg:
		return m, m.startWatch()
	case previewDueMsg:
		if msg.token != m.previewTok {
			return m, nil
		}
		if m.view == viewSessions {
			m.noteViewed(m.sessList.sel) // dwelt on: counts for alt+left and the switcher
		}
		return m, tea.Batch(m.loadPreview(false), m.openTermView())
	case clearToastMsg:
		if m.toast != nil && m.toast.token == msg.token {
			m.toast = nil
		}
		return m, nil
	case attachDoneMsg:
		return m, m.onAttachDone(msg)
	case editDoneMsg:
		return m, m.onEditDone(msg)
	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	case tea.MouseClickMsg:
		return m, m.onMouseClick(msg.Mouse())
	case tea.MouseWheelMsg:
		return m, m.onMouseWheel(msg.Mouse())
	case tea.MouseReleaseMsg:
		if cmd, ok := m.termMouse(mouseRelease, msg.Mouse()); ok {
			return m, cmd
		}
		cmd, _ := m.frameMouse(mouseRelease, msg.Mouse())
		return m, cmd
	case tea.MouseMotionMsg:
		if cmd, ok := m.termMouse(mouseMotion, msg.Mouse()); ok {
			return m, cmd
		}
		cmd, _ := m.frameMouse(mouseMotion, msg.Mouse())
		return m, cmd
	}
	if m.filtering {
		var cmd tea.Cmd
		m.filterIn, cmd = m.filterIn.Update(msg)
		return m, cmd
	}
	if m.overlay != nil {
		if u, ok := m.overlay.(msgUpdater); ok {
			return m, u.updateMsg(m, msg)
		}
	}
	return m, nil
}

// refreshVisible reloads base data plus whatever the current screen shows.
func (m *Model) refreshVisible(force bool) tea.Cmd {
	cmds := m.loadBase(true)
	cmds = append(cmds, m.refreshView(force)...)
	return tea.Batch(cmds...)
}

// refreshView reloads data for the active screen.
func (m *Model) refreshView(force bool) []tea.Cmd {
	var cmds []tea.Cmd
	switch m.view {
	case viewSessions:
		cmds = append(cmds, m.loadPreview(true), m.openTermView())
	case viewDigest:
		cmds = append(cmds, m.digestPreview())
	case viewProjects:
		cmds = append(cmds, m.loadProjectPreview(true))
	case viewProject, viewArtifact, viewDoc:
		cmds = append(cmds, m.loadProjectView(true)...)
	case viewMessages:
		cmds = append(cmds, m.loadMessages(true))
	case viewConnections:
		cmds = append(cmds, m.reload("connections", m.baseLoader("connections")))
	case viewAccounts:
		cmds = append(cmds, m.loadRecentSwitches(force))
	case viewActivity:
		cmds = append(cmds, m.loadActivity(force))
	case viewEnvs:
		cmds = append(cmds, m.loadEnvDetail(true)...)
	case viewReader:
		cmds = append(cmds, m.loadTranscript(force))
	}
	return cmds
}

func (m *Model) goTop(v viewID) tea.Cmd {
	m.view = v
	m.back = nil
	return tea.Batch(m.refreshView(false)...)
}

func (m *Model) push(v viewID) tea.Cmd {
	if m.view != v {
		m.back = append(m.back, m.view)
	}
	m.view = v
	return tea.Batch(m.refreshView(false)...)
}

func (m *Model) goBack() tea.Cmd {
	if len(m.back) == 0 {
		if !m.view.topLevel() {
			m.view = viewSessions
		}
		return nil
	}
	m.view = m.back[len(m.back)-1]
	m.back = m.back[:len(m.back)-1]
	return tea.Batch(m.refreshView(false)...)
}

// listFor returns a named list cursor (created on demand).
func (m *Model) listFor(name string) *listState {
	l := m.lists[name]
	if l == nil {
		l = &listState{}
		m.lists[name] = l
	}
	return l
}

// navKey applies canonical cursor keys (the dispatcher translates the
// configured bindings — j/k, gg/G, ctrl+d/u … — into these) to l.
func navKey(l *listState, key string, page int) bool {
	switch key {
	case "up":
		l.move(-1)
	case "down":
		l.move(1)
	case "pgup":
		l.move(-page)
	case "pgdown":
		l.move(page)
	case "home":
		l.home()
	case "end":
		l.end()
	default:
		return false
	}
	return true
}

// ---- filter ----

func (m *Model) startFilter() tea.Cmd {
	m.filtering = true
	m.filterIn.SetValue(m.filter)
	m.filterIn.CursorEnd()
	return m.filterIn.Focus()
}

func (m *Model) updateFilter(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.filtering = false
		m.filterIn.Blur()
		m.applyFilter("")
		return m.schedulePreview()
	case "enter":
		m.filtering = false
		m.filterIn.Blur()
		m.applyFilter(m.filterIn.Value())
		return m.schedulePreview()
	case "up", "down":
		if m.view == viewSessions {
			m.sessList.move(map[string]int{"up": -1, "down": 1}[k.String()])
			return m.schedulePreview()
		}
		if m.view == viewProjects {
			m.projList.move(map[string]int{"up": -1, "down": 1}[k.String()])
		}
		return nil
	}
	var cmd tea.Cmd
	m.filterIn, cmd = m.filterIn.Update(k)
	m.applyFilter(m.filterIn.Value())
	return tea.Batch(cmd, m.schedulePreview())
}

func (m *Model) applyFilter(q string) {
	m.filter = strings.TrimSpace(q)
	m.syncSessions()
	m.syncProjects()
}

// ---- lookups ----

func (m *Model) sessions() []contracts.SessionView {
	v, _ := get[[]contracts.SessionView](m, "sessions")
	return v
}

func (m *Model) session(id string) (contracts.SessionView, bool) {
	for _, s := range m.sessions() {
		if s.Session.SessionID == id {
			return s, true
		}
	}
	return m.sessionOutsideList(id) // e.g. an archived session opened from search (search.go)
}

func (m *Model) nodes() []contracts.Node {
	v, _ := get[[]contracts.Node](m, "nodes")
	return v
}

func (m *Model) node(id string) *contracts.Node {
	ns := m.nodes()
	for i := range ns {
		if ns[i].NodeID == id {
			return &ns[i]
		}
	}
	return nil
}

func (m *Model) projects() []contracts.Project {
	v, _ := get[[]contracts.Project](m, "projects")
	return v
}

func (m *Model) project(id string) *contracts.Project {
	ps := m.projects()
	for i := range ps {
		if ps[i].ProjectID == id {
			return &ps[i]
		}
	}
	return nil
}

func (m *Model) projectName(id *string) string {
	if id == nil || *id == "" {
		return ""
	}
	if p := m.project(*id); p != nil {
		return p.Name
	}
	return *id
}

func (m *Model) accounts() []contracts.AccountView {
	v, _ := get[[]contracts.AccountView](m, "accounts")
	return v
}

func (m *Model) account(id string) *contracts.AccountView {
	as := m.accounts()
	for i := range as {
		if as[i].Account.AccountID == id {
			return &as[i]
		}
	}
	return nil
}

func (m *Model) attention() []contracts.AttentionItem {
	v, _ := get[[]contracts.AttentionItem](m, "attention")
	return v
}

func (m *Model) sessionTitle(id string) string {
	if s, ok := m.session(id); ok {
		return s.Session.Title
	}
	return id
}

func (m *Model) nodeName(id string) string {
	if n := m.node(id); n != nil {
		return n.Name
	}
	return id
}

// sortSessionsByMachine orders the Sessions list: grouped by machine, then
// by title, then by ID (stable).
func sortSessionsByMachine(ss []contracts.SessionView) {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := strings.ToLower(ss[i].NodeName), strings.ToLower(ss[j].NodeName)
		if a != b {
			if a == "" || b == "" {
				return b == "" // sessions without a machine go last
			}
			return a < b
		}
		ta, tb := strings.ToLower(ss[i].Session.Title), strings.ToLower(ss[j].Session.Title)
		if ta != tb {
			return ta < tb
		}
		return ss[i].Session.SessionID < ss[j].Session.SessionID
	})
}

// sortSessions returns sessions in a stable display order.
func sortSessions(ss []contracts.SessionView) {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := strings.ToLower(ss[i].Session.Title), strings.ToLower(ss[j].Session.Title)
		if a != b {
			return a < b
		}
		return ss[i].Session.SessionID < ss[j].Session.SessionID
	})
}
