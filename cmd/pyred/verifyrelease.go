package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/updates"
)

// exitRefused is install.sh's "refused" exit code (bad signature, checksum
// mismatch).
const exitRefused = 3

// cmdVerifyRelease verifies a release SHA256SUMS signature the way
// `ssh-keygen -Y verify -n pyre-release -I pyre-release` does (Ed25519
// SSHSIG, internal/updates), for installers on machines whose OpenSSH lacks
// -Y (Windows OpenSSH before 8.1). With --file it also checks that file's
// SHA-256 against the verified SHA256SUMS. Installers must run an already
// trusted pyred for this, never one from the archive being verified.
//
// Exit codes: 0 verified, 2 usage, 3 refused (signature or checksum did not
// verify), 1 other errors.
func cmdVerifyRelease(args []string) error {
	fs := flag.NewFlagSet("verify-release", flag.ContinueOnError)
	signers := fs.String("allowed-signers", "", "OpenSSH allowed-signers file with the pyre-release key")
	sums := fs.String("sums", "", "SHA256SUMS file")
	sig := fs.String("sig", "", "signature (default <sums>.sig)")
	file := fs.String("file", "", "also check this file's SHA-256 against the verified SHA256SUMS")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *signers == "" || *sums == "" {
		fmt.Fprintln(os.Stderr, "pyred verify-release: --allowed-signers and --sums are required")
		os.Exit(2)
	}
	if *sig == "" {
		*sig = *sums + ".sig"
	}
	refuse := func(format string, a ...any) error {
		fmt.Fprintf(os.Stderr, "pyred verify-release: REFUSED: "+format+"\n", a...)
		os.Exit(exitRefused)
		return nil
	}
	sb, err := os.ReadFile(*signers)
	if err != nil {
		return err
	}
	allowed, err := updates.ParseAllowedSigners(sb)
	if err != nil {
		return refuse("%s: %v", *signers, err)
	}
	sumsData, err := readLimited(*sums, 1<<20)
	if err != nil {
		return err
	}
	sigData, err := readLimited(*sig, 1<<16)
	if err != nil {
		return refuse("no signature: %v", err)
	}
	key, err := updates.VerifySSHSig(allowed, updates.ReleaseNamespace, updates.ReleaseNamespace, sumsData, sigData)
	if err != nil {
		return refuse("%s: %v", *sums, err)
	}
	sum := sha256.Sum256(key)
	fmt.Printf("Good \"%s\" signature for %s with ED25519 key (sha256 of key bytes %s)\n", updates.ReleaseNamespace, updates.ReleaseNamespace, hex.EncodeToString(sum[:8]))
	if *file == "" {
		return nil
	}
	want, ok := updates.SumsDigest(sumsData, filepath.Base(*file))
	if !ok {
		return refuse("%s is not listed in %s", filepath.Base(*file), *sums)
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, string(want)) {
		return refuse("%s checksum mismatch (expected %s, got %s)", *file, want, got)
	}
	fmt.Printf("%s: checksum verified against the signed SHA256SUMS\n", filepath.Base(*file))
	return nil
}

func readLimited(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New(p + " is implausibly large")
	}
	return b, nil
}
