package connection

import (
	"path/filepath"
	"strings"
)

// ghAPIValueFlags are the `gh api` flags that take a value and cannot change
// the request method or body. -H is handled separately so a method-override
// header cannot turn a GET into a mutation.
var ghAPIValueFlags = map[string]bool{"--cache": true, "--hostname": true, "-q": true, "--jq": true, "-p": true, "--preview": true, "-t": true, "--template": true}

// ghAPIBoolFlags are the `gh api` flags without a value that only shape output
// or pagination, or print help instead of sending a request.
var ghAPIBoolFlags = map[string]bool{"--allow-escape-sequences": true, "-h": true, "--help": true, "-i": true, "--include": true, "--paginate": true, "--silent": true, "--slurp": true, "--verbose": true}

// ghAPIReadHeaders are the only request headers (lowercased) a read may set.
var ghAPIReadHeaders = map[string]bool{"accept": true, "x-github-api-version": true}

// ghAPIReadOnly reports whether argv (starting at "api") is a `gh api` call
// that provably cannot mutate state: a REST GET or HEAD with no request
// fields or body, or a GraphQL document that is only a query, sent to a
// relative endpoint with at most Accept and X-GitHub-Api-Version headers.
// gh switches an unspecified method to POST when fields are given, so any
// field on a REST call is treated as a write. Any flag it does not
// recognise, a missing flag value, a full URL, or more than one endpoint
// also counts as a write.
func ghAPIReadOnly(argv []string) bool {
	if len(argv) < 2 || argv[0] != "api" {
		return false
	}
	var endpoint, method string
	var fields []string
	help := false
	for index := 1; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "-") {
			if endpoint != "" {
				return false
			}
			endpoint = arg
			continue
		}
		name, value, hasValue := splitGHFlag(arg)
		if ghAPIBoolFlags[name] {
			if hasValue {
				return false
			}
			help = help || name == "-h" || name == "--help"
			continue
		}
		if !ghAPIValueFlags[name] && name != "-X" && name != "--method" && name != "-H" && name != "--header" && name != "-f" && name != "--raw-field" && name != "-F" && name != "--field" {
			return false
		}
		if !hasValue {
			if index+1 >= len(argv) {
				return false
			}
			index++
			value = argv[index]
		}
		switch name {
		case "-X", "--method":
			if method != "" {
				return false
			}
			method = strings.ToUpper(value)
		case "-H", "--header":
			// Only content negotiation is allowed: any other header (such as
			// X-HTTP-Method-Override or X-Method-Override) could change what
			// the request does.
			header, _, ok := strings.Cut(value, ":")
			if !ok || !ghAPIReadHeaders[strings.ToLower(strings.TrimSpace(header))] {
				return false
			}
		case "-f", "--raw-field":
			fields = append(fields, value)
		case "-F", "--field":
			// A typed field can read its value from a file or stdin, so its
			// content is unknown here.
			if _, fieldValue, _ := strings.Cut(value, "="); strings.HasPrefix(fieldValue, "@") {
				return false
			}
			fields = append(fields, value)
		}
	}
	if help {
		return true
	}
	// A full URL could send the gh credential to another host.
	if endpoint == "" || strings.Contains(endpoint, "://") {
		return false
	}
	if endpoint == "graphql" {
		return method == "" && ghGraphQLQueryOnly(fields)
	}
	if len(fields) > 0 {
		return false
	}
	return method == "" || method == "GET" || method == "HEAD"
}

// splitGHFlag splits --name=value and -Xvalue spellings. Short flags in gh
// api are single letters, so anything after the letter is its value.
func splitGHFlag(arg string) (name, value string, hasValue bool) {
	if strings.HasPrefix(arg, "--") {
		return strings.Cut(arg, "=")
	}
	if len(arg) > 2 {
		return arg[:2], strings.TrimPrefix(arg[2:], "="), true
	}
	return arg, "", false
}

// ghGraphQLQueryOnly accepts exactly one query field whose document starts
// as a query and names no mutation or subscription anywhere, so a document
// carrying several operations cannot smuggle one in.
func ghGraphQLQueryOnly(fields []string) bool {
	var document string
	found := false
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		if key != "query" {
			continue
		}
		if found {
			return false
		}
		found = true
		document = value
	}
	if !found {
		return false
	}
	trimmed := strings.TrimSpace(document)
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "mutation") || strings.Contains(lower, "subscription") {
		return false
	}
	if strings.HasPrefix(trimmed, "{") {
		return true
	}
	rest, ok := strings.CutPrefix(trimmed, "query")
	return ok && rest != "" && strings.ContainsAny(rest[:1], " \t\r\n{(")
}

// isGHBinary reports whether a CLI mode runs the GitHub CLI.
func isGHBinary(binary string) bool {
	return filepath.Base(binary) == "gh"
}
