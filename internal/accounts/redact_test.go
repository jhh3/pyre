package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A13: token values never reach logs, errors, reports, action results or
// the node database — even when a misbehaving server echoes them back.
func TestTokensAbsentFromLogsErrorsReportsAndDB(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.echoToken = true
	labels := []string{"A", "B", "C"}
	for i, l := range labels {
		e.addAccount(fmt.Sprintf("acct_synth-%d", i), contracts.HarnessClaude, l)
	}
	px := e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	e.reader.resp[px.Dir] = `not json at all`
	e.fake.status["A"] = 401
	e.fake.status["B"] = 500
	e.svc.ObserveDue(ctx, "")
	e.observe("acct_synth-2", contracts.ProviderAnthropic, 10, time.Minute)

	// Failure paths that format errors.
	_, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-0"), "run_0")
	e.err(err)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-2"), claudeCaps)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitAuthInvalid,
		Detail: "401 Bearer " + synthAccess("C") + " refresh_token=" + synthRefresh("C") + " Please run /login"})
	e.err(err)
	pb, _ := json.Marshal(plan)
	var outputs []string
	outputs = append(outputs, string(pb))
	for _, name := range []string{contracts.ActionAccountList, contracts.ActionAccountObserve} {
		out, err := e.svc.Handlers()[name](ctx, nil)
		e.err(err)
		b, _ := json.Marshal(out)
		outputs = append(outputs, string(b))
	}
	e.svc.db.Exec(`PRAGMA wal_checkpoint(FULL)`)

	haystacks := map[string]string{
		"logs":    e.logs.String(),
		"errors":  strings.Join(e.errs, "\n"),
		"reports": string(e.rep.all()),
		"outputs": strings.Join(outputs, "\n"),
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if b, err := os.ReadFile(e.dbPath + suffix); err == nil {
			haystacks["db"+suffix] = string(b)
		}
	}
	var needles []string
	for _, l := range append(labels, "X", "DEFAULT") {
		needles = append(needles, synthAccess(l), synthRefresh(l))
	}
	// Codex JWT access tokens too.
	if b, err := os.ReadFile(filepath.Join(px.Dir, "auth.json")); err == nil {
		var a codexAuth
		json.Unmarshal(b, &a)
		needles = append(needles, a.Tokens.AccessToken, a.Tokens.IDToken)
	}
	for where, hay := range haystacks {
		for _, n := range needles {
			if n != "" && strings.Contains(hay, n) {
				t.Errorf("token %q leaked into %s", n[:20], where)
			}
		}
		if strings.Contains(hay, "pyre-synthetic-") {
			t.Errorf("synthetic credential material in %s", where)
		}
	}
	if len(e.logs.String()) == 0 || len(e.rep.all()) == 0 {
		t.Fatal("test produced no logs/reports to inspect")
	}
}

func TestRedactAndSecret(t *testing.T) {
	s := secret{"pyre-synthetic-Q-access-initial"}
	if fmt.Sprintf("%v %s %+v %#v", s, s, s, s) != "[redacted] [redacted] [redacted] [redacted]" {
		t.Fatal("secret formatting leaks")
	}
	if b, _ := json.Marshal(struct{ S secret }{s}); strings.Contains(string(b), "pyre-synthetic") {
		t.Fatal("secret JSON leaks")
	}
	in := `Authorization: Bearer abc.def-ghi {"access_token":"tok123","refreshToken":"r456"} sk-ant-oat01-xyz eyJhbGciOi.eyJzdWIiOiJ4In0.sig pyre-synthetic-Z-refresh-g2`
	out := redact(in)
	for _, bad := range []string{"abc.def-ghi", "tok123", "r456", "sk-ant-oat01-xyz", "eyJzdWIiOiJ4In0", "pyre-synthetic-Z"} {
		if strings.Contains(out, bad) {
			t.Fatalf("redact left %q in %q", bad, out)
		}
	}
}
