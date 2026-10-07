package kit

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
