package contracts

// Folder creation for launching in a new plain directory (additive,
// W-qlpath). The quick-launch bar and `pyre new <path> --mkdir` use it; a
// node without it answers "unknown action" and the client says so.
const ActionFsMkdir = "fs.mkdir" // FsMkdirParams -> FsMkdirResult

// FsMkdirParams names the directory to create, with parents. The node
// accepts only paths inside its user's home directory ("~/…" or absolute),
// after resolving symlinks of the part that already exists.
type FsMkdirParams struct {
	Path string `json:"path"`
}

// FsMkdirResult is the absolute directory and whether it was created (false:
// it already existed).
type FsMkdirResult struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
}
