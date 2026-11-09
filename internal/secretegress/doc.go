// Package secretegress flags agent commands that send a credential over the
// network, and plants per-run decoy credentials whose use is always an
// alert (decision I79, Phase 0 of the credential-exfiltration proposal).
//
// Check runs inside the harness hook (`pyred hook claude PreToolUse`) and in
// the Codex adapter's item fold, so it must be fast (no I/O, no network;
// well under a millisecond for an ordinary command) and it only ever flags:
// nothing here blocks or changes a command.
//
// A finding is "high" when a command pairs a secret source (a secret-named
// variable, `gh auth token`, `doppler secrets get`, a credential file, a
// token literal, or an environment dump) with an outbound network sink
// (curl, wget, nc, socat, ssh, an interpreter with an HTTP library,
// Invoke-WebRequest, git with a URL, ...) whose destination is not that
// secret's own provider, or when a decoy appears. It is "low" (recorded in
// the audit log, no push) when every secret goes to its own provider or to
// this machine (`curl -H "Authorization: Bearer $(gh auth token)"
// https://api.github.com/...`), or when a decoy variable is only read.
//
// It is a cheap first layer, not a defense: a script written to a file and
// run later, obfuscated names, and code in files are not seen.
package secretegress
