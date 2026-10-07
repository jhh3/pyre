package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
)

// Without --name, `pyre pair` sends no client name (not the host name), so
// the hub keeps the name given to `pyre pair-client --name`.
func TestPairWithoutNameKeepsPairingName(t *testing.T) {
	names := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+c.PathPairExchange, func(w http.ResponseWriter, r *http.Request) {
		var ex c.PairingExchange
		_ = json.NewDecoder(r.Body).Decode(&ex)
		names = append(names, ex.ClientName)
		json.NewEncoder(w).Encode(c.ClientCredential{ClientID: "cli_paired", Token: "pyc_synthetic-paired-token"})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	link := enrollment.PairingURI{HubURL: srv.URL, PinSHA256: client.CertFingerprint(srv.Certificate().Raw), PairingID: "pair_synthetic", Code: "pyp_synthetic-code"}.String()
	e := newEnv(t, nil)
	expectCode(t, run(t, e, &App{Stdin: strings.NewReader(link + "\n")}, "--json", "pair"), 0)
	expectCode(t, run(t, e, &App{Stdin: strings.NewReader(link + "\n")}, "--json", "pair", "--force", "--name", "laptop"), 0)
	if len(names) != 2 || names[0] != "" || names[1] != "laptop" {
		t.Fatalf("exchanged names %q, want [\"\" \"laptop\"]", names)
	}
}

func TestApproveHelpNamesTheOptionValue(t *testing.T) {
	r := run(t, newEnv(t, nil), nil, "approve", "--help")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "--option ID") || strings.Contains(r.stdout, "--option pyre") {
		t.Fatalf("approve --help:\n%s", r.stdout)
	}
}
