package connection

import (
	"path/filepath"
	"regexp"
	"strings"
)

// stripeReadResources lists, for the pinned Stripe CLI (1.50.4), each
// resource command (namespace words first) that has retrieve, list, or
// search operations, and which of them. All three are HTTP GETs. Generated
// from `stripe resources help` and each resource's --help; a resource or
// operation missing here (a newer CLI's addition) needs --write.
const stripeReadResources = `
accounts: list retrieve
apple_pay_domains: list retrieve
application_fees: list retrieve
balance: retrieve
balance_settings: retrieve
balance_transactions: list retrieve
billing alerts: list retrieve
billing credit_balance_summaries: retrieve
billing credit_balance_transactions: list retrieve
billing credit_grants: list retrieve
billing meter_event_summaries: list
billing meters: list retrieve
billing_portal configurations: list retrieve
capabilities: list retrieve
cash_balances: retrieve
charges: list retrieve search
checkout sessions: list retrieve
climate orders: list retrieve
climate products: list retrieve
climate suppliers: list retrieve
confirmation_tokens: retrieve
country_specs: list retrieve
coupons: list retrieve
credit_note_line_items: list
credit_notes: list retrieve
customer_balance_transactions: list retrieve
customer_cash_balance_transactions: list retrieve
customers: list retrieve search
disputes: list retrieve
entitlements active_entitlements: list retrieve
entitlements features: list retrieve
events: list retrieve
exchange_rates: list retrieve
external_accounts: list retrieve
fee_refunds: list retrieve
file_links: list retrieve
files: list retrieve
financial_connections accounts: list retrieve
financial_connections sessions: retrieve
financial_connections transactions: list retrieve
forwarding requests: list retrieve
identity verification_reports: list retrieve
identity verification_sessions: list retrieve
invoice_line_items: list
invoice_payments: list retrieve
invoice_rendering_templates: list retrieve
invoiceitems: list retrieve
invoices: list retrieve search
issuing authorizations: list retrieve
issuing cardholders: list retrieve
issuing cards: list retrieve
issuing disputes: list retrieve
issuing personalization_designs: list retrieve
issuing physical_bundles: list retrieve
issuing tokens: list retrieve
issuing transactions: list retrieve
mandates: retrieve
payment_attempt_records: list retrieve
payment_intent_amount_details_line_items: list
payment_intents: list retrieve search
payment_links: list retrieve
payment_method_configurations: list retrieve
payment_method_domains: list retrieve
payment_methods: list retrieve
payment_records: list retrieve
payment_sources: list retrieve
payouts: list retrieve
persons: list retrieve
plans: list retrieve
prices: list retrieve search
product_features: list retrieve
products: list retrieve search
promotion_codes: list retrieve
quotes: list retrieve
radar early_fraud_warnings: list retrieve
radar value_list_items: list retrieve
radar value_lists: list retrieve
refunds: list retrieve
reviews: list retrieve
scheduled_query_runs: list retrieve
setup_attempts: list
setup_intents: list retrieve
shipping_rates: list retrieve
sources: retrieve
subscription_items: list retrieve
subscription_schedules: list retrieve
subscriptions: list retrieve search
tax calculations: retrieve
tax registrations: list retrieve
tax settings: retrieve
tax transactions: retrieve
tax_codes: list retrieve
tax_ids: list retrieve
tax_rates: list retrieve
terminal configurations: list retrieve
terminal locations: list retrieve
terminal readers: list retrieve
test_helpers test_clocks: list retrieve
tokens: retrieve
topups: list retrieve
transfer_reversals: list retrieve
transfers: list retrieve
treasury credit_reversals: list retrieve
treasury debit_reversals: list retrieve
treasury financial_accounts: list retrieve
treasury inbound_transfers: list retrieve
treasury outbound_payments: list retrieve
treasury outbound_transfers: list retrieve
treasury received_credits: list retrieve
treasury received_debits: list retrieve
treasury transaction_entrys: list retrieve
treasury transactions: list retrieve
v2 commerce product_catalog_imports: list retrieve
v2 core account_person_tokens: retrieve
v2 core account_persons: list retrieve
v2 core account_tokens: retrieve
v2 core accounts: list retrieve
v2 core event_destinations: list retrieve
v2 core events: list retrieve
webhook_endpoints: list retrieve
`

var stripeReadOperations = parseStripeReadResources(stripeReadResources)

func parseStripeReadResources(table string) map[string]map[string]bool {
	result := map[string]map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		resource, operations, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		result[resource] = map[string]bool{}
		for _, operation := range strings.Fields(operations) {
			result[resource][operation] = true
		}
	}
	return result
}

// stripeBoolFlags take no value and only select the live key, shape output,
// or print help.
var stripeBoolFlags = map[string]bool{"--live": true, "--show-headers": true, "-s": true, "--dark-style": true, "--help": true, "-h": true}

// stripeValueFlags take a value and cannot change the request method, body,
// destination, or credential.
var stripeValueFlags = map[string]bool{"--expand": true, "-e": true, "--limit": true, "-l": true, "--starting-after": true, "-a": true, "--ending-before": true, "-b": true, "--stripe-account": true, "--stripe-context": true, "--stripe-version": true, "-v": true, "--color": true}

// stripeDeniedFlags are never part of a read: a request body (-d), an
// idempotency key or confirmation (write-only), another API key, profile,
// config file, device, API host (--api-base and the other hidden *-base
// flags), or log level (debug logging can print request headers).
var stripeDeniedFlags = map[string]bool{"--data": true, "-d": true, "--idempotency": true, "-i": true, "--confirm": true, "-c": true, "--api-key": true, "--project-name": true, "-p": true, "--config": true, "--device-name": true, "--log-level": true, "--map": true}

var stripeParamFlag = regexp.MustCompile(`^--[a-z0-9][a-z0-9-]*$`)

var stripeObjectID = regexp.MustCompile(`^[a-z]+_[A-Za-z0-9_]+$`)

// stripeReadOnly reports whether argv for the Stripe CLI provably cannot
// mutate state. It allows only:
//   - get <path or id>: an HTTP GET of a relative /v1/ or /v2/ path (or an
//     object ID), with output, pagination, expand, account, and version
//     flags;
//   - [namespace...] <resource> retrieve|list|search [ids], for the pinned
//     resources above, with those flags plus --param value request
//     parameters (query parameters of the GET);
//   - logs tail with --filter-* and --format, whoami, version, and help;
//
// each with or without --live. Everything else, including post, delete,
// create, update, cancel, trigger, listen, any -d/--data, a flag that
// changes the API host, key, or profile, and anything unrecognised, is a
// write.
func stripeReadOnly(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if stripeHelpOnly(argv) {
		return true
	}
	switch argv[0] {
	case "get":
		return stripeGetReadOnly(argv[1:])
	case "version":
		return len(argv) == 1
	case "whoami":
		return stripeOnlyFlags(argv[1:], func(name string) bool { return name == "--format" })
	case "logs":
		return len(argv) >= 2 && argv[1] == "tail" && stripeOnlyFlags(argv[2:], func(name string) bool {
			return name == "--format" || (strings.HasPrefix(name, "--filter-") && stripeParamFlag.MatchString(name))
		})
	}
	for words := 1; words <= 3 && words < len(argv); words++ {
		operations, ok := stripeReadOperations[strings.Join(argv[:words], " ")]
		if !ok {
			continue
		}
		return operations[argv[words]] && stripeResourceFlagsReadOnly(argv[words+1:])
	}
	return false
}

// stripeHelpOnly accepts `help ...` and a trailing --help/-h that cannot be
// the value of a preceding flag (`--name --help` sets a name).
func stripeHelpOnly(argv []string) bool {
	if argv[0] == "help" {
		return true
	}
	last := argv[len(argv)-1]
	if last != "--help" && last != "-h" {
		return false
	}
	return len(argv) == 1 || !strings.HasPrefix(argv[len(argv)-2], "-")
}

func stripeGetReadOnly(argv []string) bool {
	var target string
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "-") {
			if target != "" {
				return false
			}
			target = arg
			continue
		}
		name, _, hasValue := splitStripeFlag(arg)
		switch {
		case stripeBoolFlags[name]:
			if hasValue && !stripeBoolValue(arg) {
				return false
			}
		case stripeValueFlags[name]:
			if !hasValue {
				if index+1 >= len(argv) {
					return false
				}
				index++
			}
		default:
			return false
		}
	}
	return stripeGetTarget(target)
}

// stripeGetTarget accepts a relative API path or a bare object ID; a full
// URL could send the key to another host.
func stripeGetTarget(target string) bool {
	if strings.Contains(target, "://") || strings.Contains(target, "..") || strings.ContainsAny(target, "\\ \t\r\n") {
		return false
	}
	if strings.HasPrefix(target, "/v1/") || strings.HasPrefix(target, "/v2/") {
		return true
	}
	return stripeObjectID.MatchString(target)
}

func stripeResourceFlagsReadOnly(argv []string) bool {
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "://") {
				return false
			}
			continue
		}
		name, _, hasValue := splitStripeFlag(arg)
		switch {
		case stripeDeniedFlags[name] || strings.HasSuffix(name, "-base"):
			return false
		case stripeBoolFlags[name]:
			if hasValue && !stripeBoolValue(arg) {
				return false
			}
		case stripeValueFlags[name] || (strings.HasPrefix(name, "--") && stripeParamFlag.MatchString(name)):
			// A --param is a query parameter of the GET. It may also be a
			// boolean parameter, so the next word is taken as its value
			// only when it is not a flag; either way nothing is sent in a
			// body.
			if !hasValue && index+1 < len(argv) && !strings.HasPrefix(argv[index+1], "-") {
				index++
			} else if !hasValue && !strings.HasPrefix(name, "--") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// stripeOnlyFlags accepts argv made only of flags the predicate allows,
// each with a value (--name value or --name=value).
func stripeOnlyFlags(argv []string, allowed func(string) bool) bool {
	for index := 0; index < len(argv); index++ {
		name, _, hasValue := splitStripeFlag(argv[index])
		if !strings.HasPrefix(argv[index], "--") || !allowed(name) {
			return false
		}
		if !hasValue {
			if index+1 >= len(argv) {
				return false
			}
			index++
		}
	}
	return true
}

// splitStripeFlag splits --name=value and -xVALUE spellings (pflag short
// flags are single letters).
func splitStripeFlag(arg string) (name, value string, hasValue bool) {
	if strings.HasPrefix(arg, "--") {
		return strings.Cut(arg, "=")
	}
	if len(arg) > 2 {
		return arg[:2], strings.TrimPrefix(arg[2:], "="), true
	}
	return arg, "", false
}

// stripeBoolValue accepts --live=true and --live=false spellings; a
// short boolean flag with letters attached (-sh) is a combined flag group
// and is not accepted.
func stripeBoolValue(arg string) bool {
	if !strings.HasPrefix(arg, "--") {
		return false
	}
	_, value, _ := strings.Cut(arg, "=")
	return value == "true" || value == "false"
}

// isStripeBinary reports whether a CLI mode runs the Stripe CLI.
func isStripeBinary(binary string) bool {
	base := filepath.Base(strings.ReplaceAll(binary, `\`, "/"))
	return base == "stripe" || base == "stripe.exe"
}
