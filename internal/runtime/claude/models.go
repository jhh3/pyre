package claude

import "github.com/jhh3/pyre/internal/contracts"

// Efforts are Claude Code's --effort levels (2.1.299 help).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Models are the model aliases Claude Code resolves to the latest model of
// each family (`claude --model <alias>`); a full model name is also
// accepted as free text. Claude Code has no command that lists models, so
// the list is static.
func Models() []contracts.ModelChoice {
	return []contracts.ModelChoice{
		{ID: "opus", Label: "Opus (latest)"},
		{ID: "sonnet", Label: "Sonnet (latest)"},
		{ID: "haiku", Label: "Haiku (latest)"},
		{ID: "fable", Label: "Fable (latest)"},
	}
}
