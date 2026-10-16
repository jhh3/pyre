//go:build !windows

package security

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/integrations/outfit"
	"github.com/jhh3/pyre/internal/connections"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
	"github.com/jhh3/pyre/internal/diagnostics"
)

// Synthetic credentials (assembled at runtime; obviously fake).
var (
	synthProviderToken = "ghp_" + strings.Repeat("S", 36)    // pattern-recognisable
	synthOpaqueSecret  = "SYNTHETIC-opaque-provisioned-0042" // only known-value detection finds this
)

// fakeOutfit is an adversarial Outfit: its failures echo a provider token.
type fakeOutfit struct{ leak string }

func (f fakeOutfit) Connections(context.Context) ([]outfit.Connection, error) {
	return []outfit.Connection{{Name: "github-work", Connector: "github", Mode: "rest", Custody: "outfit", Status: "active", Environment: []string{"GH_TOKEN"}}}, nil
}

func (f fakeOutfit) Check(context.Context, string) (outfit.Check, error) {
	return outfit.Check{Name: "github-work", State: "expired", Detail: "provider said: bad credentials token=" + f.leak, CheckedAt: time.Now()}, nil
}

func (f fakeOutfit) Invoke(context.Context, outfit.InvokeRequest) (outfit.InvokeResult, error) {
	return outfit.InvokeResult{OK: false, Failure: &outfit.Failure{Code: "auth_failed", Message: "401 from api with Authorization: Bearer " + f.leak + " token=" + f.leak, Hint: "token " + f.leak}}, nil
}

// TestCredentialNonDisclosure: provisioned credential values reach only the
// launch environment map. Manifests, launch plans, connection reports,
// availability checks, invoke errors, node logs, hub catalog rows and
// connection events never contain them — including when the store is
// locked and when Outfit echoes a token in its failure text.
func TestCredentialNonDisclosure(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	nodeID, _ := f.enrollNode("cred-node")
	connID := outfit.ConnectionIDFor("github-work")
	localConn := "conn_local-db"

	store := credentials.NewMemoryStore()
	ref := credentials.NewRef(credentials.BackendMemory, localConn, "DB_PASSWORD")
	if err := store.Put(ctx, ref, credentials.NewSecret(synthOpaqueSecret)); err != nil {
		t.Fatal(err)
	}
	ix := credentials.OpenIndex(filepath.Join(t.TempDir(), "index.json"))
	if err := ix.Upsert(credentials.Provision{ConnectionID: localConn, EnvVar: "DB_PASSWORD", Ref: ref.String(), ProvisionedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	nodeLogs := diagnostics.NewRing(5000, 4<<20, nil)
	allow := connections.GrantCheckerFunc(func(_ context.Context, c, s string) (connections.GrantDecision, error) {
		return connections.GrantDecision{ConnectionID: c, SessionID: s, Allowed: true, GrantID: "grant_synthetic", Generation: 1, CheckedAt: time.Now()}, nil
	})
	svc, err := connections.NewNodeService(connections.NodeConfig{
		NodeID: nodeID, Outfit: fakeOutfit{leak: synthProviderToken}, Stores: credentials.Stores{}.Add(store), Index: ix,
		DefaultBackend: credentials.BackendMemory, Grants: allow,
		Logger: slog.New(slog.NewTextHandler(nodeLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.remember(synthOpaqueSecret, synthProviderToken)
	var surfaces = map[string][]byte{}
	capture := func(name string, v any, err error) {
		b, _ := json.Marshal(v)
		if err != nil {
			b = append(b, []byte(" ERR "+err.Error())...)
			if ce := contracts.AsError(err); ce != nil {
				eb, _ := json.Marshal(ce)
				b = append(b, eb...)
			}
		}
		surfaces[name] = b
	}

	sid := "ses_" + "credtest01"
	plan, err := svc.PrepareLaunch(ctx, sid, []string{localConn, connID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Env["DB_PASSWORD"] != synthOpaqueSecret {
		t.Fatal("launch env lacks the provisioned value (test would be vacuous)")
	}
	capture("launch-plan", plan, nil)
	capture("manifest-names", plan.Manifest.EnvNames(), nil)
	rep, err := svc.Report(ctx)
	capture("connections-report", rep, err)
	// (The Outfit-backed check is covered by TestConnectionCheckDetailRedacted.)
	av0, err := svc.HandleCheck(ctx, json.RawMessage(`{"connection_id":"`+localConn+`"}`))
	capture("check-local", av0, err)
	res, err := svc.HandleInvoke(ctx, json.RawMessage(fmt.Sprintf(`{"connection_id":%q,"session_id":%q,"operation":"get_issue"}`, connID, sid)))
	if err == nil {
		t.Fatal("adversarial invoke should fail")
	}
	capture("invoke-error", res, err)

	store.SetLocked(true)
	_, err = svc.PrepareLaunch(ctx, sid, []string{localConn})
	if err == nil {
		t.Fatal("locked store must fail the launch closed")
	}
	capture("locked-launch-error", nil, err)
	av, err := svc.HandleCheck(ctx, json.RawMessage(`{"connection_id":"`+localConn+`"}`))
	capture("locked-check", av, err)

	// Secret values refuse serialization and print redacted.
	sec := credentials.NewSecret(synthOpaqueSecret)
	if _, err := json.Marshal(struct{ S credentials.Secret }{sec}); err == nil {
		t.Fatal("Secret marshalled to JSON")
	}
	surfaces["fmt-secret"] = []byte(fmt.Sprintf("%v %s %+v %#v %q", sec, sec, sec, sec, sec))

	// Hub side: the catalog receives node reports and grants; scan its rows
	// and events in the hub database.
	cat, err := connections.Open(ctx, f.db, connections.Options{Events: sink{}})
	if err != nil {
		t.Fatal(err)
	}
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: f.owner.ClientID}
	if _, err := cat.Upsert(ctx, owner, connections.ConnectionSpec{Mutation: op(), ConnectionID: connID, Name: "github-work", Service: "github", Modes: []string{outfit.ModeRemoteInvoke}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Grant(ctx, owner, connections.GrantRequest{Mutation: op(), ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: connID, NodeID: nodeID}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.ApplyReport(ctx, nodeID, rep); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Revoke(ctx, owner, connections.GrantRequest{Mutation: op(), ConnectionGrantParams: contracts.ConnectionGrantParams{ConnectionID: connID, NodeID: nodeID}}); err != nil {
		t.Fatal(err)
	}
	surfaces["node-logs"] = []byte(nodeLogs.String())
	if nodeLogs.Stats().TotalLines == 0 {
		t.Fatal("node service logged nothing; log scan would be vacuous")
	}
	f.rpc(f.owner.Token, contracts.OpListConnections, contracts.ListConnectionsRequest{}) // exercise hub access logging
	f.assertNoLeaks(surfaces)
}

// TestConnectionCheckDetailRedacted: connection.check results flow to the
// hub, CLI and Android. Outfit-derived detail text must be redacted at the
// Pyre boundary like every other Outfit-derived string (errors already go
// through credentials.Diagnostic).
func TestConnectionCheckDetailRedacted(t *testing.T) {
	nodeID := "node_cred-check"
	svc, err := connections.NewNodeService(connections.NodeConfig{
		NodeID: nodeID, Outfit: fakeOutfit{leak: synthProviderToken},
		Grants: connections.GrantCheckerFunc(func(context.Context, string, string) (connections.GrantDecision, error) {
			return connections.GrantDecision{Allowed: true}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	av, err := svc.HandleCheck(context.Background(), json.RawMessage(`{"connection_id":"`+outfit.ConnectionIDFor("github-work")+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(av)
	if f := diagnostics.Secrets(diagnostics.NewScanner().AddKnown("token", synthProviderToken).ScanBytes("check", b)); len(f) > 0 {
		t.Fatalf("connection check result leaks Outfit detail: %v", f)
	}
}
