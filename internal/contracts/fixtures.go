package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FixtureBundle is a portable, obviously synthetic scenario used by the mock
// backend, client tests (Go and Android), and conformance checks. All IDs,
// names, and text in fixtures are synthetic.
type FixtureBundle struct {
	SchemaVersion int      `json:"schema_version"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Synthetic     bool     `json:"synthetic"` // must be true
	Requirements  []string `json:"requirements,omitempty"`
	Cases         []string `json:"cases,omitempty"` // acceptance case IDs, e.g. A05

	Nodes        []Node                  `json:"nodes,omitempty"`
	Sessions     []SessionView           `json:"sessions,omitempty"`
	Runs         []Run                   `json:"runs,omitempty"` // historical runs (current runs live in sessions)
	Messages     []MessageView           `json:"messages,omitempty"`
	Approvals    []Approval              `json:"approvals,omitempty"`
	Leases       []InputLease            `json:"leases,omitempty"`
	Projects     []Project               `json:"projects,omitempty"`
	Documents    []Document              `json:"documents,omitempty"`
	Revisions    []DocumentRevision      `json:"revisions,omitempty"`
	Proposals    []DocumentProposal      `json:"proposals,omitempty"`
	Artifacts    []Artifact              `json:"artifacts,omitempty"`
	Versions     []ArtifactVersion       `json:"versions,omitempty"`
	Comments     []ReviewComment         `json:"comments,omitempty"`
	Notes        []Note                  `json:"notes,omitempty"`
	Accounts     []AccountView           `json:"accounts,omitempty"`
	Observations []AccountObservation    `json:"observations,omitempty"`
	Fallbacks    []FallbackPlan          `json:"fallbacks,omitempty"`
	Attention    []AttentionItem         `json:"attention,omitempty"`
	Events       []Event                 `json:"events,omitempty"`
	Environments []EnvironmentGeneration `json:"environments,omitempty"`
	Tools        []ToolVersion           `json:"tools,omitempty"`
	Drift        []DriftItem             `json:"drift,omitempty"`
	Plans        []ChangePlan            `json:"plans,omitempty"`
	Connections  []Connection            `json:"connections,omitempty"`

	// Exchanges are request/response examples (idempotency, conflicts,
	// protocol mismatch). Request/Response are raw JSON for portability.
	Exchanges []FixtureExchange `json:"exchanges,omitempty"`
	// Contents maps artifact version IDs to inline UTF-8 content for mocks.
	Contents map[string]string `json:"contents,omitempty"`
}

// FixtureExchange documents an operation and its expected outcome.
type FixtureExchange struct {
	Name        string     `json:"name"`
	Op          string     `json:"op"` // RPC op name, e.g. "messages.send"
	Principal   *Principal `json:"principal,omitempty"`
	Request     RawJSON    `json:"request"`
	Response    RawJSON    `json:"response,omitempty"`
	Error       *Error     `json:"error,omitempty"`
	Explanation string     `json:"explanation,omitempty"`
}

// LoadFixture strictly decodes a fixture bundle (unknown fields rejected).
func LoadFixture(path string) (*FixtureBundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeFixture(b)
}

// DecodeFixture strictly decodes and validates a bundle.
func DecodeFixture(b []byte) (*FixtureBundle, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f FixtureBundle
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode fixture: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// LoadFixtureDir loads every *.json bundle in dir, sorted by filename.
func LoadFixtureDir(dir string) ([]*FixtureBundle, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []*FixtureBundle
	for _, p := range paths {
		f, err := LoadFixture(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		out = append(out, f)
	}
	return out, nil
}

// Validate checks bundle-level invariants that every consumer relies on.
func (f *FixtureBundle) Validate() error {
	if f.SchemaVersion != SchemaVersion {
		return fmt.Errorf("fixture %q: schema_version %d != %d", f.Name, f.SchemaVersion, SchemaVersion)
	}
	if !f.Synthetic {
		return fmt.Errorf("fixture %q: synthetic must be true", f.Name)
	}
	seen := map[string]bool{}
	dup := func(id string) error {
		if id == "" {
			return nil
		}
		if seen[id] {
			return fmt.Errorf("fixture %q: duplicate id %s", f.Name, id)
		}
		seen[id] = true
		return nil
	}
	check := func(id, prefix string) error {
		if !ValidID(id, prefix) {
			return fmt.Errorf("fixture %q: id %q lacks prefix %q", f.Name, id, prefix)
		}
		return dup(id)
	}
	for _, n := range f.Nodes {
		if err := check(n.NodeID, PrefixNode); err != nil {
			return err
		}
	}
	for _, s := range f.Sessions {
		if err := check(s.Session.SessionID, PrefixSession); err != nil {
			return err
		}
		if s.Run != nil {
			if err := check(s.Run.RunID, PrefixRun); err != nil {
				return err
			}
			if s.Run.SessionID != s.Session.SessionID {
				return fmt.Errorf("fixture %q: run %s session mismatch", f.Name, s.Run.RunID)
			}
		}
	}
	for _, r := range f.Runs {
		if err := check(r.RunID, PrefixRun); err != nil {
			return err
		}
	}
	for _, m := range f.Messages {
		if err := check(m.Envelope.MessageID, PrefixMessage); err != nil {
			return err
		}
		if m.Status.Runtime == DeliveryAccepted && m.Status.BoundRunID == "" {
			return fmt.Errorf("fixture %q: accepted message %s must be bound to a run", f.Name, m.Envelope.MessageID)
		}
	}
	for _, a := range f.Approvals {
		if err := check(a.ApprovalID, PrefixApproval); err != nil {
			return err
		}
	}
	for _, p := range f.Projects {
		if err := check(p.ProjectID, PrefixProject); err != nil {
			return err
		}
	}
	for _, d := range f.Documents {
		if err := check(d.DocumentID, PrefixDocument); err != nil {
			return err
		}
	}
	for _, r := range f.Revisions {
		if err := check(r.RevisionID, PrefixRevision); err != nil {
			return err
		}
	}
	for _, a := range f.Artifacts {
		if err := check(a.ArtifactID, PrefixArtifact); err != nil {
			return err
		}
	}
	for _, v := range f.Versions {
		if err := check(v.VersionID, PrefixVersion); err != nil {
			return err
		}
	}
	for _, c := range f.Comments {
		if err := check(c.CommentID, PrefixComment); err != nil {
			return err
		}
	}
	for _, a := range f.Accounts {
		if err := check(a.Account.AccountID, PrefixAccount); err != nil {
			return err
		}
	}
	for _, a := range f.Attention {
		if err := check(a.AttentionID, PrefixAttention); err != nil {
			return err
		}
	}
	return nil
}
