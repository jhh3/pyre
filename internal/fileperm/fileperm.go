// Package fileperm is Pyre's owner-only file permission policy on every
// platform: the checks that refuse secrets other users can read, and the
// setters that make files and directories private.
//
// Unix (darwin, linux): exactly the mode bits Pyre always used — private
// means no group/other permission bits (0600 files, 0700 directories) and
// the file owned by the current uid.
//
// Windows: mode bits do not exist (Go reports 0666/0444), so privacy is the
// DACL. Private means the owner is the current user (or the token's default
// owner, e.g. Administrators for an elevated token) and no allow ACE grants
// anything to a principal other than that user, LocalSystem or
// BUILTIN\Administrators — the Windows counterparts of root, which can read
// a 0600 file on Unix too. Restrict replaces the DACL with a protected
// (non-inherited) one granting full control to the current user and
// LocalSystem only; directories pass it on to new children (OI)(CI).
package fileperm

import (
	"os"
)

// Private reports whether mode asks for owner-only access (no group/other
// bits), the condition under which Restrict applies an owner-only DACL on
// Windows.
func Private(mode os.FileMode) bool { return mode.Perm()&0o077 == 0 }

// Exposed reports whether the file described by fi (at path) is accessible
// to principals other than its owner, with a short description of why
// ("mode 0644" on Unix, "an ACL entry for DOMAIN\user" on Windows).
func Exposed(path string, fi os.FileInfo) (string, bool) { return exposed(path, fi) }

// ForeignOwner reports whether the file described by fi (at path) is owned
// by someone other than the current user, with a description of the owner
// ("uid 501" on Unix, "DOMAIN\user" on Windows).
func ForeignOwner(path string, fi os.FileInfo) (string, bool) { return foreignOwner(path, fi) }

// Restrict sets mode on path: os.Chmod on Unix; on Windows the read-only
// attribute from mode plus, when mode is private, the owner-only DACL.
func Restrict(path string, mode os.FileMode) error { return restrict(path, mode) }

// RestrictFile is Restrict for an open file (Unix fchmod).
func RestrictFile(f *os.File, mode os.FileMode) error { return restrictFile(f, mode) }

// MkdirAll is os.MkdirAll that also protects the directory it creates on
// Windows, where mode bits alone protect nothing: a newly created dir gets
// the owner-only DACL when mode is private. Like os.MkdirAll it never
// changes an existing directory (on Unix it is exactly os.MkdirAll).
func MkdirAll(dir string, mode os.FileMode) error {
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	if statErr == nil || !Private(mode) {
		return nil
	}
	return protectNewDir(dir)
}

// UserOnlyDir creates dir (and missing parents) and makes it private to the
// current user alone, also when it already exists. On Unix that is mode 0700
// (as Restrict). On Windows it is stricter than Restrict: a protected DACL
// with a single ACE, full control for the current user inherited by new
// files and subdirectories (OI)(CI), and the user as owner — no LocalSystem
// entry. Codex's app-server refuses a socket directory with any other ACL
// ("socket directory is not private to the current user"). A directory that
// already has exactly that ACL is left alone.
func UserOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return userOnlyDir(dir)
}

// OpenNoFollow opens path read-only and refuses a symbolic link (Unix
// O_NOFOLLOW; Windows: any reparse point — symlink or junction) as the final
// path component.
func OpenNoFollow(path string) (*os.File, error) { return openNoFollow(path) }

// FixHint is the shell command that makes path private, for error messages:
// `chmod 600 PATH` / `chmod 700 PATH` on Unix, an icacls command on Windows.
func FixHint(path string, dir bool) string { return fixHint(path, dir) }
