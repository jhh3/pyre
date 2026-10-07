// Command genschema writes JSON Schema for Pyre's v1 wire types to
// schemas/pyre-v1.schema.json, generated from internal/contracts so the Go
// types remain the single source of truth. Android and other non-Go clients
// use this file plus tests/fixtures/contracts.
package main

import (
	"encoding/json"
	"flag"
	"os"

	"github.com/invopop/jsonschema"
	c "github.com/jhh3/pyre/internal/contracts"
)

func main() {
	out := flag.String("out", "schemas/pyre-v1.schema.json", "output file")
	flag.Parse()
	r := &jsonschema.Reflector{DoNotReference: false, ExpandedStruct: false, AllowAdditionalProperties: true}
	type All struct {
		Error            c.Error
		Node             c.Node
		SessionView      c.SessionView
		Run              c.Run
		AttachDescriptor c.AttachDescriptor
		CommandReceipt   c.CommandReceipt
		InputLease       c.InputLease
		Approval         c.Approval
		MessageView      c.MessageView
		Project          c.Project
		Document         c.DocumentWithRevision
		DocumentProposal c.DocumentProposal
		Artifact         c.Artifact
		ArtifactVersion  c.ArtifactVersion
		Upload           c.Upload
		PublishReceipt   c.PublishReceipt
		ReviewComment    c.ReviewComment
		Note             c.Note
		LaunchContext    c.LaunchContext
		AccountView      c.AccountView
		FallbackPlan     c.FallbackPlan
		AuditEntry       c.AuditEntry
		AttentionItem    c.AttentionItem
		Event            c.Event
		ListEvents       c.ListEventsResponse
		EnvironmentGen   c.EnvironmentGeneration
		ChangePlan       c.ChangePlan
		ToolVersion      c.ToolVersion
		Connection       c.Connection
		NodeFrame        c.NodeFrame
		HelloResponse    c.HelloResponse
		PairingTicket    c.PairingTicket
		ClientCredential c.ClientCredential
		NodeActionResult c.NodeActionResult
		PreviewURL       c.PreviewURL
		MintPreview      c.MintPreviewRequest
		CheckGrant       c.CheckGrantRequest
		GrantDecision    c.GrantDecision
		AccountLease     c.AccountLease
		AcquireLease     c.AcquireAccountLeaseRequest
		StartSession     c.StartSessionRequest
		SendMessage      c.SendMessageRequest
		AnswerApproval   c.AnswerApprovalRequest
		ProposeRevision  c.ProposeRevisionRequest
		BeginUpload      c.BeginUploadRequest
		CreateComment    c.CreateCommentRequest
		FixtureBundle    c.FixtureBundle
	}
	s := r.Reflect(&All{})
	s.ID = "https://pyre.invalid/schemas/pyre-v1.schema.json"
	s.Title = "Pyre wire contracts v1 (generated from internal/contracts)"
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}
