package connection

import "testing"

func TestGHAPIReadOnlyClassification(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		read bool
	}{
		{"implicit GET", []string{"api", "repos/OWNER/REPO"}, true},
		{"explicit --method GET", []string{"api", "--method", "GET", "repos/OWNER/REPO/branches/main/protection"}, true},
		{"explicit -X GET", []string{"api", "-X", "GET", "repos/OWNER/REPO/rulesets"}, true},
		{"attached -XGET", []string{"api", "-XGET", "repos/OWNER/REPO"}, true},
		{"--method=GET", []string{"api", "--method=GET", "repos/OWNER/REPO"}, true},
		{"lowercase get", []string{"api", "-X", "get", "repos/OWNER/REPO"}, true},
		{"HEAD", []string{"api", "-X", "HEAD", "repos/OWNER/REPO", "-i"}, true},
		{"paginate and jq", []string{"api", "repos/OWNER/REPO/rulesets", "--paginate", "--slurp", "--jq", ".[] | .name"}, true},
		{"output and host flags", []string{"api", "-H", "Accept: application/vnd.github.raw+json", "--hostname", "github.example", "--cache", "1h", "-t", "{{.name}}", "-p", "corsair", "--silent", "--verbose", "--include", "repos/OWNER/REPO"}, true},
		{"query string in path", []string{"api", "repos/OWNER/REPO/pulls?state=closed&per_page=100"}, true},
		{"help", []string{"api", "--help"}, true},
		{"help with endpoint", []string{"api", "-X", "DELETE", "repos/OWNER/REPO", "-h"}, true},

		{"POST", []string{"api", "-X", "POST", "repos/OWNER/REPO/issues"}, false},
		{"PATCH", []string{"api", "--method", "PATCH", "repos/OWNER/REPO"}, false},
		{"DELETE", []string{"api", "-X", "DELETE", "repos/OWNER/REPO"}, false},
		{"PUT", []string{"api", "--method=PUT", "repos/OWNER/REPO/branches/main/protection"}, false},
		{"DELETE on a verb-named path", []string{"api", "-X", "DELETE", "status"}, false},
		{"raw field switches to POST", []string{"api", "repos/OWNER/REPO/issues", "-f", "title=x"}, false},
		{"long raw field", []string{"api", "repos/OWNER/REPO/issues", "--raw-field", "title=x"}, false},
		{"typed field", []string{"api", "repos/OWNER/REPO/issues", "-F", "title=x"}, false},
		{"attached typed field", []string{"api", "repos/OWNER/REPO/issues", "-Ftitle=x"}, false},
		{"field with explicit GET", []string{"api", "-X", "GET", "search/code", "-f", "q=x"}, false},
		{"input body", []string{"api", "repos/OWNER/REPO/issues", "--input", "body.json"}, false},
		{"input body explicit GET", []string{"api", "-X", "GET", "repos/OWNER/REPO", "--input", "-"}, false},
		{"method override header", []string{"api", "-H", "X-HTTP-Method-Override: DELETE", "repos/OWNER/REPO"}, false},
		{"method override header lowercase", []string{"api", "--header=x-http-method-override:PATCH", "repos/OWNER/REPO"}, false},
		{"api version and lowercase accept headers", []string{"api", "-H", "X-GitHub-Api-Version: 2022-11-28", "--header=accept:application/json", "repos/OWNER/REPO"}, true},
		{"method header", []string{"api", "-H", "X-HTTP-Method: DELETE", "repos/OWNER/REPO"}, false},
		{"x-method-override header", []string{"api", "-H", "X-Method-Override: DELETE", "repos/OWNER/REPO"}, false},
		{"other header", []string{"api", "-H", "Authorization: token synthetic", "repos/OWNER/REPO"}, false},
		{"header without colon", []string{"api", "-H", "Accept", "repos/OWNER/REPO"}, false},
		{"full https URL", []string{"api", "https://example.invalid/collect"}, false},
		{"full http URL", []string{"api", "-X", "GET", "http://example.invalid/collect"}, false},
		{"graphql full URL", []string{"api", "https://example.invalid/graphql", "-f", "query={ viewer { login } }"}, false},
		{"two methods", []string{"api", "-X", "GET", "-X", "POST", "repos/OWNER/REPO"}, false},
		{"missing method value", []string{"api", "repos/OWNER/REPO", "-X"}, false},
		{"unknown flag", []string{"api", "repos/OWNER/REPO", "--frobnicate"}, false},
		{"unknown short flag", []string{"api", "-z", "repos/OWNER/REPO"}, false},
		{"bool flag with value", []string{"api", "--paginate=false", "repos/OWNER/REPO"}, false},
		{"double dash", []string{"api", "--", "repos/OWNER/REPO"}, false},
		{"two endpoints", []string{"api", "repos/OWNER/REPO", "user"}, false},
		{"no endpoint", []string{"api", "--paginate"}, false},

		{"graphql query keyword", []string{"api", "graphql", "-f", "query=query { viewer { login } }"}, true},
		{"graphql named query", []string{"api", "graphql", "-f", "query=query($owner: String!) { repository(owner: $owner) { name } }", "-F", "owner=OWNER"}, true},
		{"graphql shorthand", []string{"api", "graphql", "--raw-field", "query={ viewer { login } }", "--paginate"}, true},
		{"graphql mutation", []string{"api", "graphql", "-f", "query=mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }"}, false},
		{"graphql query hiding a mutation", []string{"api", "graphql", "-f", "query=query A { viewer { login } } mutation B { addStar(input: {}) { clientMutationId } }", "-f", "operationName=B"}, false},
		{"graphql subscription", []string{"api", "graphql", "-f", "query=subscription { x }"}, false},
		{"graphql query from file", []string{"api", "graphql", "-F", "query=@query.graphql"}, false},
		{"graphql no query", []string{"api", "graphql"}, false},
		{"graphql two queries", []string{"api", "graphql", "-f", "query={ a }", "-f", "query={ b }"}, false},
		{"graphql query-prefixed word", []string{"api", "graphql", "-f", "query=queryX"}, false},
		{"graphql explicit method", []string{"api", "-X", "GET", "graphql", "-f", "query={ viewer { login } }"}, false},
		{"graphql input body", []string{"api", "graphql", "--input", "body.json"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cliReadOnly("gh", tc.argv); got != tc.read {
				t.Fatalf("cliReadOnly(gh, %q) = %v, want %v", tc.argv, got, tc.read)
			}
		})
	}
}

func TestCLIReadOnlyKeepsOtherCommandsOnTheVerbRule(t *testing.T) {
	cases := []struct {
		binary string
		argv   []string
		read   bool
	}{
		{"gh", []string{"pr", "list"}, true},
		{"gh", []string{"pr", "view", "1"}, false},
		{"gh", []string{"pr", "merge", "1"}, false},
		{"/opt/tools/gh", []string{"api", "repos/OWNER/REPO"}, true},
		{"stripe", []string{"api", "repos/OWNER/REPO"}, false},
		{"stripe", []string{"customers", "list"}, true},
	}
	for _, tc := range cases {
		if got := cliReadOnly(tc.binary, tc.argv); got != tc.read {
			t.Fatalf("cliReadOnly(%q, %q) = %v, want %v", tc.binary, tc.argv, got, tc.read)
		}
	}
}
