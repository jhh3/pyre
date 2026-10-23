package mock

import (
	"path"
	"sort"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// A synthetic folder tree per node for dir.list (test control for folder
// pickers). Without one, dir.list answers every folder with the same three
// children (actions.go).

// DirTree is a node's synthetic filesystem: folder path ("~", "~/src",
// "/fast") -> child folder names (dot-folders included). Legacy answers like
// an older pyred: no dot-folders, no home or worktree root.
type DirTree struct {
	Home         string // absolute, e.g. "/home/demo"
	WorktreeRoot string
	Dirs         map[string][]string
	Legacy       bool
}

// SetDirTree installs (or, with nil, removes) a node's synthetic tree.
func (b *Backend) SetDirTree(nodeID string, t *DirTree) {
	mockFSMu.Lock()
	defer mockFSMu.Unlock()
	if mockTrees[b] == nil {
		mockTrees[b] = map[string]*DirTree{}
	}
	if t == nil {
		delete(mockTrees[b], nodeID)
		return
	}
	mockTrees[b][nodeID] = t
}

var mockTrees = map[*Backend]map[string]*DirTree{}

func (b *Backend) dirTree(nodeID string) *DirTree {
	mockFSMu.Lock()
	defer mockFSMu.Unlock()
	return mockTrees[b][nodeID]
}

// list answers dir.list from the tree; a missing folder is an invalid
// path, as on a real node.
func (t *DirTree) list(nodeID string, made func(string, string) []string, p c.DirListParams) (c.DirListResult, error) {
	dir := p.Path
	if dir == "" {
		dir = "~"
	}
	if strings.HasPrefix(dir, "~/") {
		dir = "~/" + strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(dir, "~/")), "/")
	} else if dir != "~" {
		dir = path.Clean(dir)
	}
	if t.Home != "" && (dir == t.Home || strings.HasPrefix(dir, t.Home+"/")) {
		dir = "~" + strings.TrimPrefix(dir, t.Home)
	}
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "/"
	}
	abs := dir
	if strings.HasPrefix(dir, "~") && t.Home != "" {
		abs = t.Home + strings.TrimPrefix(dir, "~")
	}
	kids, ok := t.Dirs[dir]
	extra := made(nodeID, dir) // fs.mkdir by ~ path or by absolute path
	if abs != dir {
		extra = append(extra, made(nodeID, abs)...)
	}
	if !ok && len(extra) == 0 {
		return c.DirListResult{}, c.Errorf(c.ErrInvalid, "working directory %q does not exist on this node", p.Path)
	}
	res := c.DirListResult{Path: abs, Dirs: []string{}}
	hidden := p.Hidden && !t.Legacy
	for _, d := range append(append([]string(nil), kids...), extra...) {
		if strings.HasPrefix(d, ".") && !hidden {
			continue
		}
		res.Dirs = append(res.Dirs, d)
	}
	sort.Strings(res.Dirs)
	if !t.Legacy {
		res.Hidden, res.Home, res.WorktreeRoot = hidden, t.Home, t.WorktreeRoot
	}
	return res, nil
}
