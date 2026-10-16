package assemble

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// TestListSessionsLoad measures sessions.list on a synthetic busy hub
// (W-dbhygiene): ~450 sessions, ~80 live with runs on one node, a large
// node status blob, attention items, leases, pending approvals, a bloated
// commands table and event log, the background attention deriver and
// maintenance running, and node traffic (run reports, non-attention
// events) arriving while the list is served. Synthetic data only.
//
//	PYRE_LISTLOAD=1 go test ./internal/hub/assemble -run TestListSessionsLoad -v
func TestListSessionsLoad(t *testing.T) {
	if os.Getenv("PYRE_LISTLOAD") == "" {
		t.Skip("set PYRE_LISTLOAD=1 to run the sessions.list load measurement")
	}
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	live := seedListLoad(t, st, n.nodeID)
	st.Start(bg)

	// Node traffic while lists are served: a run report (activity change,
	// no event) and a non-attention event every 25ms.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seq := uint64(1_000_000)
		tk := time.NewTicker(25 * time.Millisecond)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			r := live[i%len(live)]
			r.Activity = &c.RunActivity{Text: fmt.Sprintf("synthetic step %d", i), Since: n.clock.Now()}
			seq++
			_, _ = st.Hub.HandleReport(bg, n.nodeID, c.NodeReport{OriginSeq: seq, Kind: c.ReportRun, Payload: c.RawJSON(persistence.JSON(r))})
			_ = sqlitex.Tx(bg, st.DB, func(tx *sql.Tx) error {
				_, err := st.Events.AppendTx(bg, tx, c.Event{Kind: c.EvNodeStatus, Object: c.ObjectRef{Kind: c.KindNode, ID: n.nodeID}, Payload: c.RawJSON(`{"synthetic":true}`)})
				return err
			})
			st.Events.Notify()
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	var times []time.Duration
	for i := 0; i < 15; i++ {
		t0 := time.Now()
		p, err := st.Hub.ListSessions(owner, c.ListSessionsRequest{PageRequest: c.PageRequest{Limit: 200}})
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(t0))
		if len(p.Items) != len(live) {
			t.Fatalf("listed %d sessions, want %d", len(p.Items), len(live))
		}
		time.Sleep(50 * time.Millisecond)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("sessions.list (%d live of 450): min %v median %v max %v", len(live), times[0], times[len(times)/2], times[len(times)-1])
}

// seedListLoad writes the synthetic hub state and returns the live runs.
func seedListLoad(t *testing.T, st *Stack, nodeID string) []c.Run {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	pad := strings.Repeat("x", 400)
	var live []c.Run
	usage := []c.RunUsage{}
	err := sqlitex.Tx(ctx, st.DB, func(tx *sql.Tx) error {
		for i := 0; i < 450; i++ {
			sid := fmt.Sprintf("ses_synthload%04d", i)
			rid := fmt.Sprintf("run_synthload%04d", i)
			archived := i >= 80
			state := c.RunRunning
			if archived {
				state = c.RunExited
			} else if i%3 == 1 {
				state = c.RunWaitingInput
			}
			r := c.Run{RunID: rid, SessionID: sid, NodeID: nodeID, Adapter: "mock", Harness: c.HarnessMock, Cwd: "/tmp/synth/" + sid,
				State: state, StartedAt: now.Add(-time.Duration(i) * time.Minute), Capabilities: mockCaps,
				Freshness:   c.Freshness{ObservedAt: now, Source: "node:" + nodeID},
				AdapterData: map[string]string{"synthetic.pad": pad}}
			if state == c.RunRunning {
				r.Activity = &c.RunActivity{Text: "synthetic work", Since: now}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id,harness,title,project_id,created_by,created_at,current_run_id,lineage,tags,archived,updated_at,icon) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
				sid, string(c.HarnessMock), "synthetic session "+sid, nil, persistence.JSON(ownerP), persistence.Time(r.StartedAt), rid, persistence.JSON(c.Lineage{}), `[]`, boolInt(archived), persistence.Time(now), ""); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO runs(run_id,session_id,node_id,state,run,origin_seq,start_command_id,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				rid, sid, nodeID, string(state), persistence.JSON(r), 0, nil, persistence.Time(r.StartedAt), persistence.Time(now)); err != nil {
				return err
			}
			if archived {
				continue
			}
			live = append(live, r)
			usage = append(usage, c.RunUsage{RunID: rid, SessionID: sid, Procs: 7, RSSMB: 812.5, CPUPct: 12.5, ObservedAt: now,
				Top: &c.ProcUsage{PID: 4000 + i, RSSMB: 600}})
			if i%4 == 0 { // pending approval
				a := c.Approval{ApprovalID: fmt.Sprintf("apr_synthload%04d", i), SessionID: sid, RunID: rid, State: c.ApprovalPending, CreatedAt: now}
				if _, err := tx.ExecContext(ctx, `INSERT INTO approvals(approval_id,session_id,run_id,node_id,state,approval,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
					a.ApprovalID, sid, rid, nodeID, string(a.State), persistence.JSON(a), persistence.Time(now), persistence.Time(now)); err != nil {
					return err
				}
			}
			if i%8 == 0 { // active input lease
				if _, err := tx.ExecContext(ctx, `INSERT INTO leases(lease_id,run_id,holder,holder_key,acquired_at,renewed_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
					fmt.Sprintf("lse_synthload%04d", i), rid, persistence.JSON(ownerP), persistence.PrincipalKey(ownerP), persistence.Time(now), persistence.Time(now), persistence.Time(now.Add(time.Hour))); err != nil {
					return err
				}
			}
		}
		// A large node status blob (~18KB) with per-run usage.
		stj := persistence.JSON(c.NodeStatusReport{Resources: c.NodeResources{CPUCount: 32, Load1: 9.5, MemTotalMB: 128000, MemAvailMB: 30000, ActiveRuns: len(usage), Runs: usage, ObservedAt: now},
			Versions: map[string]string{"synthetic.pad": strings.Repeat("v", 4000)}})
		if _, err := tx.ExecContext(ctx, `UPDATE node_state SET status=? WHERE node_id=?`, stj, nodeID); err != nil {
			return err
		}
		// Bloated finished commands (transcript reads) and event log.
		big := strings.Repeat("t", 16<<10)
		for i := 0; i < 3000; i++ {
			at := persistence.Time(now.Add(-time.Duration(i) * time.Minute))
			if _, err := tx.ExecContext(ctx, `INSERT INTO commands(command_id,node_id,seq,kind,actor,issued_at,payload,state,ref_kind,ref_id,completed_at,outcome,result) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				fmt.Sprintf("cmd_synthload%05d", i), nodeID, 1_000_000+i, string(c.CmdReadTranscript), "owner:owner", at, `{"session_id":"x"}`, "done", "run", live[i%len(live)].RunID, at, "succeeded", `{"entries":"`+big+`"}`); err != nil {
				return err
			}
		}
		for i := 0; i < 3000; i++ {
			if _, err := st.Events.AppendTx(ctx, tx, c.Event{Kind: c.EvCommandResult, Object: c.ObjectRef{Kind: c.KindCommand, ID: fmt.Sprintf("cmd_synthload%05d", i)},
				Payload: c.RawJSON(`{"result":"` + big[:8<<10] + `"}`)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Attention items for a third of the live sessions.
	for i, r := range live {
		if i%3 != 0 {
			continue
		}
		sctx := c.WithPrincipal(ctx, c.Principal{Kind: c.PrincipalSession, ID: r.SessionID})
		if _, err := st.Attention.FlagAttention(sctx, c.FlagAttentionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: r.SessionID, Text: "synthetic: needs a look"}); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
