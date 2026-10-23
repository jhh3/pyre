package runtime

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
)

// ProviderError is a call the provider answered with an error: a non-2xx
// HTTP response, or a CLI that printed the provider's error object. Status
// is the upstream HTTP status when known; Code is the provider's own error
// code (Stripe's resource_missing) when it gave one.
type ProviderError struct {
	Status  int
	Code    string
	Message string
}

func (failure *ProviderError) Error() string {
	text := "upstream reported an error"
	if failure.Status > 0 {
		text = "upstream returned " + statusText(failure.Status)
	}
	detail := failure.Message
	if failure.Code != "" {
		if detail == "" {
			detail = failure.Code
		} else {
			detail += " (" + failure.Code + ")"
		}
	}
	if detail != "" {
		text += ": " + detail
	}
	return text
}

func statusText(status int) string {
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprint(status)
}

// errorObjectCode returns the code of a nested provider error object
// ({"error": {"code": "resource_missing", "type": ...}}), falling back to
// its type.
func errorObjectCode(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"code", "type"} {
		if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// cliProviderError reports the provider error a CLI printed on a zero exit,
// when the mode declares executor.errorField. The status comes from
// executor.errorStatus, keyed by the error's code and then its type.
func cliProviderError(executor kit.Executor, data any) *ProviderError {
	if executor.ErrorField == "" {
		return nil
	}
	payload, ok := data.(map[string]any)
	if !ok || !envelopeValuePresent(payload[executor.ErrorField]) {
		return nil
	}
	value := payload[executor.ErrorField]
	failure := &ProviderError{Code: errorObjectCode(value), Message: providerErrorText(value)}
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"code", "type"} {
			if text, ok := object[key].(string); ok {
				if status, ok := executor.ErrorStatus[text]; ok {
					failure.Status = status
					break
				}
			}
		}
	}
	if failure.Message == "" && failure.Code == "" {
		failure.Message = "the CLI printed an error in " + executor.ErrorField
	}
	return failure
}
