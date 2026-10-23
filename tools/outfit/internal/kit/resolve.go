package kit

import "sort"

func FindMode(pkg Package, id string) (Mode, bool) {
	for _, mode := range pkg.Modes {
		if mode.ID == id {
			return mode, true
		}
	}
	return Mode{}, false
}

func FindCredentialMethod(pkg Package, id string) (CredentialMethod, bool) {
	for _, method := range pkg.CredentialMethods {
		if method.ID == id {
			return method, true
		}
	}
	return CredentialMethod{}, false
}

// BindingConditions returns the operation arguments that choose between
// conditional credential bindings (whenArgument / unlessArgument), sorted.
func BindingConditions(bindings []CredentialBinding) []string {
	seen := map[string]bool{}
	var names []string
	for _, binding := range bindings {
		for _, name := range []string{binding.WhenArgument, binding.UnlessArgument} {
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func CredentialBindingsFor(mode Mode, method CredentialMethod) []CredentialBinding {
	if bindings, ok := mode.CredentialBindings[method.ID]; ok {
		return append([]CredentialBinding{}, bindings...)
	}
	return append([]CredentialBinding{}, method.Bindings...)
}

func RecommendedMode(pkg Package) (Mode, bool) {
	for _, mode := range pkg.Modes {
		if mode.Recommendation.Rank == 1 {
			return mode, true
		}
	}
	return Mode{}, false
}
