package winenv

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeInfo struct{ dir bool }

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return map[bool]fs.FileMode{true: fs.ModeDir, false: 0}[f.dir] }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

func fakeEnv(vars map[string]string, files map[string]bool) Env {
	return Env{
		Getenv: func(k string) string { return vars[k] },
		Stat: func(p string) (os.FileInfo, error) {
			if d, ok := files[p]; ok {
				return fakeInfo{dir: d}, nil
			}
			return nil, fs.ErrNotExist
		},
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Glob: func(pat string) ([]string, error) {
			var out []string
			for p := range files {
				if ok, _ := filepath.Match(pat, p); ok {
					out = append(out, p)
				}
			}
			return out, nil
		},
	}
}

func TestToolDirsAndGitBash(t *testing.T) {
	vars := map[string]string{"USERPROFILE": "/u", "APPDATA": "/u/AppData/Roaming", "ProgramFiles": "/pf", "LOCALAPPDATA": "/u/AppData/Local"}
	files := map[string]bool{
		"/u/.local/bin": true, "/u/AppData/Roaming/npm": true, "/pf/nodejs": true, "/pf/Git/cmd": true,
		"/pf/Git/bin/bash.exe": false, "/u/AppData/Local/Programs/Pyre/tools/outfit/bin": true,
	}
	e := fakeEnv(vars, files)
	got := e.ToolDirs()
	want := []string{"/u/.local/bin", "/u/AppData/Roaming/npm", "/pf/nodejs", "/pf/Git/cmd", "/u/AppData/Local/Programs/Pyre/tools/outfit/bin"}
	if len(got) != len(want) {
		t.Fatalf("ToolDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ToolDirs = %v, want %v", got, want)
		}
	}
	if b := e.GitBash(); b != "/pf/Git/bin/bash.exe" {
		t.Fatalf("GitBash = %q", b)
	}
	vars["CLAUDE_CODE_GIT_BASH_PATH"] = "/custom/bash.exe"
	files["/custom/bash.exe"] = false
	if b := e.GitBash(); b != "/custom/bash.exe" {
		t.Fatalf("GitBash with override = %q", b)
	}
	if b := fakeEnv(map[string]string{}, nil).GitBash(); b != "" {
		t.Fatalf("GitBash without Git = %q", b)
	}
}

func TestNativeCodex(t *testing.T) {
	x64 := "/npm/node_modules/@openai/codex/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe"
	arm := "/npm/node_modules/@openai/codex/node_modules/@openai/codex-win32-arm64/vendor/aarch64-pc-windows-msvc/bin/codex.exe"
	e := fakeEnv(map[string]string{"PROCESSOR_ARCHITECTURE": "ARM64"}, map[string]bool{x64: false, arm: false})
	if got := e.NativeCodex("/npm/codex.cmd"); got != arm {
		t.Fatalf("arm64: %q", got)
	}
	e = fakeEnv(map[string]string{"PROCESSOR_ARCHITECTURE": "AMD64"}, map[string]bool{x64: false, arm: false})
	if got := e.NativeCodex("/npm/codex.cmd"); got != x64 {
		t.Fatalf("amd64: %q", got)
	}
	if got := e.NativeCodex("/somewhere/codex.exe"); got != "" {
		t.Fatalf("native exe is not a shim: %q", got)
	}
}

func TestPosixPath(t *testing.T) {
	if got := PosixPath(`C:\Users\me\pyred.exe`); got != "C:/Users/me/pyred.exe" {
		t.Fatal(got)
	}
}
