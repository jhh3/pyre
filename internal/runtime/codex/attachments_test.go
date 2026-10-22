package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// An image attachment reaches Codex as its node-local path inside the
// turn/start text input (the app-server's own image input is not used; the
// agent opens the file by path).
func TestCodexDeliversImagePathInTextInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	h := e.launch(e.a, "run_img", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	env := contracts.MessageEnvelope{MessageID: "msg_img", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"},
		Body: contracts.MessageBody{Text: "synthetic: what is wrong here?"}, Attachments: []contracts.Attachment{{ArtifactVersionID: "ver_img"}}}
	text := delivery.RenderFiles(env, map[string]string{"ver_img": "/synthetic/state/attachments/run_img/ver_img.png"})
	if r := dlv(t, e.a, h, "msg_img", text, contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	resp, err := e.a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range resp.Entries {
		if en.MessageID == "msg_img" {
			if !strings.Contains(en.Text, "synthetic: what is wrong here?\nAttachments:\n- image /synthetic/state/attachments/run_img/ver_img.png (pyre artifact get ver_img)") {
				t.Fatalf("turn input text: %q", en.Text)
			}
			return
		}
	}
	t.Fatalf("no user message for msg_img: %+v", resp.Entries)
}
