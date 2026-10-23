// Package resumenote is the note a session gets after Pyre resumed it
// because its process died underneath it: after a planned reboot (`pyre
// node resume`, sent by the owner) or after an unclean pyred restart
// (pyred's own crash resume, daemon/crashresume.go). Both use one stable
// message ID per session and resumed run, so whichever sends first wins
// and the other's send is a harmless duplicate.
package resumenote

import (
	"fmt"
	"strings"
	"time"
)

// MessageID is the stable ID of the note to sessionID once run runID (the
// resumed run) is up.
func MessageID(sessionID, runID string) string {
	return "msg_resumed-" + IDPart(sessionID) + "-" + IDPart(runID)
}

// OperationID is the matching stable operation ID (a retried send, even
// from a restarted pyred, is the same operation).
func OperationID(sessionID, runID string) string {
	return "op_resumed-" + IDPart(sessionID) + "-" + IDPart(runID)
}

// IDPart is the body of an ID (after "prefix_"), reduced to the characters
// a Pyre ID allows.
func IDPart(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	b := []byte(id)
	for i, ch := range b {
		if !(ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '-' || ch == '.' || ch == ':') {
			b[i] = '-'
		}
	}
	if len(b) == 0 {
		return "x"
	}
	return string(b)
}

// lostBackground is what every resumed session is told about the
// processes the restart took with it.
const lostBackground = "Anything you had running in the background (watchers, Monitors, dev servers, tunnels, shells) is gone: " +
	"check your state (git status, PRs, files) and restart what you need."

// AfterReboot is the owner's note after a planned reboot of nodeName.
func AfterReboot(nodeName string) string {
	return fmt.Sprintf("[from the owner] %s has restarted and you were resumed. %s If you were in the middle of work, "+
		"check the state of your last step before continuing, then carry on. If you were waiting for input, keep waiting. "+
		"No reply needed.", nodeName, lostBackground)
}

// AfterCrash is Pyre's note after it resumed a session whose process died
// underneath it (diedAt: when the old process was last seen alive; zero
// when unknown).
func AfterCrash(diedAt, resumedAt time.Time) string {
	died := "your previous process died"
	if !diedAt.IsZero() {
		died += " at about " + PT(diedAt)
	}
	return fmt.Sprintf("[from Pyre] Pyre restarted after a crash: %s and Pyre resumed you at %s. %s "+
		"If you were started by another session, check in with it before continuing. No reply needed.", died, PT(resumedAt), lostBackground)
}

// pacific is where the owner reads times. The pyred and pyre binaries
// embed the tz database (time/tzdata), so this holds on Windows too; the
// local zone is a last resort (a fixed offset would be wrong half the year).
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.Local
}()

// PT renders t in Pacific time: "Oct 20, 12:34 AM PDT".
func PT(t time.Time) string {
	return t.In(pacific).Format("Jan 2, 3:04 PM MST")
}
