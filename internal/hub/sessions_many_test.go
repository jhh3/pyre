package hub

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestGetManySessions(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	a := x.start(owner, "op_gm_a", nodeMac)
	x.clock.Add(time.Second)
	b := x.start(owner, "op_gm_b", nodeMac)
	arch := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_gm_arch"}, SessionID: b.Session.SessionID, Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	aID, bID := a.Session.SessionID, b.Session.SessionID
	resp, err := x.h.GetManySessions(as(owner), contracts.GetManySessionsRequest{IDs: []string{bID, "ses_synth-missing", aID, aID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 || resp.Items[0].Session.SessionID != bID || resp.Items[1].Session.SessionID != aID {
		t.Fatalf("items (request order, deduped, archived included): %+v", resp.Items)
	}
	if !resp.Items[0].Session.Archived {
		t.Fatal("archived session should be returned with archived=true")
	}
	if !slices.Equal(resp.Missing, []string{"ses_synth-missing"}) {
		t.Fatalf("missing: %v", resp.Missing)
	}
	// Same view as sessions.get.
	one, err := x.h.GetSession(as(owner), contracts.GetRequest{ID: aID})
	if err != nil {
		t.Fatal(err)
	}
	if one.Run == nil || resp.Items[1].Run == nil || one.Run.RunID != resp.Items[1].Run.RunID || one.NodeName != resp.Items[1].NodeName {
		t.Fatalf("get_many view differs from get: %+v vs %+v", resp.Items[1], one)
	}

	ids := make([]string, contracts.MaxGetManySessions+1)
	for i := range ids {
		ids[i] = aID
	}
	if _, err := x.h.GetManySessions(as(owner), contracts.GetManySessionsRequest{IDs: ids}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := x.h.GetManySessions(context.Background(), contracts.GetManySessionsRequest{IDs: []string{aID}}); err == nil {
		t.Fatal("no principal must be refused")
	}
	hello, err := x.h.Hello(as(owner), contracts.HelloRequest{Protocol: contracts.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hello.Features, contracts.FeatureSessionsGetMany) {
		t.Fatalf("features: %v", hello.Features)
	}
}
