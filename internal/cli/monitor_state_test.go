package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A monitor relaunched after a reboot (iss_…)
// continues from its saved state: an unchanged output wakes nobody, the
// --for deadline counts from the original start, a change the target was
// not told about yet is still reported, and a monitor that ended
// deliberately ends again at once.
func TestMonitorRelaunchContinuesFromSavedState(t *testing.T) {
	path := monitorStatePath(t.TempDir(), "ses_synth-mon")
	t0 := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	newLoop := func(now *time.Time, outputs []string, i *int, sent *[]string, busy *int) *monitorLoop {
		var log bytes.Buffer
		return &monitorLoop{To: "ses_synth-target", Label: "date", Cmd: "date +%H", Every: 15 * time.Minute, For: 2 * time.Hour,
			UntilChange: true, Out: &log, Self: "ses_synth-mon",
			Now: func() time.Time { return *now },
			Sleep: func(ctx context.Context, d time.Duration) error {
				*now = now.Add(d)
				if *i >= len(outputs) {
					return context.Canceled // the reboot
				}
				return nil
			},
			Check: func(ctx context.Context, cmd string) string { *i++; return outputs[*i-1] },
			Send:  func(ctx context.Context, text string) error { *sent = append(*sent, text); return nil },
			Target: func(ctx context.Context) monitorTargetState {
				if *busy > 0 {
					*busy--
					return monitorTargetState{Busy: true, Why: "running"}
				}
				return monitorTargetState{}
			},
			Load: func() (monitorState, bool) { return loadMonitorState(path) },
			Save: func(st monitorState) {
				if err := saveMonitorState(path, st); err != nil {
					t.Fatal(err)
				}
			},
		}
	}

	// First life: baseline "08", unchanged twice, then the machine dies.
	now, i, busy := t0, 0, 0
	var sent []string
	m := newLoop(&now, []string{"08", "08", "08"}, &i, &sent, &busy)
	if err := m.run(context.Background()); err != context.Canceled || len(sent) != 0 {
		t.Fatalf("first life: %v, sent %q", err, sent)
	}
	st, ok := loadMonitorState(path)
	if !ok || !st.Start.Equal(t0) || st.Seen != "08" || !st.HaveSeen || st.Baseline != "08" || st.Done != "" {
		t.Fatalf("saved state = %+v (ok %v)", st, ok)
	}

	// Relaunched an hour later: the same output is not a change.
	now, i = t0.Add(time.Hour), 0
	m = newLoop(&now, []string{"08", "09"}, &i, &sent, &busy)
	busy = 2 // the change to 09 is held: the target is mid-turn
	if err := m.run(context.Background()); err != context.Canceled || len(sent) != 0 {
		t.Fatalf("relaunch: %v, sent %q", err, sent)
	}
	if st, _ := loadMonitorState(path); st.Seen != "08" || !st.Changed {
		t.Fatalf("held change not kept as unseen: %+v", st)
	}

	// Relaunched again: the held change is reported, and --until-change ends it.
	now, i = t0.Add(90*time.Minute), 0
	m = newLoop(&now, []string{"09"}, &i, &sent, &busy)
	if err := m.run(context.Background()); err != nil || len(sent) != 1 || !strings.Contains(sent[0], "09") || !strings.Contains(sent[0], "previous: 08") {
		t.Fatalf("held change after relaunch: %v, sent %q", err, sent)
	}
	if st, _ := loadMonitorState(path); !strings.Contains(st.Done, "until-change") {
		t.Fatalf("done not recorded: %+v", st)
	}

	// A resume of the ended monitor ends again without a check.
	i = 0
	m = newLoop(&now, []string{"10"}, &i, &sent, &busy)
	if err := m.run(context.Background()); err != nil || i != 0 || len(sent) != 1 {
		t.Fatalf("ended monitor ran again: %v, %d checks", err, i)
	}

	// --for counts from the first start: a relaunch past it ends at once.
	path2 := filepath.Join(filepath.Dir(path), "ses_synth-mon2.json")
	if err := saveMonitorState(path2, monitorState{Start: t0, Seen: "08", HaveSeen: true}); err != nil {
		t.Fatal(err)
	}
	now, i = t0.Add(3*time.Hour), 0
	m = newLoop(&now, []string{"08"}, &i, &sent, &busy)
	m.Load = func() (monitorState, bool) { return loadMonitorState(path2) }
	m.Save = func(st monitorState) { _ = saveMonitorState(path2, st) }
	if err := m.run(context.Background()); err != nil || i != 0 {
		t.Fatalf("expired relaunch: %v, %d checks", err, i)
	}
	if st, _ := loadMonitorState(path2); !strings.Contains(st.Done, "ran for 2h") {
		t.Fatalf("expiry not recorded: %+v", st)
	}
}
