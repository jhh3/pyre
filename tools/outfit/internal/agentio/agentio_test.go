package agentio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/invoke"
)

func TestEnabledIsExplicitForInMemoryWriters(t *testing.T) {
	var output bytes.Buffer
	t.Setenv("OUTFIT_AGENT", "")
	if Enabled(&output) {
		t.Fatal("in-memory output should stay human-readable by default")
	}
	t.Setenv("OUTFIT_AGENT", "yes")
	if !Enabled(&output) {
		t.Fatal("explicit agent mode was ignored")
	}
	t.Setenv("OUTFIT_AGENT", "0")
	if Enabled(&output) {
		t.Fatal("explicit human mode was ignored")
	}
}

func TestPrepareSpillsCompleteEnvelopeToPrivateFile(t *testing.T) {
	t.Setenv("OUTFIT_AGENT_MAX_BYTES", "4096")
	original := invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "large", Class: "read", Data: strings.Repeat("evidence", 1024), Meta: invoke.Meta{Pages: 1, Items: 1}}
	compact, encoded, err := Prepare(original, true)
	if err != nil {
		t.Fatal(err)
	}
	if compact.Meta.Spill == nil || compact.Meta.Spill.Path == "" {
		t.Fatalf("missing spill receipt: %s", encoded)
	}
	defer os.Remove(compact.Meta.Spill.Path)
	info, err := os.Stat(compact.Meta.Spill.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode = %o", info.Mode().Perm())
	}
	full, err := os.ReadFile(compact.Meta.Spill.Path)
	if err != nil {
		t.Fatal(err)
	}
	var restored invoke.Envelope
	if err := json.Unmarshal(full, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Operation != original.Operation || restored.Data != original.Data || restored.Meta.Spill != nil {
		t.Fatalf("spill did not preserve the original envelope: %#v", restored)
	}
	sum := sha256.Sum256(full)
	if compact.Meta.Spill.Bytes != len(full) || compact.Meta.Spill.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("bad spill integrity receipt: %#v", compact.Meta.Spill)
	}
}

func TestPrepareKeepsSmallOrExplicitHumanResultsInline(t *testing.T) {
	t.Setenv("OUTFIT_AGENT_MAX_BYTES", "4096")
	small := invoke.Envelope{Schema: invoke.Schema, OK: true, Data: "small", Meta: invoke.Meta{Pages: 1}}
	prepared, _, err := Prepare(small, true)
	if err != nil || prepared.Meta.Spill != nil {
		t.Fatalf("small result spilled: %#v %v", prepared, err)
	}
	large := small
	large.Data = strings.Repeat("large", 1024)
	prepared, _, err = Prepare(large, false)
	if err != nil || prepared.Meta.Spill != nil {
		t.Fatalf("human result spilled: %#v %v", prepared, err)
	}
}

func TestRequestedRequiresExactJSONFlag(t *testing.T) {
	if !Requested([]string{"run", "github", "get", "--json"}) || Requested([]string{"--json-args", "{}"}) {
		t.Fatal("JSON flag detection is not exact")
	}
}
