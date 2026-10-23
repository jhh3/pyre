package daemon

import (
	"regexp"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// NativeResumeArg starts a new session that continues an existing native
// conversation started outside Pyre (pyre start --native-resume ID): its
// first run is a native resume of that Claude session instead of a fresh
// conversation. Carried in StartRunCommand.Args like PermissionsArg and
// removed before the adapter sees the args. The conversation's transcript
// must be on this node (any Claude config home the adapter searches: the
// default login or an account profile); the adapter brings it into the
// run's config home.
const NativeResumeArg = "--pyre-native-resume"

var claudeSessionRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// conversationUser tells whether a native conversation looks live outside
// Pyre (claude.Adapter.ConversationInUse): adopting it would give it two
// writers.
type conversationUser interface {
	ConversationInUse(cwd, sid string) (bool, string)
}

// splitNativeResumeArg removes every NativeResumeArg from args and returns
// the last requested conversation id ("" = none).
func splitNativeResumeArg(args []string) ([]string, string, error) {
	var out []string
	id := ""
	for i := 0; i < len(args); i++ {
		name, val, has := strings.Cut(args[i], "=")
		if name != NativeResumeArg {
			out = append(out, args[i])
			continue
		}
		if !has {
			if i+1 >= len(args) {
				return nil, "", contracts.Errorf(contracts.ErrInvalid, "%s needs a conversation id", NativeResumeArg)
			}
			i++
			val = args[i]
		}
		id = strings.TrimSpace(val)
	}
	return out, id, nil
}

// nativeResumeRef checks a requested adoption for harness h and returns
// the native reference the first run resumes.
func nativeResumeRef(h contracts.Harness, id string) (*contracts.NativeRef, error) {
	if h != contracts.HarnessClaude {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "continuing a conversation started outside Pyre is supported for claude, not %s", h)
	}
	if !claudeSessionRe.MatchString(id) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "%q is not a Claude session id (a UUID)", id)
	}
	return &contracts.NativeRef{Kind: "claude_session", ID: id}, nil
}
