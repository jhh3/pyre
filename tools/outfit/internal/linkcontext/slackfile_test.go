package linkcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlackFileIDAcceptsEveryURLForm(t *testing.T) {
	for raw, want := range map[string]string{
		"F0CG67ESGRW":                  "F0CG67ESGRW",
		"T08K5RVBYA0-F0CG67ESGRW":      "F0CG67ESGRW",
		"T08K5RVBYA0-F0CG67ESGRW-a1b2": "F0CG67ESGRW",
	} {
		if got, ok := slackFileID(raw); !ok || got != want {
			t.Errorf("slackFileID(%q) = %q, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"", "U08K5RVBYA0", "image.png", "f0cg67esgrw"} {
		if _, ok := slackFileID(raw); ok {
			t.Errorf("slackFileID(%q) accepted", raw)
		}
	}
}

type fakeSlackFile struct {
	id, name, mime string
	body           []byte
	reportedSize   int64
}

func fakeSlack(t *testing.T, files ...fakeSlackFile) *httptest.Server {
	t.Helper()
	byID := map[string]fakeSlackFile{}
	for _, file := range files {
		byID[file.id] = file
	}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer xoxc-test" || request.Header.Get("Cookie") != "d=xoxd-test%2F" {
			response.Header().Set("Content-Type", "text/html")
			_, _ = response.Write([]byte("<html>sign in</html>"))
			return
		}
		if request.URL.Path == "/api/files.info" {
			_ = request.ParseForm()
			file, ok := byID[request.PostForm.Get("file")]
			if request.PostForm.Get("token") != "xoxc-test" || !ok {
				_ = json.NewEncoder(response).Encode(map[string]any{"ok": false, "error": "file_not_found"})
				return
			}
			size := file.reportedSize
			if size == 0 {
				size = int64(len(file.body))
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "file": map[string]any{
				"id": file.id, "name": file.name, "title": file.name, "mimetype": file.mime, "size": size, "user": "U08K5RVBYA0",
				"url_private_download": server.URL + "/files-pri/T1-" + file.id + "/download/" + file.name,
			}})
			return
		}
		for _, file := range files {
			if strings.HasPrefix(request.URL.Path, "/files-pri/T1-"+file.id+"/") {
				response.Header().Set("Content-Type", file.mime)
				_, _ = response.Write(file.body)
				return
			}
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)
	return server
}

func withFakeAgentSlack(t *testing.T, server *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "default_workspace_url": server.URL, "workspaces": []any{map[string]any{
		"workspace_url": server.URL, "auth": map[string]any{"auth_type": "browser", "xoxc_token": "xoxc-test", "xoxd_cookie": "xoxd-test/"},
	}}})
	if err := os.WriteFile(credentials, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	previousPath, previousCache, previousClient, previousKeychain := agentSlackCredentialsPath, fileCacheRoot, slackHTTP, agentSlackKeychain
	agentSlackCredentialsPath = func() string { return credentials }
	fileCacheRoot = func() (string, error) { return cache, nil }
	slackHTTP = server.Client()
	agentSlackKeychain = func(context.Context, string) string { return "" }
	t.Cleanup(func() {
		agentSlackCredentialsPath, fileCacheRoot, slackHTTP, agentSlackKeychain = previousPath, previousCache, previousClient, previousKeychain
	})
	return cache
}

func TestSlackFileLinkDownloadsThroughAgentSlackLogin(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-image")
	server := fakeSlack(t,
		fakeSlackFile{id: "F0CG67ESGRW", name: "image.png", mime: "image/png", body: png},
		fakeSlackFile{id: "F0HUGE00001", name: "huge.mov", mime: "video/quicktime", body: []byte("small"), reportedSize: AutoDownloadLimit + 1},
	)
	cache := withFakeAgentSlack(t, server)
	store := linkTestStore(t)
	installEvalConnection(t, store, &evalVault{values: map[string]map[string]string{}}, "slack", "cli", "native-login", "slack", "", nil, nil)
	service := Service{State: store}
	service.Connections.State = store

	for _, raw := range []string{
		"https://example.slack.com/files/U00000000A0/F0CG67ESGRW/image.png",
		"https://files.slack.com/files-pri/T08K5RVBYA0-F0CG67ESGRW/image.png",
		"https://files.slack.com/files-pri/T08K5RVBYA0-F0CG67ESGRW/download/image.png",
	} {
		resolution := service.ResolveDetailed(context.Background(), "look at "+raw, ResolveOptions{})
		if len(resolution.Diagnostics) != 0 || len(resolution.Resources) != 1 {
			t.Fatalf("%s resolution = %#v", raw, resolution)
		}
		file := resolution.Resources[0].File
		if file == nil || file.Path != filepath.Join(cache, "slack", "F0CG67ESGRW", "image.png") || file.MimeType != "image/png" || file.Size != int64(len(png)) {
			t.Fatalf("%s file = %#v", raw, file)
		}
		got, err := os.ReadFile(file.Path)
		if err != nil || !bytes.Equal(got, png) {
			t.Fatalf("downloaded bytes = %q, %v", got, err)
		}
		if info, _ := os.Stat(file.Path); info.Mode().Perm() != 0o600 {
			t.Fatalf("download mode = %v", info.Mode())
		}
		rendered := Render(resolution.Resources)
		if !strings.Contains(rendered, "local_path: "+file.Path) || !strings.Contains(rendered, "mime_type: image/png") {
			t.Fatalf("rendered context lacks the local path:\n%s", rendered)
		}
	}

	huge := "https://example.slack.com/files/U00000000A0/F0HUGE00001/huge.mov"
	automatic := service.ResolveDetailed(context.Background(), huge, ResolveOptions{})
	if len(automatic.Resources) != 1 || automatic.Resources[0].File.Path != "" || automatic.Resources[0].File.Fetch != "outfit get "+huge {
		t.Fatalf("automatic large file = %#v", automatic)
	}
	if rendered := Render(automatic.Resources); !strings.Contains(rendered, "fetch: outfit get "+huge) {
		t.Fatalf("large file context lacks fetch command:\n%s", rendered)
	}
	explicit := service.ResolveDetailed(context.Background(), huge, ResolveOptions{Explicit: true})
	if len(explicit.Resources) != 1 || explicit.Resources[0].File.Path == "" {
		t.Fatalf("explicit large file = %#v", explicit)
	}
}

func TestSlackFileLinkReportsExpiredLogin(t *testing.T) {
	server := fakeSlack(t, fakeSlackFile{id: "F0CG67ESGRW", name: "image.png", mime: "image/png", body: []byte("x")})
	withFakeAgentSlack(t, server)
	agentSlackKeychain = func(_ context.Context, account string) string {
		if account == "xoxd" {
			return "stale"
		}
		return ""
	}
	store := linkTestStore(t)
	installEvalConnection(t, store, &evalVault{values: map[string]map[string]string{}}, "slack", "cli", "native-login", "slack", "", nil, nil)
	service := Service{State: store}
	service.Connections.State = store
	resolution := service.ResolveDetailed(context.Background(), "https://example.slack.com/files/U00000000A0/F0CG67ESGRW/image.png", ResolveOptions{})
	if len(resolution.Resources) != 0 || len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0].Code != "link_lookup_failed" {
		t.Fatalf("expired login resolution = %#v", resolution)
	}
}

func TestDownloadedAttachmentsListsLocalFiles(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "F0CG67ESGRW.png")
	if err := os.WriteFile(image, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	var data any
	_ = json.Unmarshal([]byte(`{"messages":[{"ts":"1","files":[{"name":"image.png","mimetype":"image/png","path":`+strconvQuote(image)+`},{"name":"gone.png","path":"/nonexistent/gone.png"},{"name":"relative","path":"x.png"}]}]}`), &data)
	files := downloadedAttachments(data)
	if len(files) != 1 || files[0].Path != image || files[0].Name != "image.png" || files[0].Size != 3 {
		t.Fatalf("attachments = %#v", files)
	}
	rendered := Render([]Resource{{Provider: "slack", URL: "https://x.slack.com/archives/C1/p1", Mode: ModeLight, Summary: "hi", Attachments: files}})
	if !strings.Contains(rendered, "attachment: image.png (image/png, 3 bytes) local_path: "+image) {
		t.Fatalf("rendered:\n%s", rendered)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
