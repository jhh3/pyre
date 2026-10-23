package connection

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/kit"
)

func TestStripeReadOnlyClassification(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		read bool
	}{
		// The reported calls: both are reads, with or without --live.
		{"retrieve live", []string{"subscriptions", "retrieve", "sub_SYNTHETIC1", "--live"}, true},
		{"get path live", []string{"get", "/v1/subscriptions/sub_SYNTHETIC1", "--live"}, true},
		{"retrieve", []string{"subscriptions", "retrieve", "sub_SYNTHETIC1"}, true},
		{"get path", []string{"get", "/v1/subscriptions/sub_SYNTHETIC1"}, true},
		{"get object id", []string{"get", "cus_SYNTHETIC1"}, true},
		{"get v2 path", []string{"get", "/v2/core/events"}, true},
		{"get query string", []string{"get", "/v1/customers?email=synthetic@example.invalid"}, true},
		{"get pagination and expand", []string{"get", "/v1/charges", "--limit", "50", "-a", "ch_SYNTHETIC1", "--expand", "data.customer", "-e", "data.invoice"}, true},
		{"get live=true", []string{"get", "/v1/account", "--live=true"}, true},
		{"get attached short value", []string{"get", "/v1/charges", "-l50"}, true},
		{"get account and version headers", []string{"get", "/v1/account", "--stripe-account", "acct_SYNTHETIC1", "--stripe-version=2024-06-20", "-s"}, true},
		{"list with params", []string{"subscriptions", "list", "--customer", "cus_SYNTHETIC1", "--status", "active", "--limit", "3", "--live"}, true},
		{"list param=value", []string{"invoices", "list", "--customer=cus_SYNTHETIC1", "--starting-after=in_SYNTHETIC1"}, true},
		{"search", []string{"customers", "search", "--query", "email:'synthetic@example.invalid'", "--live"}, true},
		{"balance retrieve", []string{"balance", "retrieve", "--live"}, true},
		{"namespaced list", []string{"billing", "meters", "list"}, true},
		{"v2 namespaced retrieve", []string{"v2", "core", "events", "retrieve", "evt_SYNTHETIC1"}, true},
		{"logs tail filters", []string{"logs", "tail", "--filter-http-method", "GET", "--format=JSON"}, true},
		{"whoami", []string{"whoami", "--format", "json"}, true},
		{"version", []string{"version"}, true},
		{"help", []string{"help", "customers", "create"}, true},
		{"trailing help", []string{"customers", "create", "--help"}, true},
		{"post help", []string{"post", "/v1/customers", "-d", "name=x", "-h"}, true},

		// Mutations and anything that is not provably a read need --write.
		{"post", []string{"post", "/v1/customers"}, false},
		{"post live", []string{"post", "/v1/subscriptions/sub_SYNTHETIC1", "-d", "cancel_at_period_end=true", "--live"}, false},
		{"delete", []string{"delete", "/v1/customers/cus_SYNTHETIC1"}, false},
		{"create", []string{"customers", "create", "--email", "synthetic@example.invalid"}, false},
		{"update", []string{"subscriptions", "update", "sub_SYNTHETIC1", "--description", "x"}, false},
		{"cancel", []string{"subscriptions", "cancel", "sub_SYNTHETIC1", "--live"}, false},
		{"resource delete", []string{"customers", "delete", "cus_SYNTHETIC1"}, false},
		{"delete of an id named list", []string{"customers", "delete", "list"}, false},
		{"other operation", []string{"invoices", "finalize_invoice", "in_SYNTHETIC1"}, false},
		{"retrieve on resource without it", []string{"account_links", "retrieve", "x"}, false},
		{"unknown resource", []string{"frobnicators", "list"}, false},
		{"get with data", []string{"get", "/v1/customers", "-d", "email=x"}, false},
		{"get with long data", []string{"get", "/v1/customers", "--data=email=x"}, false},
		{"list with data", []string{"customers", "list", "-d", "limit=3"}, false},
		{"retrieve with data", []string{"subscriptions", "retrieve", "sub_SYNTHETIC1", "--data", "x=y"}, false},
		{"list with idempotency", []string{"customers", "list", "-i", "key"}, false},
		{"list with confirm", []string{"customers", "list", "--confirm"}, false},
		{"trigger", []string{"trigger", "payment_intent.succeeded"}, false},
		{"listen", []string{"listen"}, false},
		{"listen forwarding", []string{"listen", "--forward-to", "localhost:4242/webhook"}, false},
		{"fixtures", []string{"fixtures", "fixture.json"}, false},
		{"config list", []string{"config", "--list"}, false},
		{"keys list", []string{"keys", "list"}, false},
		{"sandbox", []string{"sandbox", "create"}, false},
		{"terminal quickstart", []string{"terminal", "quickstart", "list"}, false},
		{"preview post", []string{"preview", "post", "/v2/x"}, false},
		{"get full URL", []string{"get", "https://example.invalid/v1/customers"}, false},
		{"get without target", []string{"get", "--live"}, false},
		{"get two targets", []string{"get", "/v1/customers", "/v1/charges"}, false},
		{"get dot segments", []string{"get", "/v1/../v1/customers"}, false},
		{"get unknown flag", []string{"get", "/v1/customers", "--frobnicate"}, false},
		{"get missing flag value", []string{"get", "/v1/customers", "--limit"}, false},
		{"api base", []string{"customers", "list", "--api-base", "http://127.0.0.1:1"}, false},
		{"get api base", []string{"get", "/v1/customers", "--api-base=http://127.0.0.1:1"}, false},
		{"dashboard base", []string{"customers", "list", "--dashboard-base", "http://127.0.0.1:1"}, false},
		{"api key", []string{"customers", "list", "--api-key", "rk_test_SYNTHETIC"}, false},
		{"project name", []string{"customers", "list", "-p", "other"}, false},
		{"config file", []string{"customers", "list", "--config", "/tmp/x.toml"}, false},
		{"log level", []string{"customers", "list", "--log-level", "debug"}, false},
		{"leading flag", []string{"--live", "customers", "list"}, false},
		{"unknown short flag", []string{"customers", "list", "-z"}, false},
		{"combined short flags", []string{"get", "/v1/customers", "-sh2"}, false},
		{"live=yes", []string{"get", "/v1/account", "--live=yes"}, false},
		{"help as a flag value", []string{"customers", "create", "--name", "--help"}, false},
		{"logs tail unknown flag", []string{"logs", "tail", "--forward-to", "x"}, false},
		{"whoami extra", []string{"whoami", "extra"}, false},
		{"version extra", []string{"version", "extra"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := stripeReadOnly(tc.argv); got != tc.read {
			t.Errorf("%s: stripeReadOnly(%q) = %v, want %v", tc.name, tc.argv, got, tc.read)
		}
	}
}

func TestStripeBinaryUsesStripeRules(t *testing.T) {
	cases := []struct {
		binary string
		argv   []string
		read   bool
	}{
		{"/home/synthetic/.config/outfit/tools/stripe-cli/1.50.4/linux-amd64/stripe", []string{"get", "/v1/account", "--live"}, true},
		{`C:\tools\stripe.exe`, []string{"subscriptions", "retrieve", "sub_SYNTHETIC1", "--live"}, true},
		// The generic verb rule would pass these; the Stripe rules do not.
		{"stripe", []string{"keys", "list"}, false},
		{"stripe", []string{"customers", "delete", "list"}, false},
		{"other", []string{"customers", "delete", "list"}, true},
	}
	for _, tc := range cases {
		if got := cliReadOnly(tc.binary, tc.argv); got != tc.read {
			t.Errorf("cliReadOnly(%q, %q) = %v, want %v", tc.binary, tc.argv, got, tc.read)
		}
	}
	pkg := kit.Package{Tools: []kit.ToolRequirement{{ID: "stripe-cli", Binary: "stripe"}}}
	if got := cliBinaryName(pkg, kit.Mode{Executor: kit.Executor{Kind: "cli", Tool: "stripe-cli"}}); got != "stripe" {
		t.Fatalf("managed tool binary = %q, want stripe", got)
	}
}

func TestStripeRawClassificationMatchesTypedOperations(t *testing.T) {
	// Running a typed operation's own argv through `outfit exec` must give
	// the same answer as its declared class, so the read table cannot drift
	// from the connector's curated surface.
	for _, line := range strings.Split(stripeReadResources, "\n") {
		if line != "" && !strings.Contains(line, ":") {
			t.Fatalf("malformed table line %q", line)
		}
	}
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	var mode kit.Mode
	for _, loaded := range packages {
		if loaded.Package.Key == "stripe" {
			mode, _ = kit.FindMode(loaded.Package, "cli")
		}
	}
	if len(mode.Operations) == 0 {
		t.Fatal("stripe cli mode not found")
	}
	placeholder := regexp.MustCompile(`\{\{arg\.[a-z_]+\}\}`)
	for _, operation := range mode.Operations {
		if operation.Interaction == "stream" {
			continue
		}
		argv := []string{}
		for _, arg := range operation.CLI.Args {
			argv = append(argv, placeholder.ReplaceAllString(arg, "obj_SYNTHETIC1"))
		}
		for _, optional := range operation.CLI.OptionalArgs {
			argv = append(argv, optional.Flag)
			if !optional.Boolean {
				argv = append(argv, "SYNTHETIC")
			}
		}
		want := operation.Class == "read"
		if operation.Name == "invoices_upcoming" {
			// create_preview is a side-effect-free POST: a read when typed,
			// but raw POST-backed commands stay behind --write.
			want = false
		}
		if got := stripeReadOnly(argv); got != want {
			t.Errorf("%s (%s): raw %q read = %v, want %v", operation.Name, operation.Class, argv, got, want)
		}
	}
}

func TestRawCLIConditionArguments(t *testing.T) {
	bindings := []kit.CredentialBinding{{Target: "process-env", Name: "STRIPE_API_KEY", Value: "{{secret.STRIPE_API_KEY}}", UnlessArgument: "live"}, {Target: "process-env", Name: "STRIPE_API_KEY", Value: "{{secret.STRIPE_LIVE_API_KEY}}", WhenArgument: "live"}}
	if got := rawCLIConditionArguments(bindings, []string{"get", "/v1/account", "--live"}); got["live"] != "true" {
		t.Fatalf("--live arguments = %v", got)
	}
	if got := rawCLIConditionArguments(bindings, []string{"get", "/v1/account", "--live=false"}); got["live"] != "false" {
		t.Fatalf("--live=false arguments = %v", got)
	}
	if got := rawCLIConditionArguments(bindings, []string{"get", "/v1/account"}); len(got) != 0 {
		t.Fatalf("test-mode arguments = %v", got)
	}
	if got := rawCLIConditionArguments(nil, []string{"get", "/v1/account", "--live"}); len(got) != 0 {
		t.Fatalf("unconditional bindings arguments = %v", got)
	}
}
