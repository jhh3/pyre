package contracts

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ID prefixes. IDs are "<prefix>_<26 char Crockford base32 ULID>".
const (
	PrefixNode        = "node"
	PrefixSession     = "ses"
	PrefixRun         = "run"
	PrefixProject     = "prj"
	PrefixMessage     = "msg"
	PrefixThread      = "thr"
	PrefixDocument    = "doc"
	PrefixRevision    = "rev"
	PrefixProposal    = "prp"
	PrefixArtifact    = "art"
	PrefixVersion     = "ver"
	PrefixUpload      = "upl"
	PrefixComment     = "cmt"
	PrefixAccount     = "acct"
	PrefixGrant       = "grant"
	PrefixAssignment  = "asg"
	PrefixEnvironment = "env"
	PrefixApproval    = "apr"
	PrefixLease       = "lease"
	PrefixEvent       = "evt"
	PrefixOperation   = "op"
	PrefixAttention   = "att"
	PrefixAttempt     = "dlv"
	PrefixConnection  = "conn"
	PrefixNote        = "note"
	PrefixCommand     = "cmd"
	PrefixClient      = "cli"
	PrefixPairing     = "pair"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	idMu       sync.Mutex
	lastMillis uint64
	lastRand   [10]byte
)

// NewID returns a new time-sortable opaque ID with the given prefix.
// IDs generated in the same millisecond by one process are monotonic.
func NewID(prefix string) string {
	return prefix + "_" + newULID(time.Now())
}

func newULID(t time.Time) string {
	idMu.Lock()
	defer idMu.Unlock()
	ms := uint64(t.UnixMilli())
	var r [10]byte
	if ms == lastMillis {
		r = lastRand
		// increment 80-bit random component
		for i := 9; i >= 0; i-- {
			r[i]++
			if r[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(r[:]); err != nil {
			panic(fmt.Sprintf("contracts: crypto/rand failed: %v", err))
		}
		lastMillis = ms
	}
	lastRand = r
	var b [16]byte
	var tb [8]byte
	binary.BigEndian.PutUint64(tb[:], ms)
	copy(b[0:6], tb[2:8])
	copy(b[6:], r[:])
	return encodeBase32(b)
}

func encodeBase32(b [16]byte) string {
	// 26 chars x 5 bits = 130 bits; the 128-bit value is left-padded with 2 zero bits.
	var out [26]byte
	bit := func(pos int) byte { // pos in [0,130); first two positions are padding
		p := pos - 2
		if p < 0 {
			return 0
		}
		return (b[p/8] >> (7 - uint(p%8))) & 1
	}
	for i := 0; i < 26; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			v = v<<1 | bit(i*5+j)
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

// IDPrefix returns the prefix part of an ID, or "" if malformed.
func IDPrefix(id string) string {
	i := strings.IndexByte(id, '_')
	if i <= 0 {
		return ""
	}
	return id[:i]
}

// ValidID reports whether id has the expected prefix and a non-empty body.
// It does not require the body to be a ULID, so imported/legacy IDs that use
// deterministic bodies (e.g. "msg_legacy-ws-...") remain valid.
func ValidID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix+"_") || len(id) <= len(prefix)+1 || len(id) > 200 {
		return false
	}
	for _, c := range id[len(prefix)+1:] {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
