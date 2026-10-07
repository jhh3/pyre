package delivery

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Render produces the text injected into a runtime for a message:
//
//	[pyre-msg:<id>] from <sender>[ (thread <t>, reply to <m>)]:
//	<body>
//	Attachments:
//	- pyre artifact get <version_id>
//
// The marker comes first so adapters can match acceptance evidence
// (UserPromptSubmit text, transcript records).
func Render(e contracts.MessageEnvelope) string {
	var b strings.Builder
	b.WriteString(contracts.MessageMarker(e.MessageID))
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

// SenderLabel renders a principal as "<kind>:<id>" plus its display alias.
func SenderLabel(p contracts.Principal) string {
	s := string(p.Kind) + ":" + p.ID
	if p.Name != "" {
		s += " (" + p.Name + ")"
	}
	if p.OnBehalfOf != nil {
		s += " on behalf of " + string(p.OnBehalfOf.Kind) + ":" + p.OnBehalfOf.ID
	}
	return s
}
