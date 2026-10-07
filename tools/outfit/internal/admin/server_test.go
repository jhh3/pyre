package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

func TestSnapshotNeverReturnsCredentialReference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded := kit.Loaded{Package: kit.Package{Key: "sample-api", Version: "1"}, Raw: []byte(`{"key":"sample-api","version":"1"}`)}
	if err := store.InstallPackage(ctx, loaded, "trusted"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(ctx, state.Connection{ID: "con_1", Name: "sample", ConnectorKey: "sample-api", SelectedMode: "rest", Status: "active", CredentialID: "cred_1", CredentialMethod: "managed-token", CredentialRef: "outfitsec_do_not_return", SafeIdentity: map[string]string{"account": "safe"}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{State: store, Nonce: "test"}
	request := httptest.NewRequest("GET", "/api/snapshot", nil)
	request.Header.Set("X-Outfit-Admin", "test")
	response := httptest.NewRecorder()
	server.snapshot(response, request)
	if response.Code != 200 || strings.Contains(response.Body.String(), "outfitsec_") {
		t.Fatalf("unsafe snapshot: %d %s", response.Code, response.Body.String())
	}
}
