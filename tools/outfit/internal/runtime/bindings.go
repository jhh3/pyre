package runtime

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
)

// ForArguments keeps only the credential bindings whose whenArgument /
// unlessArgument condition holds for this call. With a secret map (a real
// call, not a plan), a selected binding that references a secret field the
// connection does not store is an error: falling back to the other binding
// would silently use the wrong credential (a Stripe test key for a live
// read, say).
func (credentials CredentialContext) ForArguments(arguments, secret map[string]string) (CredentialContext, error) {
	if len(kit.BindingConditions(credentials.Bindings)) == 0 {
		return credentials, nil
	}
	selected := make([]kit.CredentialBinding, 0, len(credentials.Bindings))
	for _, binding := range credentials.Bindings {
		enabled, err := bindingEnabled(binding, arguments)
		if err != nil {
			return CredentialContext{}, err
		}
		if !enabled {
			continue
		}
		if secret != nil && binding.WhenArgument != "" {
			for _, match := range placeholder.FindAllStringSubmatch(binding.Value, -1) {
				if match[1] == "secret" && strings.TrimSpace(secret[match[2]]) == "" {
					return CredentialContext{}, missingConditionalSecret(credentials.Method, binding.WhenArgument, match[2])
				}
			}
		}
		selected = append(selected, binding)
	}
	credentials.Bindings = selected
	return credentials, nil
}

func missingConditionalSecret(method kit.CredentialMethod, argument, field string) error {
	hint := "add it with `outfit credential replace CONNECTION`"
	for _, declared := range method.Fields {
		if declared.Key == field && len(declared.Environment) > 0 {
			hint += " with " + declared.Environment[0] + " set in its environment"
		}
	}
	return fmt.Errorf("--%s needs this connection's %s credential, which is not stored; %s", strings.ReplaceAll(argument, "_", "-"), field, hint)
}

func bindingEnabled(binding kit.CredentialBinding, arguments map[string]string) (bool, error) {
	name := binding.WhenArgument
	if name == "" {
		name = binding.UnlessArgument
	}
	if name == "" {
		return true, nil
	}
	value := false
	if raw := strings.TrimSpace(arguments[name]); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("argument %s must be true or false", name)
		}
		value = parsed
	}
	if binding.WhenArgument != "" {
		return value, nil
	}
	return !value, nil
}
