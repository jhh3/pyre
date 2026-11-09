package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The local status says whether pyred holds an owner credential: the CLI
// falls back to this socket from a plain shell only when it does
// (iss_…).
func TestLocalStatusOwnerCredential(t *testing.T) {
	for _, tok := range []string{"", "pyre_owner_test_token_0000000000"} {
		n := startNode(t, shortDir(t), Config{OwnerToken: tok})
		rec := httptest.NewRecorder()
		n.d.localStatusHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathLocalStatus, nil))
		var st LocalStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.OwnerCredential != (tok != "") {
			t.Fatalf("owner token %q: owner_credential %v", tok, st.OwnerCredential)
		}
	}
}
