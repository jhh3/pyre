package hub

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// pyre resume --env-file PATH|none reaches the node as its env-file start
// arg (only the path); a node without launch.env_file is refused.
func TestResumeEnvFile(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	run := *v.Run
	run.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, run)
	_, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r1"}, SessionID: v.Session.SessionID, EnvFile: "none"})
	wantCode(t, err, contracts.ErrUnsupported)
	if len(x.ch.sentKinds(nodeMac, contracts.CmdResume)) != 0 {
		t.Fatal("refused resume reached the node")
	}

	hello := helloFor(nodeMac, 4)
	hello.Harnesses[0].Capabilities = append(slices.Clone(claudeCaps), contracts.CapLaunchEnvFile)
	if _, err := x.h.HandleHello(nodeCtx(nodeMac), nodeMac, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r2"}, SessionID: v.Session.SessionID, EnvFile: "/home/synth/.config/pyre/aide.env"}); err != nil {
		t.Fatal(err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	if len(cmds) != 1 {
		t.Fatalf("resume commands: %d", len(cmds))
	}
	var rc contracts.ResumeCommand
	_ = json.Unmarshal(cmds[0].Payload, &rc)
	if !slices.Equal(rc.Args, []string{"--pyre-env-file=/home/synth/.config/pyre/aide.env"}) {
		t.Fatalf("resume args %q", rc.Args)
	}
}
