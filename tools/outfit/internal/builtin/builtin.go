package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jhh3/outfit/internal/kit"
)

//go:embed connectors/*.yaml
var files embed.FS

func All() ([]kit.Loaded, error) {
	entries, err := fs.Glob(files, "connectors/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	result := make([]kit.Loaded, 0, len(entries))
	for _, name := range entries {
		raw, err := files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		loaded, err := kit.Parse(raw, "builtin:"+name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result = append(result, loaded)
	}
	return result, nil
}
