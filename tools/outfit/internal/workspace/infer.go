package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type AvailableConnection struct {
	Name, Package, Mode, Source string
}

type Inference struct {
	Manifest  Manifest            `json:"manifest"`
	Evidence  map[string][]string `json:"evidence"`
	Ambiguous map[string][]string `json:"ambiguous,omitempty"`
}

type signalRule struct {
	Package string
	Terms   []string
}

var signalRules = []signalRule{
	{Package: "github", Terms: []string{"github.com", "@octokit/", "go-github", "pygithub"}},
	{Package: "slack", Terms: []string{"slack.com/", "@slack/", "slack_sdk", "slack-go"}},
	{Package: "linear", Terms: []string{"linear.app/", "@linear/sdk"}},
	{Package: "sentry", Terms: []string{"sentry.properties", "@sentry/", "sentry-sdk", "sentry_cli"}},
	{Package: "intercom", Terms: []string{"intercom.com/", "intercom-client", "@intercom/"}},
	{Package: "notion", Terms: []string{"notion.so/", "@notionhq/client", "notion-client"}},
	{Package: "gcp", Terms: []string{"google-cloud-", "cloud.google.com/go", "gcloud"}},
	{Package: "aws", Terms: []string{"@aws-sdk/", "boto3", "aws-sdk-go"}},
	{Package: "postgres", Terms: []string{"postgresql", "postgres://", "pgx", "psycopg"}},
}

func Infer(root string, available []AvailableConnection) (Inference, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Inference{}, err
	}
	corpus, err := inferenceCorpus(absolute)
	if err != nil {
		return Inference{}, err
	}
	candidatesByPackage := map[string][]AvailableConnection{}
	for _, item := range available {
		if _, reproducible := reproduciblePackagePath(absolute, item.Source); reproducible {
			candidatesByPackage[item.Package] = append(candidatesByPackage[item.Package], item)
		}
	}
	byPackage := map[string][]AvailableConnection{}
	for key, items := range candidatesByPackage {
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		candidatesByPackage[key] = items
		if len(items) == 1 {
			byPackage[key] = items
		}
	}
	manifest := Manifest{APIVersion: APIVersion, Name: filepath.Base(absolute), Profiles: map[string][]string{}, Profile: "default"}
	found := map[string][]string{}
	ambiguous := map[string][]string{}
	for _, rule := range signalRules {
		var matches []string
		for file, text := range corpus {
			for _, term := range rule.Terms {
				if strings.Contains(text, strings.ToLower(term)) {
					matches = append(matches, file+" contains "+term)
				}
			}
		}
		if len(matches) == 0 {
			continue
		}
		if candidates := candidatesByPackage[rule.Package]; len(candidates) > 1 {
			for _, item := range candidates {
				ambiguous[rule.Package] = append(ambiguous[rule.Package], item.Name)
			}
			continue
		}
		if len(byPackage[rule.Package]) == 0 {
			continue
		}
		item := byPackage[rule.Package][0]
		manifest.Connectors = append(manifest.Connectors, Connector{Name: item.Name, Package: item.Package, Mode: item.Mode})
		manifest.Profiles["default"] = append(manifest.Profiles["default"], item.Name)
		sort.Strings(matches)
		found[item.Name] = matches
	}
	for _, item := range inferMCPServers(absolute, byPackage) {
		present := false
		for _, existing := range manifest.Connectors {
			present = present || existing.Name == item.Name
		}
		if !present {
			manifest.Connectors = append(manifest.Connectors, Connector{Name: item.Name, Package: item.Package, Mode: item.Mode})
			manifest.Profiles["default"] = append(manifest.Profiles["default"], item.Name)
			found[item.Name] = append(found[item.Name], ".mcp.json")
		}
	}
	manifest.Connectors, manifest.Packages, found = keepReproducibleConnections(absolute, manifest.Connectors, available, found)
	manifest.Profiles["default"] = manifest.Profiles["default"][:0]
	for _, connector := range manifest.Connectors {
		manifest.Profiles["default"] = append(manifest.Profiles["default"], connector.Name)
	}
	sort.Slice(manifest.Connectors, func(i, j int) bool { return manifest.Connectors[i].Name < manifest.Connectors[j].Name })
	sort.Slice(manifest.Packages, func(i, j int) bool { return manifest.Packages[i].Key < manifest.Packages[j].Key })
	sort.Strings(manifest.Profiles["default"])
	return Inference{Manifest: manifest, Evidence: found, Ambiguous: ambiguous}, nil
}

func keepReproducibleConnections(root string, connectors []Connector, available []AvailableConnection, evidence map[string][]string) ([]Connector, []Package, map[string][]string) {
	byName := map[string]AvailableConnection{}
	for _, item := range available {
		byName[item.Name] = item
	}
	var result []Connector
	packages := map[string]Package{}
	for _, connector := range connectors {
		item := byName[connector.Name]
		path, reproducible := reproduciblePackagePath(root, item.Source)
		if !reproducible {
			delete(evidence, connector.Name)
			continue
		}
		if path == "" {
			result = append(result, connector)
			continue
		}
		packages[connector.Package] = Package{Key: connector.Package, Path: path}
		evidence[connector.Name] = append(evidence[connector.Name], "connector package at "+path)
		result = append(result, connector)
	}
	packageList := make([]Package, 0, len(packages))
	for _, item := range packages {
		packageList = append(packageList, item)
	}
	return result, packageList, evidence
}

func reproduciblePackagePath(root, source string) (string, bool) {
	if source == "" || source == "builtin" || strings.HasPrefix(source, "builtin:") {
		return "", true
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	relative, err := filepath.Rel(root, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if _, err := PackagePath(root, relative); err != nil {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func inferenceCorpus(root string) (map[string]string, error) {
	files := []string{".git/config", ".mcp.json", "package.json", "go.mod", "pyproject.toml", "requirements.txt", "Cargo.toml", "README.md", "sentry.properties", ".sentryclirc"}
	result := map[string]string{}
	for _, relative := range files {
		raw, err := os.ReadFile(filepath.Join(root, relative))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(raw) > 1<<20 {
			raw = raw[:1<<20]
		}
		result[relative] = strings.ToLower(string(raw))
	}
	return result, nil
}

func inferMCPServers(root string, available map[string][]AvailableConnection) []AvailableConnection {
	raw, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		return nil
	}
	var value struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var result []AvailableConnection
	keys := make([]string, 0, len(available))
	for key := range available {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for serverName, encoded := range value.Servers {
		text := strings.ToLower(serverName + " " + string(encoded))
		for _, key := range keys {
			connections := available[key]
			if strings.Contains(text, strings.ToLower(key)) && len(connections) > 0 {
				result = append(result, connections[0])
				break
			}
		}
	}
	return result
}

func FormatInference(value Inference) string {
	var body strings.Builder
	for _, connector := range value.Manifest.Connectors {
		fmt.Fprintf(&body, "%s\t%s\t%s\t%s\n", connector.Name, connector.Package, connector.Mode, strings.Join(value.Evidence[connector.Name], ", "))
	}
	keys := make([]string, 0, len(value.Ambiguous))
	for key := range value.Ambiguous {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&body, "AMBIGUOUS\t%s\t%s\tselect one connection explicitly in .outfit/workspace.yaml\n", key, strings.Join(value.Ambiguous[key], ", "))
	}
	return body.String()
}
