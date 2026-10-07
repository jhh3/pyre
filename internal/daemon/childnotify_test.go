package daemon

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-childnotify: a session started by another session learns who started it
// ($PYRE_PARENT_SESSION_ID) and whether its results are relayed
// ($PYRE_REPORT_TO_PARENT=1); owner-started sessions get neither.
func TestRunEnvNamesTheParentSession(t *testing.T) {
	d := &Daemon{cfg: Config{StateDir: t.TempDir(), PyreBinDir: "/opt/synth/bin"}}
	child := contracts.StartRunCommand{RunID: "run_synth-child", Session: contracts.Session{SessionID: "ses_synth-child",
		Lineage: contracts.Lineage{ParentSessionID: "ses_synth-parent"}, Tags: []string{contracts.TagReportParent}}}
	env := d.runEnv(nil, child, t.TempDir())
	if env["PYRE_PARENT_SESSION_ID"] != "ses_synth-parent" || env["PYRE_REPORT_TO_PARENT"] != "1" {
		t.Fatalf("child env: parent=%q report=%q", env["PYRE_PARENT_SESSION_ID"], env["PYRE_REPORT_TO_PARENT"])
	}
	child.Session.Tags = []string{contracts.TagReportNone}
	if env := d.runEnv(nil, child, t.TempDir()); env["PYRE_PARENT_SESSION_ID"] != "ses_synth-parent" || env["PYRE_REPORT_TO_PARENT"] != "" {
		t.Fatalf("opted-out child env: %q", env["PYRE_REPORT_TO_PARENT"])
	}
	owner := contracts.StartRunCommand{RunID: "run_synth-owner", Session: contracts.Session{SessionID: "ses_synth-owner"}}
	if env := d.runEnv(nil, owner, t.TempDir()); env["PYRE_PARENT_SESSION_ID"] != "" || env["PYRE_REPORT_TO_PARENT"] != "" {
		t.Fatalf("owner-started env names a parent: %v", env)
	}
}
