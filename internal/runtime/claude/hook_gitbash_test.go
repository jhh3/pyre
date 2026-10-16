package claude

import "testing"

func TestGitBashPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\pyre\AppData\Local\Programs\Pyre\current\bin\pyred.exe`: "/c/Users/pyre/AppData/Local/Programs/Pyre/current/bin/pyred.exe",
		`D:/tools/pyred.exe`:   "/d/tools/pyred.exe",
		"/usr/local/bin/pyred": "/usr/local/bin/pyred",
	} {
		if got := gitBashPath(in); got != want {
			t.Errorf("gitBashPath(%q) = %q, want %q", in, got, want)
		}
	}
}
