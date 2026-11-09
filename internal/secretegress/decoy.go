package secretegress

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

// DecoyFile is the run-directory file that holds a run's decoys (0600).
const DecoyFile = "decoys.json"

// Decoy credentials (decision I79). Every run gets these variables with
// fresh random values: they are FAKE, never issued by any provider, and no
// Pyre code or tool reads them. They look like the real thing (a GitHub
// fine-grained token, an Anthropic API key) so an agent that goes looking
// for credentials to send out takes them too; any use of one is an alert.
//
// The names are chosen so no tool reads them: GitHub tools read GH_TOKEN /
// GITHUB_TOKEN and Anthropic SDKs ANTHROPIC_API_KEY, never these, so a
// decoy cannot replace a real credential or break a tool.
var DecoyNames = []string{"GITHUB_PAT_BACKUP", "ANTHROPIC_API_KEY_OLD"}

// Decoys are one run's planted credentials: variable name -> value.
type Decoys struct {
	Env map[string]string `json:"env"`
}

const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upalnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64url  = alnum + "-_"
)

func randString(set string, n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(set)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand does not fail on supported platforms
		}
		b[i] = set[k.Int64()]
	}
	return string(b)
}

// NewDecoys makes a run's decoys with fresh random values in the shapes of
// real credentials (random, so they are never anyone's real token).
func NewDecoys() Decoys {
	return Decoys{Env: map[string]string{
		// github_pat_ + 22 (starting "11") + "_" + 59, as fine-grained tokens.
		"GITHUB_PAT_BACKUP": "github_pat_11" + randString(upalnum, 20) + "_" + randString(alnum, 59),
		// sk-ant-api03- + 93 + "AA", as Anthropic API keys.
		"ANTHROPIC_API_KEY_OLD": "sk-ant-api03-" + randString(b64url, 93) + "AA",
	}}
}

// WriteDecoys stores d in dir (the run directory), private to the owner.
func WriteDecoys(dir string, d Decoys) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, DecoyFile), b, 0o600)
}

// ReadDecoys reads a run's decoys; none when the file is missing (a run
// started before decoys existed, or an exec run).
func ReadDecoys(dir string) Decoys {
	if dir == "" {
		return Decoys{}
	}
	b, err := os.ReadFile(filepath.Join(dir, DecoyFile))
	if err != nil {
		return Decoys{}
	}
	var d Decoys
	if json.Unmarshal(b, &d) != nil {
		return Decoys{}
	}
	return d
}

// Plant adds the decoys to env, never replacing a variable already set
// there (a session's own env file wins).
func (d Decoys) Plant(env map[string]string) {
	for k, v := range d.Env {
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
}

// valueIn: a decoy's value (or a long enough piece of it) is in s.
func (d Decoys) valueIn(s string) bool {
	for _, v := range d.Env {
		if len(v) < 24 {
			continue
		}
		// The random part alone is enough: the prefix is shared with real tokens.
		if strings.Contains(s, v[len(v)-24:]) {
			return true
		}
	}
	return false
}

// namesIn returns the decoy names s mentions as whole identifiers.
func (d Decoys) namesIn(s string) []string {
	var out []string
	for name := range d.Env {
		for i := 0; ; {
			j := strings.Index(s[i:], name)
			if j < 0 {
				break
			}
			at := i + j
			end := at + len(name)
			if (at == 0 || !identByte(s[at-1])) && (end == len(s) || !identByte(s[end])) {
				out = append(out, name)
				break
			}
			i = at + 1
		}
	}
	return out
}

func (d Decoys) isName(n string) bool {
	_, ok := d.Env[n]
	return ok
}

func identByte(c byte) bool {
	return c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9'
}

// maxCommand bounds the command text kept in an alert.
const maxCommand = 1000

// Redact masks credentials in a command for an alert: decoy values,
// token shapes and secret-named assignments (credentials.RedactTranscript).
// Variable references ($GH_TOKEN) are names, not values, and are kept.
func Redact(cmd string, d Decoys) string {
	for _, v := range d.Env {
		if v != "" {
			cmd = strings.ReplaceAll(cmd, v, "[DECOY]")
		}
		if len(v) >= 24 {
			cmd = strings.ReplaceAll(cmd, v[len(v)-24:], "[DECOY]")
		}
	}
	cmd = credentials.RedactTranscript(cmd)
	if r := []rune(cmd); len(r) > maxCommand {
		cmd = string(r[:maxCommand-1]) + "…"
	}
	return cmd
}

// Alert is the finding as a contracts.SecurityAlert (the daemon fills the
// run, session and node).
func (f *Finding) Alert(id, tool, cmd string, d Decoys, at time.Time) contracts.SecurityAlert {
	return contracts.SecurityAlert{
		AlertID: id, Tool: tool, Rule: f.Rule, Severity: f.Severity,
		Secrets: f.Secrets, Sinks: f.Sinks, Hosts: f.Hosts, Encoded: f.Encoded,
		Command: Redact(cmd, d), Reason: f.Reason(), At: at.UTC(),
	}
}
