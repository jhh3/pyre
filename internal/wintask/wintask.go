// Package wintask holds the Windows Task Scheduler pieces Pyre shares: the
// node's logon task (cmd/pyred service) and the on-demand session-host task
// (termhost). Definitions are rendered as Task Scheduler XML (schema 1.2,
// Windows 7+) on every platform so they are testable anywhere; registering
// and running them (schtasks.exe) is Windows-only.
package wintask

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeName reduces a user name to the characters used in Pyre task names
// (Task Scheduler names are machine-wide, so they carry the user).
func SafeName(user string) string {
	u := strings.Trim(nameUnsafe.ReplaceAllString(user, "_"), "_")
	if u == "" {
		u = "user"
	}
	return u
}

// QuoteArg quotes one argument for a Windows command line the way
// CommandLineToArgvW (and Go's runtime) parse it — syscall.EscapeArg,
// available on every platform.
func QuoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		default:
			slashes = 0
		}
		b.WriteByte(s[i])
	}
	for ; slashes > 0; slashes-- {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// Join quotes and joins argv into one command line.
func Join(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = QuoteArg(a)
	}
	return strings.Join(q, " ")
}

// XMLText escapes s for an XML text node.
func XMLText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// UTF16File encodes s as UTF-16LE with a BOM (what schtasks /XML expects
// for encoding="UTF-16").
func UTF16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2+2*i:], c)
	}
	return b
}

// OnDemand is a task with no trigger, started only by Run, as one user in
// that user's interactive session.
type OnDemand struct {
	Description string
	// UserID is a SID string or DOMAIN\name.
	UserID  string
	Command string
	Args    []string
	WorkDir string
	// Hidden hides the task in the Task Scheduler UI's default view.
	Hidden bool
}

// XML renders the definition: interactive token, least privilege, no
// execution time limit, no idle/battery stops, IgnoreNew for a second Run
// while an instance is running, and normal process priority (Task
// Scheduler's default 7 is below normal, which would slow every program
// the task's process starts).
func (t OnDemand) XML() string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>Pyre</Author>
    <Description>` + XMLText(t.Description) + `</Description>
  </RegistrationInfo>
  <Triggers />
  <Principals>
    <Principal id="Author">
      <UserId>` + XMLText(t.UserID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>` + strconv.FormatBool(t.Hidden) + `</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + XMLText(QuoteArg(t.Command)) + `</Command>
      <Arguments>` + XMLText(Join(t.Args)) + `</Arguments>
      <WorkingDirectory>` + XMLText(t.WorkDir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

// ParseTaskNames extracts the task names (without the leading folder
// backslash for root-folder tasks) from `schtasks /Query /FO CSV /NH`
// output. Only the first column is used: the rest is localized.
func ParseTaskNames(out string) []string {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var names []string
	seen := map[string]bool{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(rec) == 0 {
			continue
		}
		n := strings.TrimSpace(rec[0])
		if !strings.HasPrefix(n, `\`) {
			continue // a header or blank line
		}
		n = strings.TrimPrefix(n, `\`)
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}
