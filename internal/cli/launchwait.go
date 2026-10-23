package cli

import (
	"context"
	"fmt"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// DefaultLaunchWait bounds how long `pyre start` and start_session wait for
// the node to launch the run: the hub answers once the launch is queued,
// and a launch that failed (tmux refused the command line, issue
// iss_…) was otherwise only visible later, in the
// run state or the parent's report.
const DefaultLaunchWait = 20 * time.Second

// launchPoll paces the launch wait (or PollInterval when shorter).
const launchPoll = 500 * time.Millisecond

// awaitLaunch waits up to wait for the started run to leave "starting" and
// updates res.Session with what it saw. A run that failed is returned as an
// error naming the session and the reason (the session itself exists). A
// run still starting at the deadline, or a hub that cannot be asked, is not
// an error: the launch may still succeed. wait <= 0 returns at once.
func (o *Ops) awaitLaunch(ctx context.Context, res *c.StartSessionResponse, wait time.Duration) error {
	if wait <= 0 || res.Session.Run == nil || res.Session.Run.State != c.RunStarting {
		return launchOutcome(res.Session)
	}
	poll := launchPoll
	if o.PollInterval > 0 && o.PollInterval < poll {
		poll = o.PollInterval
	}
	runID := res.Session.Run.RunID
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(min(poll, time.Until(deadline))):
		}
		v, err := o.API.GetSession(ctx, c.GetRequest{ID: res.Session.Session.SessionID})
		if err != nil || v.Run == nil || v.Run.RunID != runID {
			continue // transient, or the run was replaced: keep the answer we have
		}
		res.Session = v
		if v.Run.State != c.RunStarting {
			break
		}
	}
	return launchOutcome(res.Session)
}

// launchOutcome is the error for a session whose run failed to launch.
func launchOutcome(v c.SessionView) error {
	if v.Run == nil || v.Run.State != c.RunFailed {
		return nil
	}
	reason := v.Run.StateReason
	if reason == "" {
		reason = "launch failed"
	}
	return withStatus(ExitError, fmt.Errorf("session %s did not start on %s: %s", v.Session.SessionID, orDash(v.NodeName), reason))
}
