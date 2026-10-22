package delivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeFiles is an AttachmentFiles that "fetched" some versions.
type fakeFiles struct {
	paths map[string]string
	calls []string // run IDs asked for
	block bool     // wait for ctx (timeout)
}

func (f *fakeFiles) Fetch(ctx context.Context, t Target, e contracts.MessageEnvelope) map[string]string {
	f.calls = append(f.calls, t.Run.RunID)
	if f.block {
		<-ctx.Done()
		return nil
	}
	return f.paths
}

func TestAttachedImagesAreRenderedAsLocalPaths(t *testing.T) {
	e := newEnv(t)
	files := &fakeFiles{paths: map[string]string{"ver_img": "/state/attachments/run_1/ver_img.png"}}
	e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep, Files: files})
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", func(d *contracts.DeliverCommand) {
		d.Message.Attachments = []contracts.Attachment{{ArtifactVersionID: "ver_img"}, {ArtifactVersionID: "ver_doc"}, {NoteID: "note_1"}}
	})
	e.put("msg_2", "ses_a", nil) // no attachments: Files is not asked
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	p, _ := e.world.Proc("run_1")
	if len(p.Delivered) != 2 {
		t.Fatalf("delivered = %d", len(p.Delivered))
	}
	want := "body of msg_1\nAttachments:\n- image /state/attachments/run_1/ver_img.png (pyre artifact get ver_img)\n- pyre artifact get ver_doc\n- pyre note get note_1"
	if text := p.Delivered[0].Text; !strings.HasSuffix(text, want) {
		t.Fatalf("rendered = %q", text)
	}
	if len(files.calls) != 1 || files.calls[0] != "run_1" {
		t.Fatalf("Files calls = %v", files.calls)
	}
}

func TestAttachmentFetchTimeoutStillDelivers(t *testing.T) {
	e := newEnv(t)
	files := &fakeFiles{block: true}
	e.engine = New(Config{NodeID: "node_test", Inbox: e.inbox, Runs: e.runs, Reporter: e.rep, Files: files, FilesTimeout: 20 * time.Millisecond})
	e.launch("ses_a", "run_1", nil)
	e.put("msg_1", "ses_a", func(d *contracts.DeliverCommand) {
		d.Message.Attachments = []contracts.Attachment{{ArtifactVersionID: "ver_img"}}
	})
	e.engine.ProcessSession(context.Background(), "ses_a", false)
	if m := e.state("msg_1"); m.State != contracts.DeliveryAccepted {
		t.Fatalf("message = %+v", m)
	}
	p, _ := e.world.Proc("run_1")
	if text := p.Delivered[0].Text; !strings.HasSuffix(text, "\nAttachments:\n- pyre artifact get ver_img") {
		t.Fatalf("rendered = %q", text)
	}
}
