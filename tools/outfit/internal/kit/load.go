package kit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Loaded struct {
	Package Package
	Digest  string
	Source  string
	Raw     []byte
}

func Load(path string) (Loaded, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Loaded{}, err
	}
	file := path
	if info.IsDir() {
		file = filepath.Join(path, "connector.yaml")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return Loaded{}, err
	}
	return Parse(raw, file)
}

func Parse(raw []byte, source string) (Loaded, error) {
	var pkg Package
	if err := yaml.Unmarshal(raw, &pkg); err != nil {
		return Loaded{}, fmt.Errorf("parse %s: %w", source, err)
	}
	for modeIndex := range pkg.Modes {
		for operationIndex := range pkg.Modes[modeIndex].Operations {
			pkg.Modes[modeIndex].Operations[operationIndex] = NormalizeOperation(pkg.Modes[modeIndex].Operations[operationIndex])
		}
	}
	if issues := Validate(pkg); len(issues) > 0 {
		return Loaded{}, fmt.Errorf("invalid connector package:\n- %s", joinIssues(issues))
	}
	canonical, err := json.Marshal(pkg)
	if err != nil {
		return Loaded{}, err
	}
	digest := sha256.Sum256(canonical)
	return Loaded{Package: pkg, Digest: hex.EncodeToString(digest[:]), Source: source, Raw: canonical}, nil
}

func joinIssues(issues []string) string {
	result := ""
	for index, issue := range issues {
		if index > 0 {
			result += "\n- "
		}
		result += issue
	}
	return result
}
