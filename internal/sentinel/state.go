package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/transport"
)

// StateVersion is the state.json format version.
const StateVersion = 1

// Status is one sentinel's vote on a target.
type Status string

const (
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
	// StatusUnknown abstains: this sentinel's own network failed or it
	// could not tell (e.g. ssh refused its key and no hub is configured).
	StatusUnknown Status = "unknown"
)

// Level is a target's evaluated condition.
type Level int

const (
	LevelOK Level = iota
	LevelDegraded
	// LevelUnconfirmed is a down vote from the only live sentinel.
	LevelUnconfirmed
	LevelDown
)

func (l Level) String() string {
	switch l {
	case LevelDegraded:
		return "DEGRADED"
	case LevelUnconfirmed:
		return "DOWN (unconfirmed)"
	case LevelDown:
		return "DOWN"
	}
	return "OK"
}

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

func (l *Level) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "DEGRADED":
		*l = LevelDegraded
	case "DOWN (unconfirmed)":
		*l = LevelUnconfirmed
	case "DOWN":
		*l = LevelDown
	default:
		*l = LevelOK
	}
	return nil
}

// Check is the result of one HTTP probe.
type Check struct {
	Label  string `json:"label"`
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// SSH outcome kinds.
const (
	SSHOK          = "ok"
	SSHUnreachable = "unreachable" // timeout, refused, no route: the host may be down
	SSHAuth        = "auth"        // key or host key refused: a sentinel setup problem
	SSHHung        = "hung"        // connected, but the probe did not finish
	SSHError       = "error"
)

// Manager states of the target's systemd user manager.
const (
	ManagerRunning = "running" // running, degraded, starting...: present
	ManagerAbsent  = "absent"  // offline / no bus: the user manager is not running
	ManagerNA      = "n/a"     // not Linux
	ManagerTimeout = "timeout" // systemctl did not answer within 5 s
)

// Observation is what one sentinel saw of one target in one tick.
type Observation struct {
	CheckedAt time.Time `json:"checked_at"`
	Hub       []Check   `json:"hub,omitempty"`
	SSH       string    `json:"ssh"`
	SSHAlias  string    `json:"ssh_alias,omitempty"`
	SSHDetail string    `json:"ssh_detail,omitempty"`
	OS        string    `json:"os,omitempty"`
	Manager   string    `json:"manager,omitempty"`
	// ManagerRaw is the is-system-running output (running, degraded, offline...).
	ManagerRaw string `json:"manager_raw,omitempty"`
	// ManagerStart is the user manager's UserspaceTimestamp: a change between
	// ticks means it restarted.
	ManagerStart string   `json:"manager_start,omitempty"`
	UnitsDown    []string `json:"units_down,omitempty"`
	TCPDown      []string `json:"tcp_down,omitempty"`
	Public       []Check  `json:"public,omitempty"`
	// Maintenance is the target's maintenance.json, when read.
	Maintenance *Window `json:"maintenance,omitempty"`

	// PeerState is the target's own sentinel state.json (when it runs one),
	// pulled in the same ssh session, and RemoteNow the target's clock at
	// that moment (its age is judged on one clock). Not persisted.
	PeerState []byte    `json:"-"`
	RemoteNow time.Time `json:"-"`
}

// Vote is a sentinel's classified observation.
type Vote struct {
	Status Status `json:"status"`
	// Since is when this sentinel's vote last changed to Status.
	Since   time.Time   `json:"since"`
	Reasons []string    `json:"reasons,omitempty"`
	Obs     Observation `json:"obs"`
}

// Seen is one sentinel as another sentinel saw it this round.
type Seen struct {
	AgeS      int64  `json:"age_s"`
	NotifyErr string `json:"notify_err,omitempty"`
}

// Window is a maintenance window (maintenance.json).
type Window struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
	Set    time.Time `json:"set_at,omitempty"`
}

// Active reports whether the window covers now.
func (w *Window) Active(now time.Time) bool { return w != nil && now.Before(w.Until) }

// Timer is a target's evaluation history on one sentinel.
type Timer struct {
	// Level is this tick's evaluation; DownLevel the worst down level
	// (DOWN or DOWN unconfirmed) while DownSince holds.
	Level     Level `json:"level"`
	DownLevel Level `json:"down_level,omitempty"`
	// DownSince / DegradedSince start when the condition is first seen and
	// clear only after it has been absent for RecoverGrace (DownClear /
	// OKSince), so a flapping service (up 20 s of every 30) still confirms.
	DownSince     time.Time `json:"down_since,omitzero"`
	DownClear     time.Time `json:"down_clear,omitzero"`
	DegradedSince time.Time `json:"degraded_since,omitzero"`
	OKSince       time.Time `json:"ok_since,omitzero"`
	// QuietUntil is the last tick the target was in a maintenance window:
	// grace periods count from it, so a problem that started during the
	// window needs its full grace after the window ends before it alerts.
	QuietUntil time.Time `json:"quiet_until,omitzero"`
	// Public: this tick's problem is only public URLs failing (seen from
	// the watchers); DEGRADED then needs PublicGrace.
	Public bool `json:"public,omitempty"`
	// LastEval is the last tick that updated the timer; after a longer gap
	// (abstaining, sentinel stopped) the timer starts over.
	LastEval time.Time `json:"last_eval,omitzero"`
	// ManagerStart is the last user-manager start seen by this sentinel;
	// NotifiedStart the last restart reported; LastRestartNotice when, and
	// RestartsMuted how many restarts were not reported since (rate limit).
	ManagerStart      string    `json:"manager_start,omitempty"`
	PrevManagerStart  string    `json:"prev_manager_start,omitempty"`
	StartChangedAt    time.Time `json:"start_changed_at,omitzero"`
	NotifiedStart     string    `json:"notified_start,omitempty"`
	LastRestartNotice time.Time `json:"last_restart_notice,omitzero"`
	RestartsMuted     int       `json:"restarts_muted,omitempty"`
}

// RecoveryAttempt is one ssh-login recovery.
type RecoveryAttempt struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Result string    `json:"result,omitempty"`
}

// Incident is one problem with one target, from confirmation to recovery.
// The ID is stable across leader changes: every sentinel mirrors the
// leader's incidents, and a new leader adopts them.
type Incident struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"`
	Level     Level     `json:"level"`
	Since     time.Time `json:"since"`
	Confirmed time.Time `json:"confirmed"`
	Detail    string    `json:"detail,omitempty"`
	By        []string  `json:"by,omitempty"`
	// Alerting state, persisted so neither a restart nor a leader change
	// repeats a message.
	AlertedLevel   Level             `json:"alerted_level"`
	FirstAlertAt   time.Time         `json:"first_alert_at,omitzero"`
	LastAlertAt    time.Time         `json:"last_alert_at,omitzero"`
	RemindersSent  int               `json:"reminders_sent,omitempty"`
	Recovery       []RecoveryAttempt `json:"recovery,omitempty"`
	ManualHelpSent bool              `json:"manual_help_sent,omitempty"`
	Resolved       time.Time         `json:"resolved,omitzero"`
	RecoveredSent  bool              `json:"recovered_sent,omitempty"`
	Owner          string            `json:"owner,omitempty"` // leader that last acted
	// Reopened counts how often the incident came back within FlapWindow of
	// resolving; a reopened incident needs FlapWindow clear to resolve.
	Reopened int `json:"reopened,omitempty"`
}

// State is one sentinel's state.json, read by its peers over ssh.
type State struct {
	Version   int       `json:"version"`
	Sentinel  string    `json:"sentinel"`
	Rank      int       `json:"rank"`
	UpdatedAt time.Time `json:"updated_at"`
	NetworkOK bool      `json:"network_ok"`
	Network   string    `json:"network,omitempty"`
	// NotifyErr is why this sentinel's last bot DM (or token check) failed;
	// empty when it can deliver. A sentinel that cannot deliver is skipped
	// as actor while another live one can.
	NotifyErr string    `json:"notify_err,omitempty"`
	NotifyAt  time.Time `json:"notify_at,omitzero"`
	Leader    string    `json:"leader,omitempty"`
	// Live are the sentinels this one saw as live this tick.
	Live []string `json:"live,omitempty"`
	// Seen is the same set with how old each one's state was (seconds) and
	// its delivery status, for one-hop gossip: a peer's view expires with
	// its own freshness, and carries whether that sentinel can deliver.
	Seen        map[string]Seen      `json:"seen,omitempty"`
	Votes       map[string]*Vote     `json:"votes,omitempty"`
	Timers      map[string]*Timer    `json:"timers,omitempty"`
	Maintenance map[string]*Window   `json:"maintenance,omitempty"`
	Incidents   map[string]*Incident `json:"incidents,omitempty"` // open, by target
	// SelfPublic is this sentinel's check of its own host's public URLs
	// (the target named Self), at SelfPublicAt: the self-check a public-URL
	// alert about this host needs.
	SelfPublic   []Check   `json:"self_public,omitempty"`
	SelfPublicAt time.Time `json:"self_public_at,omitzero"`
	// LinkTimers are this sentinel's partition evaluations and Links the
	// open link incidents (mirrored from the actor like Incidents), by link
	// key ("home~office", or "network" without sites).
	LinkTimers map[string]*Timer    `json:"link_timers,omitempty"`
	Links      map[string]*Incident `json:"links,omitempty"`
	Closed     []*Incident          `json:"closed,omitempty"` // newest first, bounded
}

const maxClosed = 20

func newState(self string, rank int) *State {
	return &State{
		Version: StateVersion, Sentinel: self, Rank: rank,
		Votes: map[string]*Vote{}, Timers: map[string]*Timer{},
		Maintenance: map[string]*Window{}, Incidents: map[string]*Incident{},
		LinkTimers: map[string]*Timer{}, Links: map[string]*Incident{},
	}
}

func (s *State) fill() {
	if s.Votes == nil {
		s.Votes = map[string]*Vote{}
	}
	if s.Timers == nil {
		s.Timers = map[string]*Timer{}
	}
	if s.Maintenance == nil {
		s.Maintenance = map[string]*Window{}
	}
	if s.Incidents == nil {
		s.Incidents = map[string]*Incident{}
	}
	if s.LinkTimers == nil {
		s.LinkTimers = map[string]*Timer{}
	}
	if s.Links == nil {
		s.Links = map[string]*Incident{}
	}
}

func (s *State) close(inc *Incident) {
	delete(s.Incidents, inc.Target)
	s.Closed = append([]*Incident{inc}, s.Closed...)
	if len(s.Closed) > maxClosed {
		s.Closed = s.Closed[:maxClosed]
	}
}

// closedAfter: inc (this generation of it) appears in Closed, so it is done.
// A reopened incident has a higher Reopened than its closed copy.
func (s *State) closedAfter(inc *Incident) bool {
	c := s.closedByID(inc.ID)
	return c != nil && c.Reopened >= inc.Reopened
}

// lastClosed is the most recently resolved closed incident of target.
func (s *State) lastClosed(target string) *Incident {
	var best *Incident
	for _, c := range s.Closed {
		if c.Target == target && (best == nil || c.Resolved.After(best.Resolved)) {
			best = c
		}
	}
	return best
}

func (s *State) removeClosed(id string) {
	out := s.Closed[:0]
	for _, c := range s.Closed {
		if c.ID != id {
			out = append(out, c)
		}
	}
	s.Closed = out
}

func (s *State) closedByID(id string) *Incident {
	for _, c := range s.Closed {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// ParseState decodes a state.json (peer or local).
func ParseState(b []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	s.fill()
	return &s, nil
}

// ReadState reads path; a missing file is (nil, nil).
func ReadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseState(b)
}

// WriteState writes s to path atomically (0600).
func WriteState(path string, s *State) error {
	if err := fileperm.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return transport.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
