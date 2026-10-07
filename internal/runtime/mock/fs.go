package mock

import (
	"path"
	"strings"
	"sync"

	c "github.com/jhh3/pyre/internal/contracts"
)

// fs.mkdir in the mock (W-qlpath): nothing touches the disk; created folders
// are remembered per node so dir.list shows them afterwards.

var (
	mockFSMu   sync.Mutex
	mockMadeFS = map[*Backend]map[string][]string{} // node + "\x00" + parent -> child names
)

func (b *Backend) mockMadeDirs(nodeID, parent string) []string {
	mockFSMu.Lock()
	defer mockFSMu.Unlock()
	return append([]string(nil), mockMadeFS[b][nodeID+"\x00"+strings.TrimSuffix(parent, "/")]...)
}

func (b *Backend) fsAction(n *c.Node, action string, params c.RawJSON) (any, bool, error) {
	if action != c.ActionFsMkdir {
		return nil, false, nil
	}
	var p c.FsMkdirParams
	if err := decodeParams(params, &p); err != nil {
		return nil, true, err
	}
	if p.Path != "~" && !strings.HasPrefix(p.Path, "~/") && !strings.HasPrefix(p.Path, "/Users/") && !strings.HasPrefix(p.Path, "/home/") {
		return nil, true, c.Errorf(c.ErrForbidden, "fs.mkdir: %s is outside the home directory", p.Path)
	}
	clean := p.Path
	if strings.HasPrefix(clean, "/") {
		clean = path.Clean(clean)
	} else if clean != "~" {
		clean = "~/" + strings.TrimPrefix(path.Clean(strings.TrimPrefix(clean, "~/")), "/")
	}
	if clean == "~" {
		return c.FsMkdirResult{Path: clean}, true, nil
	}
	parent, name := path.Split(clean)
	parent = strings.TrimSuffix(parent, "/")
	mockFSMu.Lock()
	defer mockFSMu.Unlock()
	if mockMadeFS[b] == nil {
		mockMadeFS[b] = map[string][]string{}
	}
	k := n.NodeID + "\x00" + parent
	for _, d := range mockMadeFS[b][k] {
		if d == name {
			return c.FsMkdirResult{Path: clean}, true, nil
		}
	}
	mockMadeFS[b][k] = append(mockMadeFS[b][k], name)
	return c.FsMkdirResult{Path: clean, Created: true}, true, nil
}
