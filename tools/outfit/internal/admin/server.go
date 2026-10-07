package admin

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/state"
)

type Server struct {
	State       *state.Store
	Connections connection.Service
	Nonce       string
}

type snapshot struct {
	Packages    []state.InstalledPackage `json:"packages"`
	Connections []safeConnection         `json:"connections"`
}

type safeConnection struct {
	ID, Name, ConnectorKey, SelectedMode, Status string
	SafeIdentity                                 map[string]string
}

func Serve(ctx context.Context, address string, store *state.Store, service connection.Service, ready func(string)) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &Server{State: store, Connections: service, Nonce: randomNonce()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.index)
	mux.HandleFunc("GET /api/snapshot", server.snapshot)
	mux.HandleFunc("POST /api/connections", server.configure)
	mux.HandleFunc("POST /api/connections/{name}/remove", server.remove)
	httpServer := &http.Server{Handler: security(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	ready("http://" + listener.Addr().String() + "/?nonce=" + url.QueryEscape(server.Nonce))
	err = httpServer.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (server *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("nonce") != server.Nonce {
		http.Error(w, "open this page from outfit admin", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]string{"Nonce": server.Nonce})
}

func (server *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if !server.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	packages, err := server.State.Packages(r.Context())
	if err != nil {
		writeError(w, err, 500)
		return
	}
	connections, err := server.State.Connections(r.Context())
	if err != nil {
		writeError(w, err, 500)
		return
	}
	safe := make([]safeConnection, 0, len(connections))
	for _, item := range connections {
		if !server.Connections.Allows(item.Name) {
			continue
		}
		safe = append(safe, safeConnection{ID: item.ID, Name: item.Name, ConnectorKey: item.ConnectorKey, SelectedMode: item.SelectedMode, Status: item.Status, SafeIdentity: item.SafeIdentity})
	}
	writeJSON(w, snapshot{Packages: packages, Connections: safe}, 200)
}

func (server *Server) configure(w http.ResponseWriter, r *http.Request) {
	if !server.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Name, Connector, Mode string
		Fields                map[string]string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, err, 400)
		return
	}
	configured, err := server.Connections.Configure(r.Context(), body.Name, body.Connector, body.Mode, body.Fields)
	if err != nil {
		writeError(w, err, 422)
		return
	}
	configured.CredentialRef = ""
	writeJSON(w, configured, 200)
}

func (server *Server) remove(w http.ResponseWriter, r *http.Request) {
	if !server.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := server.Connections.Remove(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err, 422)
		return
	}
	writeJSON(w, map[string]bool{"removed": true}, 200)
}

func (server *Server) authorized(r *http.Request) bool {
	return r.Header.Get("X-Outfit-Admin") == server.Nonce || r.URL.Query().Get("nonce") == server.Nonce
}
func writeJSON(w http.ResponseWriter, value any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error, status int) {
	writeJSON(w, map[string]string{"error": err.Error()}, status)
}
func randomNonce() string {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

//go:embed admin.html
var assets embed.FS
var page = template.Must(template.ParseFS(assets, "admin.html"))
