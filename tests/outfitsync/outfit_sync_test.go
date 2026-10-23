// Package outfitsync_test runs scripts/outfit-sync.sh against fake ssh, scp,
// outfit and pyre commands: the "remote" is a throwaway home on this
// machine reached through a fake ssh that runs commands in a non-login
// shell (PATH=/usr/bin:/bin, like `ssh host CMD`). Nothing real is
// contacted and no real Outfit vault is touched.
package outfitsync_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func script(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "scripts", "outfit-sync.sh")
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

const fakeSSH = `#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; --) shift; break ;; -*) shift ;; *) break ;; esac; done
shift # host
exec env -i HOME="$FAKE_REMOTE_HOME" PATH=/usr/bin:/bin TMPDIR="$FAKE_REMOTE_TMP" FAKE_STATE="$FAKE_STATE" sh -c "$*"
`

// fakeSCP copies "scp [-q] [-o X] SRC... HOST:DIR/" locally (bash 3.2 safe).
const fakeSCP = `#!/bin/bash
srcs=()
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; -*) shift ;; *) srcs+=("$1"); shift ;; esac; done
last=$((${#srcs[@]} - 1))
dest=${srcs[$last]}
unset "srcs[$last]"
cp "${srcs[@]}" "${dest#*:}"
`

// The remote outfit restores the names in $FAKE_STATE/ok, fails those in
// $FAKE_STATE/fail, and its connector test needs gh (also only in
// ~/.local/bin) and a restored connection.
const remoteOutfit = `#!/bin/sh
case "$1" in
setup|"skill") exit 0 ;;
backup)
  for n in $(cat "$FAKE_STATE/ok" 2>/dev/null); do printf 'connection\tRestoring %s\ndone\t%s is ready\n' "$n" "$n"; echo "$n" >>"$FAKE_STATE/restored"; done
  for n in $(cat "$FAKE_STATE/fail" 2>/dev/null); do printf 'connection\tRestoring %s\nerror: %s: sql: no rows in result set\n' "$n" "$n" >&2; done
  exit "$(cat "$FAKE_STATE/rc" 2>/dev/null || echo 0)" ;;
doctor) echo "vault	file"; exit 0 ;;
connector)
  command -v gh >/dev/null || { echo "gh: command not found"; exit 1; }
  grep -qx "$3" "$FAKE_STATE/restored" 2>/dev/null || { echo "sql: no rows in result set"; exit 1; }
  echo "$3: ok"; exit 0 ;;
esac
exit 2
`

const localOutfit = `#!/bin/sh
case "$1" in
backup) : >"$3"; exit 0 ;;
ls) echo '{"data":[{"name":"github"},{"name":"slack"}]}' ;;
esac
`

// pyre: a connected node when $FAKE_STATE/node exists; connections verify
// answers from $FAKE_STATE/verify-<name> (else "missing", exit 1).
const fakePyre = `#!/bin/bash
args="$*"
case "$args" in
"--json nodes")
  [ -f "$FAKE_STATE/node" ] || exit 1
  echo '{"items":[{"node_id":"node_synth-target","name":"synth-target","connection":{"state":"connected"}}]}' ;;
"--json connections verify "*)
  n=$4
  if [ -f "$FAKE_STATE/verify-$n" ]; then cat "$FAKE_STATE/verify-$n"; exit 0; fi
  echo '[{"node_id":"node_synth-target","mode":"remote_invoke","state":"missing","detail":"connection \"'$n'\" is not configured in Outfit on this node"}]'; exit 1 ;;
*) exit 1 ;;
esac
`

type rig struct {
	dir, state, bin string
	env             []string
}

func newRig(t *testing.T) *rig {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("bash script")
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is required")
		}
	}
	d := t.TempDir()
	r := &rig{dir: d, state: filepath.Join(d, "state"), bin: filepath.Join(d, "bin")}
	remoteHome := filepath.Join(d, "remote-home")
	write(t, filepath.Join(r.bin, "ssh"), fakeSSH)
	write(t, filepath.Join(r.bin, "scp"), fakeSCP)
	write(t, filepath.Join(r.bin, "outfit"), localOutfit)
	write(t, filepath.Join(r.bin, "pyre"), fakePyre)
	write(t, filepath.Join(remoteHome, ".local", "bin", "outfit"), remoteOutfit)
	write(t, filepath.Join(remoteHome, ".local", "bin", "gh"), "#!/bin/sh\nexit 0\n")
	os.MkdirAll(r.state, 0o755)
	os.MkdirAll(filepath.Join(d, "remote-tmp"), 0o755)
	os.MkdirAll(filepath.Join(d, "local-tmp"), 0o755)
	r.env = []string{
		"PATH=" + r.bin + ":/usr/bin:/bin", "HOME=" + filepath.Join(d, "local-home"), "TMPDIR=" + filepath.Join(d, "local-tmp"),
		"FAKE_REMOTE_HOME=" + remoteHome, "FAKE_REMOTE_TMP=" + filepath.Join(d, "remote-tmp"), "FAKE_STATE=" + r.state,
	}
	return r
}

func (r *rig) set(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.state, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script(t)}, args...)...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// A connection whose restore failed is FAILED and never "verified"; one
// that restored is checked for real in an ssh login, where outfit and gh are
// found although the remote shell is not a login shell.
func TestOutfitSyncReportsFailedRestorePerConnection(t *testing.T) {
	r := newRig(t)
	r.set(t, "ok", "github\n")
	r.set(t, "fail", "slack\n")
	r.set(t, "rc", "1")
	out, code := r.run(t, "--via", "ssh", "synth-target")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"verified github on synth-target in an ssh login",
		"FAILED slack on synth-target: not restored",
		"FAILED connections: synth-target:slack",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "verified slack") || strings.Contains(out, "slack verified") {
		t.Fatalf("a connection that failed to restore was called verified:\n%s", out)
	}
}

// --outfit '$HOME/...' (kept literal by quoting) and '~/...' mean the
// target's home.
func TestOutfitSyncExpandsRemoteHome(t *testing.T) {
	r := newRig(t)
	r.set(t, "ok", "github\nslack\n")
	for _, of := range []string{"$HOME/.local/bin/outfit", "~/.local/bin/outfit"} {
		os.Remove(filepath.Join(r.state, "restored"))
		out, code := r.run(t, "--via", "ssh", "--outfit", of, "synth-target")
		if code != 0 || !strings.Contains(out, "verified slack on synth-target") || !strings.Contains(out, "all targets restored and verified") {
			t.Fatalf("--outfit %s: exit %d\n%s", of, code, out)
		}
	}
}

// With a connected Pyre node, the verdict is what pyred sees (Pyre
// sessions); the ssh login result is reported beside it, never as
// "verified" for the node. A pyre verify that finds the connection missing
// is FAILED even though the ssh test passes.
func TestOutfitSyncUsesPyreVerdictOnNode(t *testing.T) {
	r := newRig(t)
	r.set(t, "ok", "github\nslack\n")
	r.set(t, "node", "")
	r.set(t, "verify-github", `[{"node_id":"node_synth-target","mode":"remote_invoke","state":"verified"}]`)
	out, code := r.run(t, "--via", "ssh", "synth-target")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"verified github on synth-target in Pyre sessions",
		"FAILED slack on synth-target in Pyre sessions: missing (connection \"slack\" is not configured in Outfit on this node)",
		"ssh login on synth-target: slack works",
		"FAILED connections: synth-target:slack",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}
