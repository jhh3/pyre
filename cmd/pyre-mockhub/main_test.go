package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestMockhubTLSPinAndPairing starts the binary's run loop with TLS, reads
// the printed pin, and exercises Hello, pairing and a session token.
func TestMockhubTLSPinAndPairing(t *testing.T) {
	addr := freeAddr(t)
	pr, pw := io.Pipe()
	go func() {
		_ = run([]string{"--listen", addr, "--token", "test-token", "--tls", "--tick", "50ms", "--quiet"}, pw, io.Discard)
	}()
	sc := bufio.NewScanner(pr)
	pin, url := "", ""
	sawBanner := false
	deadline := time.After(10 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for pin == "" || url == "" {
		select {
		case l := <-lines:
			if strings.Contains(l, "MOCK BACKEND — not runtime evidence") {
				sawBanner = true
			}
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "TLS" {
				pin = strings.TrimPrefix(f[2], "sha256:")
			}
			if f := strings.Fields(l); len(f) >= 3 && f[0] == "hub" {
				url = f[2]
			}
		case <-deadline:
			t.Fatal("banner not printed")
		}
	}
	go func() {
		for range lines {
		}
	}()
	if !sawBanner {
		t.Fatal("missing mock banner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl := client.New(url, "test-token", client.Options{CertSHA256: pin})
	h, err := cl.Hello(ctx, c.HelloRequest{Client: "mockhub-test"})
	if err != nil || !h.Mock {
		t.Fatalf("hello %+v %v", h, err)
	}
	if _, err := client.New(url, "test-token", client.Options{CertSHA256: strings.Repeat("00", 32)}).Hello(ctx, c.HelloRequest{}); !c.IsCode(err, c.ErrUnauthorized) {
		t.Fatalf("wrong pin accepted: %v", err)
	}
	// Session token.
	who, err := client.New(url, "test-token:session:ses_synth-a", client.Options{CertSHA256: pin}).WhoAmI(ctx)
	if err != nil || who.Kind != c.PrincipalSession || who.ID != "ses_synth-a" {
		t.Fatalf("session token %+v %v", who, err)
	}
	// Pairing: create a ticket, exchange it, use the new credential.
	ticket, err := cl.CreatePairing(ctx, c.PairingRequest{ClientName: "test phone", Kind: "android"})
	if err != nil || !strings.Contains(ticket.URI, "pin=") {
		t.Fatalf("pairing %+v %v", ticket, err)
	}
	// Real clients parse the link: https hub URL, pin, pyp_ code.
	link, err := enrollment.ParsePairingURI(ticket.URI)
	if err != nil || link.HubURL != url || !strings.HasPrefix(link.HubURL, "https://") || link.PinSHA256 != pin {
		t.Fatalf("pairing link %q: %+v %v", ticket.URI, link, err)
	}
	cred, err := exchange(ctx, link.HubURL, link.PinSHA256, c.PairingExchange{PairingID: link.PairingID, Code: link.Code, ClientName: "test phone", Kind: "android"})
	if err != nil {
		t.Fatal(err)
	}
	who, err = client.New(url, cred.Token, client.Options{CertSHA256: pin}).WhoAmI(ctx)
	if err != nil || who.ID != cred.ClientID {
		t.Fatalf("paired credential %+v %v", who, err)
	}
	if _, err := exchange(ctx, url, pin, c.PairingExchange{PairingID: ticket.PairingID, Code: ticket.Code}); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("pairing code reused: %v", err)
	}
	// The tick loop advances simulated runs.
	resp, err := cl.StartSession(ctx, c.StartSessionRequest{NodeID: "node_synth-mac", Harness: c.HarnessMock, Cwd: "/tmp/demo"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		sv, err := cl.GetSession(ctx, c.GetRequest{ID: resp.Session.Session.SessionID})
		if err == nil && sv.Run.State == c.RunWaitingInput {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tick loop did not advance the run")
}
