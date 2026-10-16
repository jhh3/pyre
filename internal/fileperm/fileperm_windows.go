//go:build windows

package fileperm

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ACE types (winnt.h) that grant access.
const (
	aceAccessAllowed         = 0x0
	aceAccessAllowedObject   = 0x5
	aceAccessAllowedCallback = 0x9
	aceAllowedCallbackObject = 0xB
	tokenOwnerClass          = 4 // TOKEN_INFORMATION_CLASS TokenOwner
)

type identity struct {
	user  *windows.SID
	owner *windows.SID // the token's default owner for new objects
	err   error
}

var (
	selfOnce sync.Once
	self     identity
)

func current() identity {
	selfOnce.Do(func() {
		tok := windows.GetCurrentProcessToken()
		tu, err := tok.GetTokenUser()
		if err != nil {
			self.err = fmt.Errorf("fileperm: current user: %w", err)
			return
		}
		u, err := tu.User.Sid.Copy()
		if err != nil {
			self.err = err
			return
		}
		self.user = u
		var n uint32
		_ = windows.GetTokenInformation(tok, tokenOwnerClass, nil, 0, &n)
		if n > 0 {
			buf := make([]byte, n)
			if err := windows.GetTokenInformation(tok, tokenOwnerClass, &buf[0], n, &n); err == nil {
				// TOKEN_OWNER { PSID Owner }
				o := *(**windows.SID)(unsafe.Pointer(&buf[0]))
				if c, err := o.Copy(); err == nil {
					self.owner = c
				}
			}
		}
	})
	return self
}

func wellKnown(t windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	s, err := windows.CreateWellKnownSid(t)
	if err != nil {
		return nil
	}
	return s
}

// trusted reports whether sid may hold access to a private file: the
// current user, the token's default owner, LocalSystem, Administrators,
// OWNER RIGHTS and CREATOR OWNER.
func trusted(id identity, sid *windows.SID) bool {
	if sid == nil {
		return false
	}
	if id.user != nil && sid.Equals(id.user) || id.owner != nil && sid.Equals(id.owner) {
		return true
	}
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid,
		windows.WinCreatorOwnerRightsSid, windows.WinCreatorOwnerSid} {
		if w := wellKnown(t); w != nil && sid.Equals(w) {
			return true
		}
	}
	return false
}

func describe(sid *windows.SID) string {
	if sid == nil {
		return "<nil SID>"
	}
	if acct, dom, _, err := sid.LookupAccount(""); err == nil {
		if dom != "" {
			return dom + `\` + acct
		}
		return acct
	}
	return sid.String()
}

func exposed(path string, _ os.FileInfo) (string, bool) {
	id := current()
	if id.err != nil {
		return id.err.Error(), true // fail closed
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "an unreadable security descriptor (" + err.Error() + ")", true
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return "no DACL (everyone has full access)", true
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return "an unreadable ACL", true
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask == 0 {
			continue
		}
		switch ace.Header.AceType {
		case aceAccessAllowed, aceAccessAllowedCallback:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !trusted(id, sid) {
				return "an ACL entry granting access to " + describe(sid), true
			}
		case aceAccessAllowedObject, aceAllowedCallbackObject:
			return "an object ACL entry Pyre cannot evaluate", true
		}
	}
	return "", false
}

func foreignOwner(path string, _ os.FileInfo) (string, bool) {
	id := current()
	if id.err != nil {
		return id.err.Error(), true
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "unknown (" + err.Error() + ")", true
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "unknown", true
	}
	if owner.Equals(id.user) || id.owner != nil && owner.Equals(id.owner) {
		return "", false
	}
	return describe(owner), true
}

// privateSDDL is the protected owner-only DACL: full control for the current
// user and LocalSystem; directories pass it to new files and subdirectories.
func privateSDDL(dir bool) (string, error) {
	id := current()
	if id.err != nil {
		return "", id.err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	return fmt.Sprintf("D:P(A;%s;FA;;;SY)(A;%s;FA;;;%s)", inherit, inherit, id.user.String()), nil
}

func setPrivate(path string, dir bool) error {
	sddl, err := privateSDDL(dir)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("fileperm: set owner-only ACL on %s: %w", path, err)
	}
	return nil
}

func restrict(path string, mode os.FileMode) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket != 0 {
		// AF_UNIX socket files cannot be opened by name; they carry the
		// (private) DACL inherited from the state directory.
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if !Private(mode) {
		return nil
	}
	// Already private (e.g. the profile's user+SYSTEM+Administrators ACL, or
	// ours from an earlier start): leave it, so a large state directory is
	// not re-propagated on every daemon start.
	if _, bad := exposed(path, st); !bad {
		if _, foreign := foreignOwner(path, st); !foreign {
			return nil
		}
	}
	return setPrivate(path, st.IsDir())
}

func restrictFile(f *os.File, mode os.FileMode) error {
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if !Private(mode) {
		return nil
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return setPrivate(f.Name(), st.IsDir())
}

var errReparse = errors.New("is a symbolic link or junction (refused)")

func openNoFollow(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: errReparse}
	}
	return os.NewFile(uintptr(h), path), nil
}

func fixHint(path string, dir bool) string {
	g := "F"
	if dir {
		g = "(OI)(CI)F"
	}
	return fmt.Sprintf("run `icacls \"%s\" /inheritance:r /grant:r \"%%USERNAME%%:%s\" /grant:r *S-1-5-18:%s` (owner-only ACL)", path, g, g)
}

func protectNewDir(dir string) error { return setPrivate(dir, true) }
