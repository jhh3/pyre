package mock

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.API = (*Backend)(nil)

// Features advertised by the mock in Hello.
var Features = []string{"mock", "events.stream", "uploads.resume", "content.range", "long_poll"}

func (b *Backend) Hello(ctx context.Context, req c.HelloRequest) (c.HelloResponse, error) {
	if req.Protocol != 0 && (req.Protocol < c.MinProtocolVersion || req.Protocol > c.ProtocolVersion) {
		return c.HelloResponse{}, c.Errorf(c.ErrProtocolMismatch, "client protocol %d not supported; hub supports %d..%d", req.Protocol, c.MinProtocolVersion, c.ProtocolVersion).
			WithDetail("min_protocol", c.MinProtocolVersion).WithDetail("protocol", c.ProtocolVersion)
	}
	return query(b, ctx, func(p c.Principal) (c.HelloResponse, error) {
		return c.HelloResponse{
			Protocol: c.ProtocolVersion, MinProtocol: c.MinProtocolVersion, HubID: b.opts.HubID,
			HubVersion: "mock-" + buildinfo.Version, ServerTime: b.now(), Principal: p, Mock: true,
			Features: append([]string(nil), Features...),
		}, nil
	})
}

func (b *Backend) WhoAmI(ctx context.Context) (c.Principal, error) {
	return query(b, ctx, func(p c.Principal) (c.Principal, error) { return p, nil })
}

// ---- nodes ----

func (b *Backend) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Node], error) {
		var out []c.Node
		for _, id := range sortedKeys(b.nodes) {
			n := b.nodes[id]
			if n.Connection.State == c.ConnRevoked && !req.IncludeRevoked {
				continue
			}
			out = append(out, *n)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) GetNode(ctx context.Context, req c.GetRequest) (c.Node, error) {
	return query(b, ctx, func(p c.Principal) (c.Node, error) {
		n, ok := b.nodes[req.ID]
		if !ok {
			return c.Node{}, notFound(c.KindNode, req.ID)
		}
		return *n, nil
	})
}

// pairingCodePrefix matches transport.PrefixPairingCode (asserted by
// tests; not imported to keep the mock free of the hub storage stack).
const pairingCodePrefix = "pyp_"

// pairingCode returns a code in the real format (transport.NewSecret with
// the pairing prefix: 32 random bytes, unpadded base64url), so
// enrollment.ParsePairingURI accepts mock tickets.
func pairingCode() string {
	var buf [32]byte
	_, _ = rand.Read(buf[:])
	return pairingCodePrefix + base64.RawURLEncoding.EncodeToString(buf[:])
}

func randomSecret() string {
	var buf [18]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func (b *Backend) CreateEnrollment(ctx context.Context, req c.EnrollmentRequest) (c.EnrollmentTicket, error) {
	return mutate(b, ctx, c.OpCreateEnrollment, req.Mutation, req, func(p c.Principal) (c.EnrollmentTicket, error) {
		if err := requireOwner(p); err != nil {
			return c.EnrollmentTicket{}, err
		}
		if req.Name == "" {
			return c.EnrollmentTicket{}, c.Errorf(c.ErrInvalid, "name is required")
		}
		ttl := time.Duration(req.TTL) * time.Second
		if ttl <= 0 {
			ttl = 15 * time.Minute
		}
		now := b.now()
		n := &c.Node{
			NodeID: c.NewID(c.PrefixNode), Name: req.Name, SSHAlias: req.SSHAlias, Harnesses: []c.HarnessInfo{},
			Versions: map[string]string{}, Capabilities: []string{}, Connection: c.NodeConnection{State: c.ConnNever},
			EnrolledAt: now, Labels: map[string]string{"mock": "true"},
		}
		b.nodes[n.NodeID] = n
		b.emit(c.EvNodeEnrolled, ref(c.KindNode, n.NodeID), "", nil, n)
		return c.EnrollmentTicket{NodeID: n.NodeID, Secret: "pye_mock-enroll-" + randomSecret(), ExpiresAt: now.Add(ttl), HubURL: b.opts.HubURL, HubCertSHA256: b.opts.HubCertSHA256}, nil
	})
}

func (b *Backend) RevokeNode(ctx context.Context, req c.RevokeRequest) (c.Node, error) {
	return mutate(b, ctx, c.OpRevokeNode, req.Mutation, req, func(p c.Principal) (c.Node, error) {
		if err := requireOwner(p); err != nil {
			return c.Node{}, err
		}
		n, ok := b.nodes[req.ID]
		if !ok {
			return c.Node{}, notFound(c.KindNode, req.ID)
		}
		if n.Connection.State != c.ConnRevoked {
			now := b.now()
			n.Connection.State = c.ConnRevoked
			n.Connection.LastSeenAt = &now
			b.markNodeRunsStale(n.NodeID, "node revoked")
			b.emit(c.EvNodeRevoked, ref(c.KindNode, n.NodeID), "", nil, n)
		}
		return *n, nil
	})
}

func (b *Backend) CreatePairing(ctx context.Context, req c.PairingRequest) (c.PairingTicket, error) {
	return mutate(b, ctx, c.OpCreatePairing, req.Mutation, req, func(p c.Principal) (c.PairingTicket, error) {
		if err := requireOwner(p); err != nil {
			return c.PairingTicket{}, err
		}
		switch req.Kind {
		case "cli", "android", "tui":
		default:
			return c.PairingTicket{}, c.Errorf(c.ErrInvalid, "kind must be cli, android or tui")
		}
		ttl := time.Duration(req.TTL) * time.Second
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		t := c.PairingTicket{
			PairingID: c.NewID(c.PrefixPairing), Code: pairingCode(), HubURL: b.opts.HubURL,
			HubCertSHA256: b.opts.HubCertSHA256, ExpiresAt: b.now().Add(ttl),
		}
		q := url.Values{"hub": {t.HubURL}, "id": {t.PairingID}, "code": {t.Code}}
		if t.HubCertSHA256 != "" {
			q.Set("pin", t.HubCertSHA256)
		}
		t.URI = "pyre://pair?" + q.Encode()
		b.pairings[t.PairingID] = &pairing{ticket: t, kind: req.Kind, name: req.ClientName}
		return t, nil
	})
}

// RedeemPairing exchanges a pairing code for a new client record (the
// mock hub's /v1/pair/exchange uses it). One-time; expired codes fail.
func (b *Backend) RedeemPairing(ex c.PairingExchange) (c.ClientInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pr, ok := b.pairings[ex.PairingID]
	if !ok || pr.ticket.Code != ex.Code {
		return c.ClientInfo{}, c.Errorf(c.ErrUnauthorized, "invalid pairing code")
	}
	if pr.redeemed {
		return c.ClientInfo{}, c.Errorf(c.ErrConflict, "pairing code already used")
	}
	if b.now().After(pr.ticket.ExpiresAt) {
		return c.ClientInfo{}, c.Errorf(c.ErrExpired, "pairing code expired")
	}
	pr.redeemed = true
	name := ex.ClientName
	if name == "" {
		name = pr.name
	}
	kind := ex.Kind
	if kind == "" {
		kind = pr.kind
	}
	ci := &c.ClientInfo{ClientID: c.NewID(c.PrefixClient), Name: name, Kind: kind, CreatedAt: b.now()}
	b.clients[ci.ClientID] = ci
	return *ci, nil
}

func (b *Backend) ListClients(ctx context.Context, req c.PageRequest) (c.Page[c.ClientInfo], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.ClientInfo], error) {
		if err := requireOwner(p); err != nil {
			return c.Page[c.ClientInfo]{}, err
		}
		var out []c.ClientInfo
		for _, id := range sortedKeys(b.clients) {
			out = append(out, *b.clients[id])
		}
		return paginate(out, req, b.now())
	})
}

func (b *Backend) RevokeClient(ctx context.Context, req c.RevokeRequest) (c.ClientInfo, error) {
	return mutate(b, ctx, c.OpRevokeClient, req.Mutation, req, func(p c.Principal) (c.ClientInfo, error) {
		if err := requireOwner(p); err != nil {
			return c.ClientInfo{}, err
		}
		ci, ok := b.clients[req.ID]
		if !ok {
			return c.ClientInfo{}, notFound(c.KindClient, req.ID)
		}
		ci.Revoked = true
		return *ci, nil
	})
}

// ---- notification prefs / push ----

func (b *Backend) GetNotificationPrefs(ctx context.Context) (c.NotificationPrefs, error) {
	return query(b, ctx, func(p c.Principal) (c.NotificationPrefs, error) { return b.prefs, nil })
}

func (b *Backend) SetNotificationPrefs(ctx context.Context, req c.SetNotificationPrefsRequest) (c.NotificationPrefs, error) {
	return mutate(b, ctx, c.OpSetNotificationPrefs, req.Mutation, req, func(p c.Principal) (c.NotificationPrefs, error) {
		if err := requireOwner(p); err != nil {
			return c.NotificationPrefs{}, err
		}
		if req.Prefs.TZ != "" {
			if _, err := time.LoadLocation(req.Prefs.TZ); err != nil {
				return c.NotificationPrefs{}, c.Errorf(c.ErrInvalid, "unknown time zone %q", req.Prefs.TZ)
			}
		}
		for _, hm := range []string{req.Prefs.QuietHoursStart, req.Prefs.QuietHoursEnd} {
			if hm == "" {
				continue
			}
			if _, err := time.Parse("15:04", hm); err != nil {
				return c.NotificationPrefs{}, c.Errorf(c.ErrInvalid, "quiet hours must be HH:MM")
			}
		}
		b.prefs = clone(req.Prefs)
		return b.prefs, nil
	})
}

func (b *Backend) RegisterPush(ctx context.Context, req c.PushRegistration) (c.PushRegistrationResult, error) {
	return mutate(b, ctx, c.OpRegisterPush, req.Mutation, req, func(p c.Principal) (c.PushRegistrationResult, error) {
		if err := requireOwner(p); err != nil {
			return c.PushRegistrationResult{}, err
		}
		b.pushRegs = append(b.pushRegs, req)
		switch req.Provider {
		case "fcm":
			return c.PushRegistrationResult{Provider: "fcm", Enabled: false, Detail: "mock backend: FCM not configured; no push is sent"}, nil
		case "hub-stream":
			return c.PushRegistrationResult{Provider: "hub-stream", Enabled: true, Detail: "mock backend: use the event stream"}, nil
		}
		return c.PushRegistrationResult{}, c.Errorf(c.ErrInvalid, "unknown push provider %q", req.Provider)
	})
}
