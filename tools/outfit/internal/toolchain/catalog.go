package toolchain

import "github.com/jhh3/outfit/internal/kit"

// CatalogEntry is the human-facing registry record used by search, the TUI,
// and package authors. A connector package owns the executable contract; this
// registry makes the broader blessed workstation discoverable before every
// connector has a complete vertical package.
type CatalogEntry struct {
	ID          string
	Display     string
	Binary      string
	Category    string
	Description string
	Requirement kit.ToolRequirement
}

var catalog = []CatalogEntry{
	{ID: "gws", Display: "Google Workspace CLI", Binary: "gws", Category: "productivity", Description: "Gmail, Drive, Calendar, Sheets, Docs, Chat, Admin, and Meet", Requirement: GWSRequirement()},
	{ID: "stripe-cli", Display: "Stripe CLI", Binary: "stripe", Category: "customer-systems", Description: "Payments, customers, subscriptions, logs, fixtures, and webhooks", Requirement: StripeRequirement()},
	{ID: "github-cli", Display: "GitHub CLI", Binary: "gh", Category: "developer", Description: "Repositories, issues, pull requests, Actions, releases, and API"},
	{ID: "sentry-cli", Display: "Sentry CLI", Binary: "sentry-cli", Category: "operations", Description: "Releases, deploys, sourcemaps, symbols, events, and projects"},
	{ID: "atlassian-cli", Display: "Atlassian CLI", Binary: "acli", Category: "customer-systems", Description: "Jira work items, projects, sprints, and Rovo"},
	{ID: "salesforce-cli", Display: "Salesforce CLI", Binary: "sf", Category: "customer-systems", Description: "Organizations, SOQL, metadata, deploys, tests, and data"},
	{ID: "google-cloud-cli", Display: "Google Cloud CLI", Binary: "gcloud", Category: "cloud-data", Description: "Google Cloud and BigQuery project operations"},
	{ID: "aws-cli", Display: "AWS CLI", Binary: "aws", Category: "cloud-data", Description: "AWS accounts, regions, and service APIs"},
	{ID: "azure-cli", Display: "Azure CLI", Binary: "az", Category: "cloud-data", Description: "Azure subscriptions, resource groups, and services"},
	{ID: "databricks-cli", Display: "Databricks CLI", Binary: "databricks", Category: "cloud-data", Description: "Workspaces, SQL warehouses, jobs, clusters, and Unity Catalog"},
	{ID: "snowflake-cli", Display: "Snowflake CLI", Binary: "snow", Category: "cloud-data", Description: "Queries, objects, stages, procedures, and projects"},
	{ID: "atlas-cli", Display: "MongoDB Atlas CLI", Binary: "atlas", Category: "cloud-data", Description: "Projects, clusters, alerts, backups, and access lists"},
	{ID: "supabase-cli", Display: "Supabase CLI", Binary: "supabase", Category: "cloud-data", Description: "Projects, migrations, functions, and generated types"},
	{ID: "kubectl", Display: "Kubernetes CLI", Binary: "kubectl", Category: "infrastructure", Description: "Cluster resources, diagnostics, logs, and rollout operations"},
	{ID: "helm", Display: "Helm", Binary: "helm", Category: "infrastructure", Description: "Kubernetes charts, releases, repositories, and templates"},
	{ID: "terraform", Display: "Terraform", Binary: "terraform", Category: "infrastructure", Description: "Infrastructure plans, state, validation, and apply"},
	{ID: "wrangler", Display: "Cloudflare Wrangler", Binary: "wrangler", Category: "infrastructure", Description: "Workers, Pages, storage, queues, and Cloudflare resources"},
	{ID: "vercel-cli", Display: "Vercel CLI", Binary: "vercel", Category: "infrastructure", Description: "Projects, deployments, environment, domains, and logs"},
	{ID: "flyctl", Display: "Fly.io CLI", Binary: "flyctl", Category: "infrastructure", Description: "Applications, machines, volumes, secrets, deploys, and logs"},
	{ID: "docker", Display: "Docker CLI", Binary: "docker", Category: "infrastructure", Description: "Containers, images, builds, registries, and Compose"},
	{ID: "hubspot-cli", Display: "HubSpot CLI", Binary: "hs", Category: "customer-systems", Description: "CRM schema, projects, serverless functions, and development"},
	{ID: "linear-cli", Display: "Linear CLI", Binary: "linear", Category: "customer-systems", Description: "Issues, projects, teams, and workflows"},
	{ID: "notion-cli", Display: "Notion CLI", Binary: "notion", Category: "productivity", Description: "Pages, databases, search, and workspace content"},
	{ID: "intercom-cli", Display: "Intercom CLI", Binary: "intercom", Category: "customer-systems", Description: "Contacts, companies, conversations, and support operations"},
	{ID: "posthog-cli", Display: "PostHog CLI", Binary: "posthog", Category: "operations", Description: "Projects, feature flags, events, and product analytics"},
}

func Catalog() []CatalogEntry {
	result := make([]CatalogEntry, len(catalog))
	copy(result, catalog)
	return result
}

func CatalogByID(id string) (CatalogEntry, bool) {
	for _, entry := range catalog {
		if entry.ID == id {
			return entry, true
		}
	}
	return CatalogEntry{}, false
}

func StripeRequirement() kit.ToolRequirement {
	return kit.ToolRequirement{
		ID: "stripe-cli", Display: "Stripe CLI", Binary: "stripe", Version: "1.50.4", Verify: []string{"version"},
		Source: kit.ToolSource{
			Kind: "github-release", Repository: "stripe/stripe-cli", Tag: "v{{version}}", BinaryPath: "stripe",
			Assets: map[string]string{
				"darwin-amd64": "stripe_1.50.4_mac-os_x86_64.tar.gz", "darwin-arm64": "stripe_1.50.4_mac-os_arm64.tar.gz",
				"linux-amd64": "stripe_1.50.4_linux_x86_64.tar.gz", "linux-arm64": "stripe_1.50.4_linux_arm64.tar.gz",
				"windows-amd64": "stripe_1.50.4_windows_x86_64.zip",
			},
			Checksums: map[string]string{
				"darwin-amd64": "30f85c4988d1e5d42a65d97a87265e6e4865fbf01083e6b3d1d90d2994446622", "darwin-arm64": "5dbf4bc580c3870d3f7cd0c56fde3c8a210340b59d45050a0c9c0ceb2110dc2f",
				"linux-amd64": "f6a04bbb318e678c20bb62f1ceb2bb6b03b47173189e08e043f7a6b4d9916bc2", "linux-arm64": "35ff9ba4f861a10a94e1dff074b06b5fb36b0b3848caeeace995317de0f03f6e",
				"windows-amd64": "69a7fbefa337e2a896d59540a7cf252ebca939eaf6f66abb011e8e929592e67a",
			},
		},
	}
}

func GWSRequirement() kit.ToolRequirement {
	return kit.ToolRequirement{
		ID: "gws", Display: "Google Workspace CLI", Binary: "gws", Version: "0.22.5", Verify: []string{"--version"},
		Source: kit.ToolSource{
			Kind: "github-release", Repository: "googleworkspace/cli", Tag: "v{{version}}", BinaryPath: "gws",
			Assets: map[string]string{
				"darwin-amd64": "google-workspace-cli-x86_64-apple-darwin.tar.gz", "darwin-arm64": "google-workspace-cli-aarch64-apple-darwin.tar.gz",
				"linux-amd64": "google-workspace-cli-x86_64-unknown-linux-gnu.tar.gz", "linux-arm64": "google-workspace-cli-aarch64-unknown-linux-gnu.tar.gz",
				"windows-amd64": "google-workspace-cli-x86_64-pc-windows-msvc.zip",
			},
			Checksums: map[string]string{
				"darwin-amd64": "51f9bd731404d4bba26c36e2e30dd68c56dccd1f834c01252cb0b14d6a6544b2", "darwin-arm64": "1d2a9ffd5bc9b2c2c4b48630daf082fad13d9e57d741988a2c248eed562f7dac",
				"linux-amd64": "de78ecdbd2f1a84cca0063a7ecbc440240fc14b6ebccbb17f4646b792a8c5c1f", "linux-arm64": "94490295d9580e1e88574e715a0a162991747d12d62f8c7b8dcc8268b6c1cea0",
				"windows-amd64": "407705d695dc83d48b1c5f50d71b5aa64095bf6f17d5b439b2e9a373bbe67ec2",
			},
		},
	}
}
