package importer

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
)

var helpCommand = regexp.MustCompile(`^\s{1,8}([a-z][a-z0-9-]{1,63})(?:\s+[^\s].*?)?\s{2,}(.+)$`)

func importCLI(options Options) (Result, error) {
	binary := options.Binary
	if binary == "" {
		binary = options.Source
	}
	if strings.ContainsAny(binary, `/\\`) {
		binary = strings.TrimSuffix(filepathBase(binary), filepathExt(binary))
	}
	if binary == "" {
		return Result{}, errors.New("--binary is required")
	}
	var operations []kit.Operation
	structured := false
	if document, err := readDocument(options.Source); err == nil {
		operations = completionOperations(document)
		structured = len(operations) > 0
	}
	if !structured {
		help, err := cliHelp(options.Source, options.Binary)
		if err != nil {
			return Result{}, err
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(help, "\n") {
			match := helpCommand.FindStringSubmatch(line)
			if len(match) == 0 || strings.HasPrefix(match[1], "-") || seen[match[1]] {
				continue
			}
			seen[match[1]] = true
			class := classifyCLI(match[1] + " " + match[2])
			operations = append(operations, kit.Operation{Name: strings.ReplaceAll(match[1], "-", "_"), Title: match[1], Description: strings.TrimSpace(match[2]), Class: class, Input: map[string]any{"type": "object", "additionalProperties": true}, CLI: &kit.CLIOperation{Args: []string{match[1]}}})
		}
	}
	if len(operations) == 0 {
		return Result{}, errors.New("no subcommands were found in CLI help; pass a conventional --help file with aligned command descriptions")
	}
	operations = uniqueOperations(operations)
	mode := kit.Mode{ID: "cli", Label: "Native CLI", Description: "Generated from deterministic CLI help/completion metadata and uses the CLI's native login.", Recommendation: kit.Recommendation{Rank: 1, Reason: "The native CLI is the imported command surface."}, Executor: kit.Executor{Kind: "cli", Binary: binary}, Operations: operations}
	for _, operation := range operations {
		if operation.Class == "read" {
			mode.Verify = operation.Name
			break
		}
	}
	warnings := []string{"Mutation classification is conservative and must be reviewed."}
	if !structured {
		warnings = append(warnings, "Help-text import does not execute subcommand help; add exact argument templates and schemas before relying on parameterized operations.")
	}
	return Result{Package: basePackage(options, mode), Warnings: warnings}, nil
}

func completionOperations(document map[string]any) []kit.Operation {
	commands := sliceValue(document["subcommands"])
	if len(commands) == 0 {
		commands = sliceValue(document["commands"])
	}
	var result []kit.Operation
	for _, command := range commands {
		walkCompletion(mapValue(command), nil, &result)
	}
	return result
}

func walkCompletion(command map[string]any, parents []string, result *[]kit.Operation) {
	name := stringValue(command["name"])
	if name == "" {
		return
	}
	path := append(append([]string{}, parents...), name)
	children := sliceValue(command["subcommands"])
	if len(children) == 0 {
		children = sliceValue(command["commands"])
	}
	if len(children) > 0 {
		for _, child := range children {
			walkCompletion(mapValue(child), path, result)
		}
		return
	}
	description := stringValue(command["description"])
	if description == "" {
		description = "Run " + strings.Join(path, " ")
	}
	input := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	properties := input["properties"].(map[string]any)
	args := append([]string{}, path...)
	var optional []kit.CLIOptionalArg
	var required []string
	for _, raw := range sliceValue(command["options"]) {
		option := mapValue(raw)
		flag := completionFlag(option["name"])
		if flag == "" || !strings.HasPrefix(flag, "-") {
			continue
		}
		argument := mapValue(option["args"])
		argumentName := operationName(stringValue(argument["name"]))
		if argumentName == "" || argumentName == "op_" {
			argumentName = operationName(strings.TrimLeft(flag, "-"))
			if argumentName == "" || argumentName == "op_" {
				continue
			}
			properties[argumentName] = map[string]any{"type": "boolean", "description": stringValue(option["description"])}
			optional = append(optional, kit.CLIOptionalArg{Flag: flag, Argument: argumentName, Boolean: true})
			continue
		}
		properties[argumentName] = map[string]any{"type": "string", "description": stringValue(option["description"])}
		optional = append(optional, kit.CLIOptionalArg{Flag: flag, Argument: argumentName})
	}
	for _, raw := range completionArgs(command["args"]) {
		argument := mapValue(raw)
		name := operationName(stringValue(argument["name"]))
		if name == "" || name == "op_" {
			continue
		}
		properties[name] = map[string]any{"type": "string", "description": stringValue(argument["description"])}
		if argument["isOptional"] == true || argument["optional"] == true {
			optional = append(optional, kit.CLIOptionalArg{Argument: name})
		} else {
			args = append(args, "{{arg."+name+"}}")
			required = append(required, name)
		}
	}
	if len(required) > 0 {
		input["required"] = required
	}
	*result = append(*result, kit.Operation{Name: strings.Join(path, "_"), Title: strings.Join(path, " "), Description: description, Class: classifyCLI(strings.Join(path, " ") + " " + description), Input: input, CLI: &kit.CLIOperation{Args: args, OptionalArgs: optional}})
}

func completionFlag(value any) string {
	if text := stringValue(value); text != "" {
		return text
	}
	values := sliceValue(value)
	for index := len(values) - 1; index >= 0; index-- {
		if text := stringValue(values[index]); strings.HasPrefix(text, "--") {
			return text
		}
	}
	if len(values) > 0 {
		return stringValue(values[0])
	}
	return ""
}

func completionArgs(value any) []any {
	if list := sliceValue(value); len(list) > 0 {
		return list
	}
	if object := mapValue(value); len(object) > 0 {
		return []any{object}
	}
	return nil
}

func cliHelp(source, explicitBinary string) (string, error) {
	if source != "" {
		if raw, err := os.ReadFile(source); err == nil {
			return string(raw), nil
		}
	}
	binary := explicitBinary
	if binary == "" {
		binary = source
	}
	if binary == "" || strings.ContainsAny(binary, `/\\`) {
		return "", errors.New("help file was not readable and executable must be a PATH binary")
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", err
	}
	output, err := exec.Command(path, "--help").CombinedOutput()
	if err != nil {
		return "", errors.New("CLI --help failed: " + strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func classifyCLI(value string) string {
	value = strings.ToLower(value)
	for _, word := range []string{"delete", "remove", "destroy", "revoke", "purge", "drop"} {
		if strings.Contains(value, word) {
			return "destructive"
		}
	}
	for _, word := range []string{"create", "add", "update", "edit", "set", "send", "write", "apply", "deploy", "push", "merge", "close"} {
		if strings.Contains(value, word) {
			return "write"
		}
	}
	return "read"
}

func filepathBase(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	parts := strings.Split(value, "/")
	return parts[len(parts)-1]
}

func filepathExt(value string) string {
	base := filepathBase(value)
	if index := strings.LastIndex(base, "."); index >= 0 {
		return base[index:]
	}
	return ""
}
