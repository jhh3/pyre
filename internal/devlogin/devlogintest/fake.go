// Package devlogintest provides a synthetic stand-in for `codex login
// --device-auth` for tests: it prints the real CLI's layout (with ANSI
// colours) and, on approval, writes an obviously synthetic auth.json into
// $CODEX_HOME.
//
// Environment:
//
//	FAKE_CODEX_MODE     approve (default) | fail | hang
//	FAKE_CODEX_APPROVE  approve only once this file exists (default: at once)
//	FAKE_CODEX_EMAIL    account email (default person@synthetic.invalid)
//	FAKE_CODEX_ACCOUNT  chatgpt account id (default synthetic-acct-new)
package devlogintest

import (
	"os"
	"path/filepath"
)

// Code is the one-time code the fake prints.
const Code = "SYNT-H3TIC"

// URL is the verification URL the fake prints (the real one).
const URL = "https://auth.openai.com/codex/device"

const script = `#!/bin/bash
# Synthetic stand-in for "codex login --device-auth" (Pyre tests only).
if [ "$1 $2" != "login --device-auth" ]; then echo "fake codex: unexpected args $*" >&2; exit 2; fi
printf '\nWelcome to Codex [v\033[90m0.0.0-synthetic\033[0m]\n\033[90mOpenAI'"'"'s command-line coding agent\033[0m\n\n'
printf 'Follow these steps to sign in with ChatGPT using device code authorization:\n\n'
printf '1. Open this link in your browser and sign in to your account\n   \033[94mhttps://auth.openai.com/codex/device\033[0m\n\n'
printf '2. Enter this one-time code \033[90m(expires in 15 minutes)\033[0m\n   \033[94mSYNT-H3TIC\033[0m\n\n'
case "${FAKE_CODEX_MODE:-approve}" in
fail) echo "Error: device code request failed (synthetic)" >&2; exit 1 ;;
hang) while true; do sleep 1; done ;;
esac
while [ -n "$FAKE_CODEX_APPROVE" ] && [ ! -e "$FAKE_CODEX_APPROVE" ]; do sleep 0.05; done
email=${FAKE_CODEX_EMAIL:-person@synthetic.invalid}
acct=${FAKE_CODEX_ACCOUNT:-synthetic-acct-new}
b64() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
hdr=$(b64 '{"alg":"none","typ":"JWT"}')
pay=$(b64 "{\"email\":\"$email\",\"pyre_synthetic\":true,\"https://api.openai.com/auth\":{\"chatgpt_account_id\":\"$acct\",\"chatgpt_plan_type\":\"pro\"}}")
umask 077
printf '{"OPENAI_API_KEY":null,"auth_mode":"chatgpt","tokens":{"id_token":"%s.%s.sig","access_token":"pyre-synthetic-access","refresh_token":"pyre-synthetic-refresh","account_id":"%s"}}\n' "$hdr" "$pay" "$acct" > "$CODEX_HOME/auth.json"
echo "Successfully logged in"
`

// Write installs the fake as dir/codex and returns its path.
func Write(dir string) (string, error) {
	p := filepath.Join(dir, "codex")
	return p, os.WriteFile(p, []byte(script), 0o755)
}
