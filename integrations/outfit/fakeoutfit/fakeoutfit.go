// Package fakeoutfit is a deterministic stand-in for the Outfit CLI used by
// Pyre tests. It holds SYNTHETIC credentials only and implements the subset of
// commands the adapter calls: version, inventory --json, ls --json, tool
// status, skill list, connector test, and run ... --input - --json.
//
// State comes from the JSON file named by FAKE_OUTFIT_STATE and is re-read on
// every call, so tests can change it between invocations (expire, lock,
// remove). Every invocation's argv and stdin size is appended to
// FAKE_OUTFIT_LOG so tests can prove secrets never reach argv.
package fakeoutfit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// State is the fake's world.
type State struct {
	Version     string       `json:"version"`            // e.g. "0.10.7"
	Revision    string       `json:"revision,omitempty"` // e.g. "c8a3dd7+working"
	NoInventory bool         `json:"no_inventory"`       // emulate Outfit before `inventory`
	Locked      bool         `json:"locked"`             // emulate a locked OS keychain
	Leak        bool         `json:"leak"`               // adversarial: echo the secret in failures
	Connections []Connection `json:"connections"`
	Tools       []Tool       `json:"tools,omitempty"`
	Skills      []Skill      `json:"skills,omitempty"`
}

type Connection struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	Connector   string                     `json:"connector"`
	Mode        string                     `json:"mode"`
	Method      string                     `json:"method"`
	Custody     string                     `json:"custody"`
	Status      string                     `json:"status"` // "active" | "repair required" ...
	Expired     bool                       `json:"expired"`
	Env         []string                   `json:"env,omitempty"`
	Secret      string                     `json:"secret,omitempty"` // SYNTHETIC; the fake's "keychain"
	Operations  map[string]json.RawMessage `json:"operations,omitempty"`
	WriteOps    []string                   `json:"write_ops,omitempty"`
	VerifiedAt  *time.Time                 `json:"verified_at,omitempty"`
	SleepMillis int                        `json:"sleep_millis,omitempty"`
}

type Tool struct {
	ID, Version, Platform, Path, SHA256 string
	Current                             bool
}

type Skill struct {
	ID, Connection, Connector, Host, Kind, Source, Ref, Digest, Path string
}

func load(getenv func(string) string) (State, error) {
	var st State
	raw, err := os.ReadFile(getenv("FAKE_OUTFIT_STATE"))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

// Save writes a state file (test helper).
func Save(path string, st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func logCall(getenv func(string) string, args []string, stdinLen int) {
	path := getenv("FAKE_OUTFIT_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{"argv": args, "stdin_bytes": stdinLen, "env_has_pyre_token": getenv("PYRE_SESSION_TOKEN") != ""})
	f.Write(append(line, '\n'))
}

type envelope struct {
	Schema     string         `json:"schema"`
	OK         bool           `json:"ok"`
	Connection string         `json:"connection,omitempty"`
	Operation  string         `json:"operation,omitempty"`
	Class      string         `json:"class,omitempty"`
	Data       any            `json:"data,omitempty"`
	Meta       map[string]any `json:"meta"`
	Error      map[string]any `json:"error,omitempty"`
}

const resultSchema = "outfit.jhh3.dev/result/v1"

// Main runs the fake. It returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	var in []byte
	if len(args) > 0 && args[0] == "run" {
		in, _ = io.ReadAll(io.LimitReader(stdin, 1<<20))
	}
	logCall(getenv, args, len(in))
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: outfit COMMAND")
		return 2
	}
	st, err := load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "fake outfit: state: %v\n", err)
		return 1
	}
	fail := func(op string, code, msg string) int {
		json.NewEncoder(stdout).Encode(envelope{Schema: resultSchema, OK: false, Operation: op, Meta: map[string]any{"pages": 0}, Error: map[string]any{"code": code, "message": msg}})
		return 1
	}
	find := func(name string) (Connection, bool) {
		for _, c := range st.Connections {
			if c.Name == name {
				return c, true
			}
		}
		return Connection{}, false
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "outfit %s (%s, 2026-09-21T18:33:02Z)\n", st.Version, orDefault(st.Revision, "fake000"))
		return 0
	case "inventory":
		if st.NoInventory {
			return fail("inventory", "operation_failed", `unknown command "inventory" for "outfit"`)
		}
		doc := map[string]any{"schema": "outfit.jhh3.dev/inventory/v1", "version": st.Version}
		var conns []map[string]any
		for _, c := range st.Connections {
			m := map[string]any{"id": c.ID, "name": c.Name, "connector": c.Connector, "mode": c.Mode, "executor": "cli", "credential_method": c.Method, "custody": c.Custody, "status": c.Status, "environment": orEmpty(c.Env), "operations": len(c.Operations)}
			if c.VerifiedAt != nil {
				m["last_verified_at"] = c.VerifiedAt
			}
			conns = append(conns, m)
		}
		doc["connections"] = orEmptyAny(conns)
		var tools, skills []map[string]any
		for _, t := range st.Tools {
			tools = append(tools, map[string]any{"id": t.ID, "version": t.Version, "platform": t.Platform, "path": t.Path, "sha256": t.SHA256, "current": t.Current, "installed_at": time.Unix(0, 0).UTC()})
		}
		for _, s := range st.Skills {
			skills = append(skills, map[string]any{"id": s.ID, "connection": s.Connection, "connector": s.Connector, "host": s.Host, "kind": s.Kind, "source": s.Source, "ref": s.Ref, "digest": s.Digest, "path": s.Path, "enabled": true})
		}
		doc["tools"], doc["skills"] = orEmptyAny(tools), orEmptyAny(skills)
		json.NewEncoder(stdout).Encode(doc)
		return 0
	case "ls":
		var rows []map[string]any
		for _, c := range st.Connections {
			rows = append(rows, map[string]any{"name": c.Name, "connector": c.Connector, "display": c.Connector, "mode": c.Mode, "status": c.Status, "operations": len(c.Operations)})
		}
		json.NewEncoder(stdout).Encode(envelope{Schema: resultSchema, OK: true, Operation: "ls", Class: "read", Data: orEmptyAny(rows), Meta: map[string]any{"pages": 1, "items": len(rows)}})
		return 0
	case "tool":
		for _, t := range st.Tools {
			cur := ""
			if t.Current {
				cur = "current"
			}
			fmt.Fprintf(stdout, "%s %s %s %-7s %-7s %s\n", t.ID, t.Version, t.Platform, "managed", cur, t.Path)
		}
		return 0
	case "skill":
		for _, s := range st.Skills {
			fmt.Fprintf(stdout, "%-8s %-20s %-32s %-18s %-12s %s\n", s.Host, s.Connection, s.ID, s.Kind, s.Ref, s.Path)
		}
		return 0
	case "connector":
		if len(args) < 3 || args[1] != "test" {
			fmt.Fprintln(stderr, "fake outfit: unsupported connector subcommand")
			return 2
		}
		c, ok := find(args[2])
		switch {
		case !ok:
			// What the pinned real Outfit (3c7577f) prints for an unknown name.
			fmt.Fprintln(stderr, "Error: sql: no rows in result set")
			return 1
		case st.Locked:
			fmt.Fprintln(stderr, "OS keychain: User interaction is not allowed.")
			return 1
		case c.Expired:
			msg := "provider returned 401 Unauthorized: token expired; run outfit credential reauthenticate " + c.Name
			if st.Leak {
				msg += " (sent Authorization: Bearer " + c.Secret + ")"
			}
			fmt.Fprintln(stderr, msg)
			return 1
		}
		fmt.Fprintf(stdout, "%s is healthy\n", c.Name)
		return 0
	case "run":
		if len(args) < 3 {
			return fail("run", "invalid_arguments", "run requires CONNECTION OPERATION")
		}
		c, ok := find(args[1])
		if !ok {
			return fail(args[2], "operation_failed", "connection "+args[1]+" was not found; run `outfit ls` to list active names")
		}
		if c.SleepMillis > 0 {
			time.Sleep(time.Duration(c.SleepMillis) * time.Millisecond)
		}
		for _, w := range c.WriteOps {
			if w == args[2] {
				return fail(args[2], "write_confirmation_required", args[2]+" is a write operation; run `outfit write` only when the human requested this mutation")
			}
		}
		if st.Locked {
			return fail(args[2], "operation_failed", "OS keychain: User interaction is not allowed.")
		}
		if c.Expired {
			msg := "provider returned 401"
			if st.Leak {
				msg += " for token " + c.Secret
			}
			return fail(args[2], "operation_failed", msg)
		}
		data, ok := c.Operations[args[2]]
		if !ok {
			return fail(args[2], "unknown_operation", "unknown operation "+args[2])
		}
		var input map[string]string
		if len(in) > 0 {
			json.Unmarshal(in, &input)
		}
		var payload any
		json.Unmarshal(data, &payload)
		if s, isStr := payload.(string); isStr && strings.HasPrefix(s, "BIG:") {
			payload = strings.Repeat("x", 200000)
			path := os.TempDir() + "/outfit-result-fake.json"
			os.WriteFile(path, []byte(`{"spilled":"content"}`), 0o600)
			json.NewEncoder(stdout).Encode(envelope{Schema: resultSchema, OK: true, Connection: c.Name, Operation: args[2], Class: "read", Data: map[string]any{"spilled": true, "path": path}, Meta: map[string]any{"pages": 1, "spill": map[string]any{"path": path, "bytes": 200000, "sha256": "abc"}}})
			return 0
		}
		json.NewEncoder(stdout).Encode(envelope{Schema: resultSchema, OK: true, Connection: c.Name, Operation: args[2], Class: "read", Data: map[string]any{"result": payload, "args": input}, Meta: map[string]any{"pages": 1, "items": 1}})
		return 0
	}
	fmt.Fprintf(stderr, "Error: unknown command %q for \"outfit\"\n", args[0])
	return 1
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyAny(s []map[string]any) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	return s
}

// EnvSwitch makes a test binary behave as the fake when set to "1".
const EnvSwitch = "PYRE_FAKE_OUTFIT"

// RunIfRequested belongs in a test package's TestMain: when the test binary
// was launched as the fake Outfit (EnvSwitch=1) it runs Main and exits, so
// the adapter can exec the test binary itself as "outfit" without a shell.
func RunIfRequested() {
	if os.Getenv(EnvSwitch) == "1" {
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
}

// Env returns the child environment entries selecting the fake's state/log.
func Env(statePath, logPath string) []string {
	return []string{EnvSwitch + "=1", "FAKE_OUTFIT_STATE=" + statePath, "FAKE_OUTFIT_LOG=" + logPath}
}
