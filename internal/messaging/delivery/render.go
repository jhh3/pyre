package delivery

import (
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/header"
)

// Render produces the text injected into a runtime for a message, with the
// send time in the node's local zone and its age at delivery:
//
//	[pyre-msg:<id> · sent 3:05 PM PT (8h ago)] from <kind>:<title or id>[ (thread <t>, reply to <m>)]:
//	<body>
//	Attachments:
//	- pyre artifact get <version_id>
//	- image <absolute path> (pyre artifact get <version_id>)
//
// The second attachment form is an image the node fetched for the run
// (Files): both harnesses open an image file by path. The marker comes first
// so adapters can match acceptance evidence (UserPromptSubmit text,
// transcript records).
func Render(e contracts.MessageEnvelope) string {
	return RenderFiles(e, nil)
}

// RenderFiles is Render with local copies of attached artifact versions
// (absolute paths by version ID, from AttachmentFiles).
func RenderFiles(e contracts.MessageEnvelope, files map[string]string) string {
	return renderAt(e, time.Now(), time.Local, files)
}

// RenderAt is Render at a given delivery time and zone. A message without a
// send time keeps the bare "[pyre-msg:<id>]" marker.
func RenderAt(e contracts.MessageEnvelope, now time.Time, loc *time.Location) string {
	return renderAt(e, now, loc, nil)
}

func renderAt(e contracts.MessageEnvelope, now time.Time, loc *time.Location, files map[string]string) string {
	var b strings.Builder
	b.WriteString(header.Marker(e.MessageID, header.Sent(e.CreatedAt, now, loc)))
	b.WriteString(" from ")
	b.WriteString(SenderLabel(e.Sender))
	var ctx []string
	if e.Mode == contracts.ModeSteer {
		ctx = append(ctx, "steering")
	}
	if e.ThreadID != "" {
		ctx = append(ctx, "thread "+e.ThreadID)
	}
	if e.InReplyTo != "" {
		ctx = append(ctx, "reply to "+e.InReplyTo)
	}
	if e.Reissues != "" {
		ctx = append(ctx, "reissue of "+e.Reissues)
	}
	if e.ProjectID != nil && *e.ProjectID != "" {
		ctx = append(ctx, "project "+*e.ProjectID)
	}
	if e.RequestAck {
		ctx = append(ctx, "ack requested") // how to ack is in the agent guide (MCP tool or CLI, per harness)
	}
	if len(ctx) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(ctx, ", "))
		b.WriteString(")")
	}
	b.WriteString(":\n")
	b.WriteString(e.Body.Text)
	if len(e.Attachments) > 0 {
		b.WriteString("\nAttachments:")
		for _, a := range e.Attachments {
			switch {
			case a.ArtifactVersionID != "" && files[a.ArtifactVersionID] != "":
				b.WriteString("\n- image " + files[a.ArtifactVersionID] + " (pyre artifact get " + a.ArtifactVersionID + ")")
			case a.ArtifactVersionID != "":
				b.WriteString("\n- pyre artifact get " + a.ArtifactVersionID)
			case a.DocumentRevisionID != "":
				b.WriteString("\n- pyre doc get --revision " + a.DocumentRevisionID)
			case a.NoteID != "":
				b.WriteString("\n- pyre note get " + a.NoteID)
			}
		}
	}
	return b.String()
}

// SenderLabel renders a principal as "<kind>:<title>", or "<kind>:<id>"
// when it has no display name (the id stays available via get_message).
func SenderLabel(p contracts.Principal) string {
	s := string(p.Kind) + ":" + p.ID
	if p.Name != "" {
		s = string(p.Kind) + ":" + p.Name
	}
	if p.OnBehalfOf != nil {
		s += " on behalf of " + string(p.OnBehalfOf.Kind) + ":" + p.OnBehalfOf.ID
	}
	return s
}
