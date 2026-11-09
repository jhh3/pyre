package linkcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// AutoDownloadLimit bounds files fetched automatically for pasted links in
// prompt context. Explicit `outfit get` downloads have no size limit.
const AutoDownloadLimit int64 = 50 << 20

// File describes a provider file downloaded to this machine's private cache.
type File struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size"`
	// Path is the absolute local path; empty when the file was not downloaded.
	Path string `json:"path,omitempty"`
	// Skipped explains why an automatic download did not happen.
	Skipped string `json:"skipped,omitempty"`
	// Fetch is the one-step command that downloads it explicitly.
	Fetch     string `json:"fetch,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

var slackFileIDPattern = regexp.MustCompile(`(?:^|-)(F[A-Z0-9]{6,})(?:-|$)`)

// slackFileID accepts F… ids and Slack's files-pri / files-tmb compound
// segments such as T0123-F0456 or T0123-F0456-abcdef.
func slackFileID(value string) (string, bool) {
	match := slackFileIDPattern.FindStringSubmatch(value)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// slackAuth is one Slack credential: a standard token, or a browser session
// token plus its `d` cookie.
type slackAuth struct {
	workspace string
	token     string
	cookie    string
}

func (auth slackAuth) apply(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+auth.token)
	if auth.cookie != "" {
		request.Header.Set("Cookie", "d="+url.QueryEscape(auth.cookie))
		request.Header.Set("Origin", "https://app.slack.com")
		request.Header.Set("Referer", "https://app.slack.com/")
	}
}

func (auth slackAuth) apiBase() string {
	if auth.cookie != "" && auth.workspace != "" {
		return strings.TrimRight(auth.workspace, "/") + "/api/"
	}
	return "https://slack.com/api/"
}

var slackHTTP = &http.Client{Timeout: 10 * time.Minute}

// agentSlackCredentialsPath is overridden in tests.
var agentSlackCredentialsPath = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "agent-slack", "credentials.json")
}

// agentSlackKeychain reads agent-slack's macOS keychain entries.
var agentSlackKeychain = func(ctx context.Context, account string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", "agent-slack", "-a", account, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// agentSlackAuths loads the workspaces from the agent-slack login that the
// Slack CLI mode already uses, preferring the workspace whose host matches
// the link, then the default workspace.
func agentSlackAuths(ctx context.Context, host string) ([]slackAuth, error) {
	raw, err := os.ReadFile(agentSlackCredentialsPath())
	if err != nil {
		return nil, fmt.Errorf("agent-slack login not found; run `agent-slack auth import-desktop`: %w", err)
	}
	var stored struct {
		Default    string `json:"default_workspace_url"`
		Workspaces []struct {
			URL  string `json:"workspace_url"`
			Auth struct {
				Type   string `json:"auth_type"`
				Token  string `json:"token"`
				XOXC   string `json:"xoxc_token"`
				Cookie string `json:"xoxd_cookie"`
			} `json:"auth"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.New("agent-slack credentials file is not valid JSON")
	}
	var preferred, defaults, rest []slackAuth
	for _, workspace := range stored.Workspaces {
		parsed, _ := url.Parse(workspace.URL)
		normalized := workspace.URL
		if parsed != nil && parsed.Host != "" {
			normalized = parsed.Scheme + "://" + parsed.Host
		}
		auth := slackAuth{workspace: normalized}
		switch workspace.Auth.Type {
		case "browser":
			auth.token, auth.cookie = workspace.Auth.XOXC, workspace.Auth.Cookie
			if value := agentSlackKeychain(ctx, "xoxc:"+normalized); value != "" {
				auth.token = value
			}
			if value := agentSlackKeychain(ctx, "xoxd"); value != "" {
				auth.cookie = value
			}
		default:
			auth.token = workspace.Auth.Token
			if value := agentSlackKeychain(ctx, "token:"+normalized); value != "" {
				auth.token = value
			}
		}
		if auth.token == "" || auth.token == "__KEYCHAIN__" || auth.cookie == "__KEYCHAIN__" {
			continue
		}
		switch {
		case parsed != nil && strings.EqualFold(parsed.Hostname(), host):
			preferred = append(preferred, auth)
		case workspace.URL == stored.Default:
			defaults = append(defaults, auth)
		default:
			rest = append(rest, auth)
		}
	}
	all := append(append(preferred, defaults...), rest...)
	if len(all) == 0 {
		return nil, errors.New("agent-slack has no usable workspace login; run `agent-slack auth import-desktop`")
	}
	return all, nil
}

func (service Service) slackAuths(ctx context.Context, item candidate, host string) ([]slackAuth, error) {
	if item.connection.CredentialMethod == "native-login" || item.lookup.Mode == "cli" {
		return agentSlackAuths(ctx, host)
	}
	fields, err := service.Connections.CredentialFields(ctx, item.connection.Name)
	if err != nil {
		return nil, err
	}
	token := firstNonempty(fields["TOKEN"], fields["OAUTH_ACCESS_TOKEN"])
	if token == "" {
		return nil, errors.New("the Slack connection has no stored token for file downloads")
	}
	return []slackAuth{{token: token}}, nil
}

type slackFileInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	MimeType    string `json:"mimetype"`
	FileType    string `json:"filetype"`
	Size        int64  `json:"size"`
	User        string `json:"user"`
	Created     int64  `json:"created"`
	Permalink   string `json:"permalink"`
	URLPrivate  string `json:"url_private"`
	URLDownload string `json:"url_private_download"`
	Mode        string `json:"mode"`
}

func slackFilesInfo(ctx context.Context, auth slackAuth, fileID string) (slackFileInfo, error) {
	form := url.Values{"file": {fileID}}
	if auth.cookie != "" {
		form.Set("token", auth.token)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.apiBase()+"files.info", strings.NewReader(form.Encode()))
	if err != nil {
		return slackFileInfo{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	auth.apply(request)
	response, err := slackHTTP.Do(request)
	if err != nil {
		return slackFileInfo{}, fmt.Errorf("slack files.info: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		OK    bool          `json:"ok"`
		Error string        `json:"error"`
		File  slackFileInfo `json:"file"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&body); err != nil {
		return slackFileInfo{}, fmt.Errorf("slack files.info returned HTTP %d", response.StatusCode)
	}
	if !body.OK {
		return slackFileInfo{}, fmt.Errorf("slack files.info: %s", body.Error)
	}
	return body.File, nil
}

// fileCacheRoot is the private cache for downloaded link files; overridden in tests.
var fileCacheRoot = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "outfit", "files"), nil
}

var unsafeFileName = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

func cachedFilePath(provider, id, name string) (string, error) {
	root, err := fileCacheRoot()
	if err != nil {
		return "", err
	}
	name = strings.Trim(unsafeFileName.ReplaceAllString(filepath.Base(name), "_"), ". ")
	if name == "" {
		name = id
	}
	if len(name) > 120 {
		extension := filepath.Ext(name)
		if len(extension) > 16 {
			extension = ""
		}
		name = name[:120-len(extension)] + extension
	}
	return filepath.Join(root, provider, id, name), nil
}

func downloadTo(ctx context.Context, auth slackAuth, source, destination, expectedMime string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", 0, err
	}
	auth.apply(request)
	response, err := slackHTTP.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("download: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	contentType := response.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") && !strings.Contains(expectedMime, "html") {
		return "", 0, errors.New("download returned a sign-in page instead of the file; the Slack login may have expired")
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return "", 0, err
	}
	written, copyErr := io.Copy(temporary, response.Body)
	closeErr := temporary.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil {
		copyErr = os.Chmod(temporary.Name(), 0o600)
	}
	if copyErr == nil {
		copyErr = os.Rename(temporary.Name(), destination)
	}
	if copyErr != nil {
		_ = os.Remove(temporary.Name())
		return "", 0, copyErr
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return mediaType, written, nil
}

// resolveSlackFile looks the file up with files.info and downloads it into
// the private cache. Automatic (hook) resolution skips files over
// AutoDownloadLimit and reports the explicit command instead.
func (service Service) resolveSlackFile(ctx context.Context, item candidate, explicit bool) (Resource, error) {
	fileID := item.arguments["file"]
	parsed, _ := url.Parse(item.url)
	host := ""
	if parsed != nil {
		host = parsed.Hostname()
	}
	auths, err := service.slackAuths(ctx, item, host)
	if err != nil {
		return Resource{}, err
	}
	var info slackFileInfo
	var auth slackAuth
	var lookupErr error
	for _, candidate := range auths {
		info, lookupErr = slackFilesInfo(ctx, candidate, fileID)
		if lookupErr == nil {
			auth = candidate
			break
		}
	}
	if lookupErr != nil {
		return Resource{}, lookupErr
	}
	name := firstNonempty(info.Name, info.Title, fileID)
	file := File{ID: firstNonempty(info.ID, fileID), Name: name, MimeType: info.MimeType, Size: info.Size, Permalink: info.Permalink, Fetch: "outfit get " + item.url}
	resource := Resource{CanonicalID: file.ID, Title: firstNonempty(info.Title, name), Author: info.User, File: &file}
	if info.Created > 0 {
		resource.UpdatedAt = time.Unix(info.Created, 0).UTC().Format(time.RFC3339)
	}
	source := firstNonempty(info.URLDownload, info.URLPrivate)
	destination, err := cachedFilePath("slack", file.ID, name)
	if err != nil {
		return Resource{}, err
	}
	switch {
	case source == "":
		file.Skipped = "Slack returned no download URL for this file (mode " + firstNonempty(info.Mode, "unknown") + ")"
	case !explicit && info.Size > AutoDownloadLimit:
		file.Skipped = fmt.Sprintf("larger than the %d MB automatic download limit", AutoDownloadLimit>>20)
	default:
		if stat, statErr := os.Stat(destination); statErr == nil && stat.Mode().IsRegular() && (info.Size == 0 || stat.Size() == info.Size) {
			file.Path, file.Size = destination, stat.Size()
			break
		}
		mediaType, written, downloadErr := downloadTo(ctx, auth, source, destination, info.MimeType)
		if downloadErr != nil {
			return Resource{}, downloadErr
		}
		file.Path, file.Size = destination, written
		if file.MimeType == "" {
			file.MimeType = mediaType
		}
	}
	resource.Summary = describeFile(file)
	return resource, nil
}

func describeFile(file File) string {
	parts := []string{firstNonempty(file.Name, file.ID)}
	if file.MimeType != "" {
		parts = append(parts, file.MimeType)
	}
	parts = append(parts, humanBytes(file.Size))
	summary := strings.Join(parts, ", ")
	if file.Path != "" {
		return summary + "; downloaded to " + file.Path
	}
	return summary + "; not downloaded (" + file.Skipped + "); fetch with `" + file.Fetch + "`"
}

func humanBytes(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

const maxAttachments = 20

// downloadedAttachments collects files that a connector read already saved
// locally (agent-slack downloads message and thread attachments and reports
// files[].path), so prompt context can list them even when content is clipped.
func downloadedAttachments(data any) []File {
	var result []File
	seen := map[string]bool{}
	var walk func(value any, depth int)
	walk = func(value any, depth int) {
		if depth > 8 || len(result) >= maxAttachments {
			return
		}
		switch typed := value.(type) {
		case map[string]any:
			if files, ok := typed["files"].([]any); ok {
				for _, entry := range files {
					object, _ := entry.(map[string]any)
					path, _ := object["path"].(string)
					if path == "" || !filepath.IsAbs(path) || seen[path] || len(result) >= maxAttachments {
						continue
					}
					stat, err := os.Stat(path)
					if err != nil || !stat.Mode().IsRegular() {
						continue
					}
					seen[path] = true
					id, _ := object["id"].(string)
					name, _ := object["name"].(string)
					title, _ := object["title"].(string)
					mimeType, _ := object["mimetype"].(string)
					result = append(result, File{ID: id, Name: firstNonempty(name, title, filepath.Base(path)), MimeType: mimeType, Size: stat.Size(), Path: path})
				}
			}
			for key, child := range typed {
				if key != "files" {
					walk(child, depth+1)
				}
			}
		case []any:
			for _, child := range typed {
				walk(child, depth+1)
			}
		}
	}
	walk(data, 0)
	return result
}
