package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// An image attachment reaches Claude Code as its node-local path in the
// pasted prompt (Claude Code opens image files by path).
func TestFakeDeliversImagePathInPrompt(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_img", nil, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	env := contracts.MessageEnvelope{MessageID: "msg_img", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"},
		Body: contracts.MessageBody{Text: "synthetic: what is wrong here?"}, Attachments: []contracts.Attachment{{ArtifactVersionID: "ver_img"}}}
	text := delivery.RenderFiles(env, map[string]string{"ver_img": "/synthetic/state/attachments/run_img/ver_img.png"})
	if r := deliver(t, f.a, h, "msg_img", text, contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("deliver %+v", r)
	}
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	resp, _ := f.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Tail: true, Limit: 5})
	for _, e := range resp.Entries {
		if e.MessageID == "msg_img" {
			if !strings.Contains(e.Text, "\nAttachments:\n- image /synthetic/state/attachments/run_img/ver_img.png (pyre artifact get ver_img)") {
				t.Fatalf("prompt text: %q", e.Text)
			}
			return
		}
	}
	t.Fatalf("no prompt for msg_img: %+v", resp.Entries)
}
