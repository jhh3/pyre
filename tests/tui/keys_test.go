package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

func TestLeaderWhichKeyPopupTiming(t *testing.T) {
	h, _ := start(t, 120, 34, nil, nil)
	h.Key("space")
	if h.Contains("esc cancels") {
		t.Fatal("which-key popup must wait for the delay, not appear at once")
	}
	if !h.Contains("␣ …") {
		t.Errorf("pending leader not shown in the status line:\n%s", h.View())
	}
	t0 := time.Now()
	h.WaitFor("which-key popup", 3*sec, func() bool { return h.Contains("esc cancels") })
	if d := time.Since(t0); d < 150*time.Millisecond {
		t.Errorf("popup appeared after %s, want ~300ms", d)
	}
	for _, want := range []string{"Machines", "Accounts", "launch a new session"} {
		if !h.Contains(want) {
			t.Errorf("popup lacks %q:\n%s", want, h.View())
		}
	}
	h.Key("m")
	if h.M.ActiveView() != "Machines" || h.Contains("esc cancels") {
		t.Fatalf("<leader>m: view=%s\n%s", h.M.ActiveView(), h.View())
	}
	// Esc cancels a pending leader; the next key acts normally.
	h.Key("S", "space", "esc", "j")
	if h.M.ActiveView() != "Sessions" || h.M.OverlayOpen() {
		t.Fatalf("esc did not cancel the leader")
	}
}

func TestLeaderLeaderOpensFinderAndAttaches(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	h.Key("space", "space")
	if !h.M.OverlayOpen() || !h.Contains("Find ›") {
		t.Fatalf("<leader><leader> should open the finder:\n%s", h.View())
	}
	h.Type("coord")
	h.Settle()
	v := h.View()
	if !strings.Contains(v, "sess") || !strings.Contains(v, "coordinator") {
		t.Fatalf("finder results:\n%s", v)
	}
	// preview of the top result is shown
	if !strings.Contains(v, "Activity") {
		t.Errorf("finder should preview the selected session:\n%s", v)
	}
	h.Key("ctrl+a")
	h.WaitFor("attach from finder", 3*sec, func() bool { return len(h.Attaches()) == 1 })
	if h.M.SelectedSession() != "ses_synth-a" {
		t.Errorf("selection after ctrl+a = %s", h.M.SelectedSession())
	}
}

func TestFinderSearchesAcrossKinds(t *testing.T) {
	h, _ := start(t, 160, 34, nil, nil)
	h.Key("ctrl+p")
	h.Settle()
	h.Type("mac")
	v := h.View()
	if !strings.Contains(v, "node") || !strings.Contains(v, "synth-mac") {
		t.Fatalf("machines should be searchable:\n%s", v)
	}
	h.Key("ctrl+w")
	h.Type("approve make")
	if !strings.Contains(h.View(), "attn") || !strings.Contains(h.View(), "Approve: make deploy-staging") {
		t.Fatalf("attention items should be searchable:\n%s", h.View())
	}
	h.Key("enter")
	if h.M.ActiveView() != "Sessions" || h.M.SelectedSession() != "ses_synth-apr" {
		t.Fatalf("enter should open the item's session: %s %s", h.M.ActiveView(), h.M.SelectedSession())
	}
}

func TestVimNavigationAndPanes(t *testing.T) {
	h, _ := start(t, 120, 34, nil, nil)
	first := h.M.SelectedSession()
	h.Key("end") // G opens the Grid
	last := h.M.SelectedSession()
	if last == first {
		t.Fatal("end did not move to the last item")
	}
	h.Key("g")
	if h.M.SelectedSession() != last {
		t.Fatal("a single g must wait for the second g")
	}
	h.Key("g")
	if h.M.SelectedSession() != first {
		t.Fatal("gg did not move to the first item")
	}
	h.Key("j", "j", "k")
	h.Key("ctrl+d")
	h.Key("ctrl+u")
	h.Key("l")
	h.WaitFor("terminal focus", 3*sec, func() bool { return h.M.TermFocused() && h.Contains("back to the list") })
	before := h.M.SelectedSession()
	h.Key("j", "j")
	if h.M.SelectedSession() != before {
		t.Fatal("j in the focused terminal must go to the session, not move the list")
	}
	h.Key("ctrl+]")
	h.Key("j")
	if h.M.SelectedSession() == before {
		t.Fatal("ctrl+] should return focus to the list")
	}
}

func TestFilterLiveAndNextMatch(t *testing.T) {
	h, _ := start(t, 100, 30, nil, nil)
	h.Key("/")
	h.Type("work")
	if !h.Contains("matches") {
		t.Errorf("live filter should show the match count:\n%s", h.View())
	}
	h.Key("enter")
	if h.M.Filter() != "work" {
		t.Fatalf("filter = %q", h.M.Filter())
	}
	a := h.M.SelectedSession()
	h.Key("n")
	b := h.M.SelectedSession()
	if a == b || h.M.ActiveView() != "Sessions" {
		t.Fatalf("n should move to the next match (view %s)", h.M.ActiveView())
	}
	h.Key("N")
	if h.M.SelectedSession() != a {
		t.Fatal("N should move back")
	}
	h.Key("N")
	if h.M.SelectedSession() == a {
		t.Fatal("N should wrap to the last match")
	}
	h.Key("esc")
	if h.M.Filter() != "" {
		t.Fatal("esc should clear the filter")
	}
	h.Key("n")
	if !h.M.QuickLaunchOpen() {
		t.Fatalf("without a filter n opens the quick-launch bar, got %s", h.M.ActiveView())
	}
}

func TestCmdlineCompletionAndHistory(t *testing.T) {
	rec := (*tuitest.Recorder)(nil)
	h, _ := start(t, 100, 30, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	h.Key(":")
	h.Type("mach")
	if !h.Contains("machines") {
		t.Fatalf("completion menu:\n%s", h.View())
	}
	h.Key("tab")
	if !h.Contains(":machines ") {
		t.Fatalf("tab should complete the command:\n%s", h.View())
	}
	h.Key("enter")
	if h.M.ActiveView() != "Machines" {
		t.Fatalf("view = %s", h.M.ActiveView())
	}
	h.Key(":")
	h.Type("attach coord")
	if !h.Contains("coordinator") {
		t.Fatalf("argument completion should list sessions:\n%s", h.View())
	}
	h.Key("enter")
	h.WaitFor("attach", 3*sec, func() bool { return len(h.Attaches()) == 1 })
	if got := writableAttaches(rec); len(got) != 1 || got[0].SessionID != "ses_synth-a" {
		t.Fatalf("writable attach calls = %+v", got)
	}
	h.Settle()
	h.Key(":", "up")
	if !h.Contains(":attach coord") {
		t.Fatalf("up should recall the last command:\n%s", h.View())
	}
	h.Key("up")
	if !h.Contains(":machines") {
		t.Fatalf("up again should recall the previous command:\n%s", h.View())
	}
	h.Key("esc")
	if h.M.OverlayOpen() {
		t.Fatal("esc should close the command line")
	}
}

func TestComposerKeys(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 120, 34, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	selectSession(t, h, "ses_synth-b")
	// jk leaves the composer without sending and keeps the draft (sans j).
	h.Key("m")
	h.Type("draft one")
	h.Key("j", "k")
	if h.M.OverlayOpen() {
		t.Fatal("jk should leave the composer")
	}
	h.Key("m")
	if !h.Contains("draft one") || h.Contains("draft onej") {
		t.Fatalf("draft not preserved exactly:\n%s", h.View())
	}
	// shift+enter / alt+enter / ctrl+j insert newlines; enter sends.
	h.Key("shift+enter")
	h.Type("line two")
	h.Key("alt+enter")
	h.Type("line three")
	h.Key("ctrl+j")
	h.Type("@coo")
	if !h.Contains("@coordinator") {
		t.Fatalf("@ completion menu missing:\n%s", h.View())
	}
	h.Key("tab")
	h.Key("enter")
	h.WaitFor("sent", 3*sec, func() bool { return len(rec.Calls("SendMessage")) == 1 })
	body := rec.Calls("SendMessage")[0].Request.(contracts.SendMessageRequest).Body.Text
	if body != "draft one\nline two\nline three\n@coordinator" {
		t.Fatalf("body = %q", body)
	}
	h.Settle()
	// ctrl+r searches previously sent messages.
	h.Key("S")
	selectSession(t, h, "ses_synth-a")
	h.Key("m", "ctrl+r")
	h.Type("three")
	if !h.Contains("history ›") || !h.Contains("draft one") {
		t.Fatalf("reverse search:\n%s", h.View())
	}
	h.Key("enter")
	if !h.Contains("line three") {
		t.Fatalf("history entry not inserted:\n%s", h.View())
	}
	h.Key("esc")
	if h.M.OverlayOpen() || len(rec.Calls("SendMessage")) != 1 {
		t.Fatal("esc must close without sending")
	}
}

func TestComposerPasteIsVerbatim(t *testing.T) {
	var rec *tuitest.Recorder
	h, _ := start(t, 120, 34, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key("m")
	h.Paste("pasted\nblock with jk inside")
	h.Key("enter")
	h.WaitFor("sent", 3*sec, func() bool { return len(rec.Calls("SendMessage")) == 1 })
	if got := rec.Calls("SendMessage")[0].Request.(contracts.SendMessageRequest).Body.Text; got != "pasted\nblock with jk inside" {
		t.Fatalf("paste body = %q", got)
	}
}

func TestSessionShortcuts(t *testing.T) {
	var mu sync.Mutex
	var copied []string
	var rec *tuitest.Recorder
	h, _ := start(t, 160, 34, func(a contracts.API) contracts.API { rec = tuitest.NewRecorder(a); return rec }, func(o *tui.Options) {
		o.Clipboard = func(s string) { mu.Lock(); copied = append(copied, s); mu.Unlock() }
	})
	selectSession(t, h, "ses_synth-a")
	h.Key("y")
	mu.Lock()
	if len(copied) != 1 || copied[0] != "ses_synth-a" {
		t.Fatalf("yank copied %v", copied)
	}
	mu.Unlock()
	// a answers the pending request of the selected session in place.
	for i := 0; i < 20 && !strings.Contains(h.View(), "a answer"); i++ {
		h.Key("j")
	}
	h.Key("a")
	h.WaitFor("answer overlay", 3*sec, func() bool { return h.Contains("Allow once") })
	h.Key("1")
	h.WaitFor("answered", 3*sec, func() bool { return len(rec.Calls("AnswerApproval")) == 1 })
	h.Key("esc")
	// r reads the transcript full-screen, / searches, n/N jump.
	selectSession(t, h, "ses_synth-a")
	h.Key("r")
	h.WaitFor("reader", 3*sec, func() bool { return h.M.ActiveView() == "Transcript" && h.Contains("synthetic") })
	h.Key("/")
	h.Type("synthetic")
	h.Key("enter")
	if !h.Contains("/synthetic") || !h.Contains("n/N") {
		t.Fatalf("reader search status:\n%s", h.View())
	}
	h.Key("n", "N", "esc", "esc")
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("esc should leave the reader, got %s", h.M.ActiveView())
	}
	// <leader>t opens the reader straight into search (<leader>/ is the
	// global search).
	h.Key("space", "t")
	if h.M.ActiveView() != "Transcript" {
		t.Fatalf("<leader>t view = %s", h.M.ActiveView())
	}
	h.Type("task")
	if !h.Contains("/task") {
		t.Fatalf("search prompt:\n%s", h.View())
	}
}

func TestHelpIsSearchable(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	h.Key("?")
	if !h.Contains("Keys & help") || !h.Contains("half page down") {
		t.Fatalf("help:\n%s", h.View())
	}
	h.Type("yank")
	v := h.View()
	if !strings.Contains(v, "copy the session ID") || strings.Contains(v, "half page down") {
		t.Fatalf("help search should filter:\n%s", v)
	}
	h.Key("esc")
	if h.M.OverlayOpen() {
		t.Fatal("esc closes help")
	}
}

func TestKeysFileOverridesInTheUI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.toml")
	os.WriteFile(path, []byte("leader = \",\"\n[keys]\n\"sessions.attach_full\" = \"o\"\n\"nav.down\" = \"ctrl+n\"\n"), 0o600)
	h, _ := start(t, 120, 34, nil, func(o *tui.Options) { o.KeysFile = path })
	first := h.M.SelectedSession()
	h.Key("j")
	if h.M.SelectedSession() != first {
		t.Fatal("j should be unbound by the override")
	}
	h.Key("ctrl+n")
	if h.M.SelectedSession() == first {
		t.Fatal("ctrl+n should move down")
	}
	h.Key("ctrl+n", "ctrl+n") // outfit API client: a live run
	h.Key("o")
	h.WaitFor("attach via o", 3*sec, func() bool { return len(h.Attaches()) == 1 })
	h.Settle()
	h.Key(",", "m")
	if h.M.ActiveView() != "Machines" {
		t.Fatalf("custom leader: view = %s", h.M.ActiveView())
	}
	h.Key("?")
	if !h.Contains("overrides from") {
		t.Errorf("help should name the override file:\n%s", h.View())
	}
}

func TestInteractionScreensFitAt60(t *testing.T) {
	tours := [][]string{{"space"}, {"space", "space"}, {":"}, {"?"}, {"/"}, {"r"}, {"m"}, {"l"}}
	for _, tour := range tours {
		h, _ := start(t, 60, 24, nil, nil)
		h.Key(tour...)
		time.Sleep(350 * time.Millisecond) // let the which-key popup appear
		h.Settle()
		lines := strings.Split(h.View(), "\n")
		if len(lines) != 24 {
			t.Errorf("tour %v: %d lines", tour, len(lines))
		}
		for i, l := range lines {
			if n := tuitestWidth(l); n > 60 {
				t.Errorf("tour %v line %d is %d wide: %q", tour, i, n, l)
			}
		}
		h.Close()
	}
}
