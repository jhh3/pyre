// Command pyre-mockhub serves the deterministic mock backend over the v1
// HTTP API so the TUI, CLI and Android app can be developed without a real
// hub, nodes, or runtimes. Everything it reports is MOCK data and must never
// be cited as runtime evidence.
//
//	pyre-mockhub --listen 127.0.0.1:7788 --token dev-token [--fixtures dir]... [--tick 1s] [--tls]
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, strings.Split(v, ",")...); return nil }

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pyre-mockhub:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pyre-mockhub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:7788", "listen address")
	token := fs.String("token", "dev-token", "owner bearer token (development only)")
	var fixtures multiFlag
	fs.Var(&fixtures, "fixtures", "fixture directory (repeatable or comma-separated); default tests/fixtures/{contracts,mock}")
	empty := fs.Bool("empty", false, "start with no seeded data")
	tick := fs.Duration("tick", time.Second, "simulation tick interval (0 disables; deliveries then never progress)")
	useTLS := fs.Bool("tls", false, "serve HTTPS with an in-memory self-signed certificate and print its SHA-256 pin")
	certFile := fs.String("tls-cert", "", "PEM certificate (with --tls-key) instead of a generated one")
	keyFile := fs.String("tls-key", "", "PEM private key for --tls-cert")
	clockMode := fs.String("clock", "fixture", `"fixture" (time starts at the fixtures' 2026-10-04T12:00Z so ages read right) or "real"`)
	quiet := fs.Bool("quiet", false, "disable the access log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return errors.New("--token must not be empty")
	}

	var clk c.Clock = c.SystemClock{}
	switch *clockMode {
	case "fixture":
		clk = mock.NewOffsetClock(mock.FixtureEpoch)
	case "real":
	default:
		return fmt.Errorf("--clock must be fixture or real")
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	scheme := "http"
	var tlsCfg *tls.Config
	pin := ""
	if *useTLS || *certFile != "" {
		scheme = "https"
		var cert tls.Certificate
		if *certFile != "" {
			cert, err = tls.LoadX509KeyPair(*certFile, *keyFile)
		} else {
			cert, err = selfSigned(hostOf(addr))
		}
		if err != nil {
			ln.Close()
			return fmt.Errorf("tls: %w", err)
		}
		pin = client.CertFingerprint(cert.Certificate[0])
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	hubURL := scheme + "://" + addr

	opts := mock.Options{Clock: clk, Empty: *empty, HubID: "hub_mock", HubURL: hubURL, HubCertSHA256: pin}
	if len(fixtures) > 0 {
		opts.FixtureDirs = fixtures
	}
	b, err := mock.NewE(opts)
	if err != nil {
		ln.Close()
		return fmt.Errorf("load fixtures: %w", err)
	}

	owner := c.Principal{Kind: c.PrincipalOwner, ID: "cli_mock-owner", Name: "owner (mock)"}
	auth := httpapi.NewStaticTokens(map[string]c.Principal{*token: owner})
	// "<token>:session:<session_id>" authenticates as that agent session.
	auth.Fallback = func(t string) (c.Principal, bool) {
		prefix := *token + ":session:"
		if strings.HasPrefix(t, prefix) && c.ValidID(t[len(prefix):], c.PrefixSession) {
			return c.Principal{Kind: c.PrincipalSession, ID: t[len(prefix):]}, true
		}
		return c.Principal{}, false
	}

	var logger *slog.Logger
	if !*quiet {
		logger = slog.New(slog.NewTextHandler(stderr, nil))
	}
	handler := httpapi.NewServer(b, auth, httpapi.Options{
		Logger: logger,
		Extra: map[string]http.Handler{
			"POST " + c.PathPairExchange:   pairExchange(b, auth),
			"POST " + c.PathEnrollExchange: unsupported("mock hub: node enrollment needs the real hub (WP04)"),
			c.PathTerminal:                 unsupported("mock hub: no live terminals; use sessions.read for transcript/screen"),
			"GET " + c.PathPreview:         preview(b),
		},
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *tick > 0 {
		go b.Run(ctx, *tick)
	}
	banner(stdout, hubURL, *token, pin, *tick, *clockMode, ln.Addr())

	errCh := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			srv.TLSConfig = tlsCfg
			errCh <- srv.ServeTLS(ln, "", "")
		} else {
			errCh <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	fmt.Fprintln(stdout, "pyre-mockhub: stopped")
	return nil
}

func banner(w io.Writer, hubURL, token, pin string, tick time.Duration, clock string, addr net.Addr) {
	line := strings.Repeat("=", 72)
	fmt.Fprintln(w, line)
	fmt.Fprintln(w, "  MOCK BACKEND — not runtime evidence")
	fmt.Fprintln(w, "  Deterministic synthetic data. Nothing here proves a real runtime,")
	fmt.Fprintln(w, "  node, account, or delivery works. Hello.mock=true on every response.")
	fmt.Fprintln(w, line)
	fmt.Fprintf(w, "  hub URL     %s\n", hubURL)
	fmt.Fprintf(w, "  owner token %s   (Authorization: Bearer %s)\n", token, token)
	fmt.Fprintf(w, "  session     Bearer %s:session:<session_id>\n", token)
	if pin != "" {
		fmt.Fprintf(w, "  TLS pin     sha256:%s\n", pin)
	}
	fmt.Fprintf(w, "  tick        %s   clock %s\n", tick, clock)
	fmt.Fprintln(w, "  markers     [mock:indeterminate] / [mock:reject] in a message body")
	if tcp, ok := addr.(*net.TCPAddr); ok && !tcp.IP.IsLoopback() && pin == "" {
		fmt.Fprintln(w, "  WARNING     listening beyond loopback without TLS; the token is sent in clear text")
	}
	fmt.Fprintln(w, line)
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

// selfSigned returns an in-memory ECDSA certificate valid for localhost,
// loopback, the listen host, and this machine's addresses.
func selfSigned(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pyre-mockhub (synthetic)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" && ip == nil {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
				tmpl.IPAddresses = append(tmpl.IPAddresses, n.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func unsupported(msg string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteError(w, c.Errorf(c.ErrUnsupported, "%s", msg))
	})
}

// pairExchange redeems a pairing code (from clients.pair) and mints a new
// owner token for the paired client.
func pairExchange(b *mock.Backend, auth *httpapi.StaticTokens) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			httpapi.WriteError(w, c.Errorf(c.ErrInvalid, "read body"))
			return
		}
		var ex c.PairingExchange
		if err := httpapi.DecodeStrict(body, &ex); err != nil {
			httpapi.WriteError(w, err)
			return
		}
		ci, err := b.RedeemPairing(ex)
		if err != nil {
			httpapi.WriteError(w, err)
			return
		}
		var buf [24]byte
		_, _ = rand.Read(buf[:])
		tok := fmt.Sprintf("mock-%x", buf)
		auth.Add(tok, c.Principal{Kind: c.PrincipalOwner, ID: ci.ClientID, Name: ci.Name})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(c.ClientCredential{ClientID: ci.ClientID, Token: tok})
	})
}

// preview serves active content sandboxed. The mock does not validate the
// preview token (there is no ticket issuer); it only demonstrates headers.
func preview(b *mock.Backend) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("version_id")
		ctx := c.WithPrincipal(r.Context(), c.Principal{Kind: c.PrincipalSystem, ID: "preview"})
		rc, ver, err := b.OpenContent(ctx, id)
		if err != nil {
			httpapi.WriteError(w, err)
			return
		}
		defer rc.Close()
		h := w.Header()
		h.Set("Content-Type", httpapi.SafeMediaType(ver.MediaType))
		h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, rc)
	})
}
