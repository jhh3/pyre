//go:build windows

package fileperm

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func ownerOf(t *testing.T, path string) *windows.SID {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := sd.Owner()
	if err != nil || o == nil {
		t.Fatalf("owner: %v", err)
	}
	return o
}

// A private file and directory are owned by the current user, also when the
// process is an elevated administrator whose new objects default to
// BUILTIN\Administrators (the case that broke a node enrolled over sshd).
func TestPrivateFilesOwnedByUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cred.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestrictFile(f, 0o600); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Restrict(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	user := current().user
	for _, p := range []string{path, dir} {
		if o := ownerOf(t, p); !o.Equals(user) {
			t.Errorf("%s owned by %s, want the current user %s", p, describe(o), describe(user))
		}
	}
	if desc, bad := Exposed(path, nil); bad {
		t.Errorf("Exposed: %s", desc)
	}
}

func dacl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// UserOnlyDir leaves exactly the ACL Codex's app-server accepts on its
// socket directory (codex-rs/uds windows_socket_validation.rs): owner the
// user, protected DACL, one (OI)(CI) full-control ACE for the user — for a
// new directory, one with the inherited user+SYSTEM ACL (the failing install)
// and one Restrict already made private (user+SYSTEM, protected). Children,
// including a live AF_UNIX socket, do not stop it.
func TestUserOnlyDir(t *testing.T) {
	root := t.TempDir()
	want := "O:" + current().user.String() + "D:P(A;OICI;FA;;;" + current().user.String() + ")"

	fresh := filepath.Join(root, "rt", "codex", "fresh")
	inherited := filepath.Join(root, "rt", "codex", "inherited")
	restricted := filepath.Join(root, "rt", "codex", "restricted")
	if err := os.MkdirAll(inherited, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, _ := isUserOnly(inherited); ok {
		t.Fatalf("a new dir under %s is already user-only: %s", root, dacl(t, inherited))
	}
	if err := MkdirAll(restricted, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, _ := isUserOnly(restricted); ok {
		t.Fatalf("Restrict's ACL counted as user-only: %s", dacl(t, restricted))
	}
	sub := filepath.Join(inherited, "refs")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(inherited, "codex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	for _, d := range []string{fresh, inherited, restricted} {
		for i := 0; i < 2; i++ { // the second call finds it already right
			if err := UserOnlyDir(d); err != nil {
				t.Fatalf("UserOnlyDir(%s) #%d: %v", d, i, err)
			}
			if ok, err := isUserOnly(d); !ok || err != nil {
				t.Fatalf("%s not user-only (%v): %s", d, err, dacl(t, d))
			}
			// SetNamedSecurityInfo also marks the DACL auto-inherited (PAI),
			// which Codex does not check.
			if got := strings.Replace(dacl(t, d), "D:PAI(", "D:P(", 1); got != want {
				t.Fatalf("%s: SD %s, want %s", d, got, want)
			}
		}
		if desc, bad := Exposed(d, nil); bad {
			t.Errorf("Exposed(%s): %s", d, desc)
		}
	}
	// The inheritable ACE reached the existing child (SYSTEM gone there too).
	if got := dacl(t, sub); strings.Contains(got, ";;;SY)") || !strings.Contains(got, "(A;OICIID;FA;;;"+current().user.String()+")") {
		t.Errorf("child refs: %s", got)
	}
	c, err := net.Dial("unix", filepath.Join(inherited, "codex.sock"))
	if err != nil {
		t.Fatalf("socket after re-securing its dir: %v", err)
	}
	c.Close()
}
