package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Cross-node provisioning (explicit, owner-initiated):
//
//  1. The recipient node creates a ProvisionOffer: a fresh X25519 key pair
//     whose private half stays in the recipient's memory only, bound to the
//     connection, variable name, destination Ref, and an expiry.
//  2. The sending node (which holds the value, e.g. from its own store or an
//     owner prompt) Seals the value to the offer. Only the SealedCredential
//     (ciphertext) crosses the hub; the hub may persist it durably as part of
//     a node command without being able to read it.
//  3. The recipient Accepts once: it opens the ciphertext, writes the value to
//     its local store, records a Provision (names only), and destroys the
//     private key. Replays, expired offers, and redirected ciphertexts fail.
//
// The protocol never puts a value in a node command payload, report, event,
// log, or hub table.

// ProvisionOffer is safe to transmit and persist.
type ProvisionOffer struct {
	OfferID       string    `json:"offer_id"`
	RecipientNode string    `json:"recipient_node"`
	ConnectionID  string    `json:"connection_id"`
	EnvVar        string    `json:"env_var"`
	Ref           string    `json:"ref"`
	PublicKey     []byte    `json:"public_key"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// SealedCredential is ciphertext bound to one offer.
type SealedCredential struct {
	OfferID      string `json:"offer_id"`
	SenderNode   string `json:"sender_node"`
	EphemeralKey []byte `json:"ephemeral_key"`
	Nonce        []byte `json:"nonce"`
	Ciphertext   []byte `json:"ciphertext"`
}

func offerAAD(o ProvisionOffer, sender string) []byte {
	b, _ := json.Marshal(struct {
		O ProvisionOffer `json:"o"`
		S string         `json:"s"`
	}{o, sender})
	return b
}

func deriveKey(shared, ephPub, recipientPub []byte, offerID string) ([]byte, error) {
	salt := append(append([]byte(nil), ephPub...), recipientPub...)
	return hkdf.Key(sha256.New, shared, salt, "pyre/credential-provision/v1/"+offerID, 32)
}

// Seal encrypts value for offer. now is checked against the offer expiry.
func Seal(offer ProvisionOffer, senderNode string, value Secret, now time.Time) (SealedCredential, error) {
	if value.IsZero() {
		return SealedCredential{}, fmt.Errorf("credentials: refusing to provision an empty value")
	}
	if !now.Before(offer.ExpiresAt) {
		return SealedCredential{}, contracts.Errorf(contracts.ErrExpired, "provisioning offer %s expired", offer.OfferID)
	}
	recipient, err := ecdh.X25519().NewPublicKey(offer.PublicKey)
	if err != nil {
		return SealedCredential{}, fmt.Errorf("credentials: offer public key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return SealedCredential{}, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return SealedCredential{}, err
	}
	key, err := deriveKey(shared, eph.PublicKey().Bytes(), offer.PublicKey, offer.OfferID)
	if err != nil {
		return SealedCredential{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return SealedCredential{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return SealedCredential{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return SealedCredential{}, err
	}
	ct := gcm.Seal(nil, nonce, value.v, offerAAD(offer, senderNode))
	return SealedCredential{OfferID: offer.OfferID, SenderNode: senderNode, EphemeralKey: eph.PublicKey().Bytes(), Nonce: nonce, Ciphertext: ct}, nil
}

// Recipient holds pending offers (private keys in memory only).
type Recipient struct {
	NodeID string
	Clock  contracts.Clock
	mu     sync.Mutex
	offers map[string]pendingOffer
}

type pendingOffer struct {
	offer ProvisionOffer
	key   *ecdh.PrivateKey
}

// NewRecipient creates a recipient for nodeID.
func NewRecipient(nodeID string, clock contracts.Clock) *Recipient {
	if clock == nil {
		clock = contracts.SystemClock{}
	}
	return &Recipient{NodeID: nodeID, Clock: clock, offers: map[string]pendingOffer{}}
}

// NewOffer prepares to receive a credential for connectionID under envVar,
// stored at ref, valid for ttl (capped at 15 minutes).
func (r *Recipient) NewOffer(connectionID, envVar string, ref Ref, ttl time.Duration) (ProvisionOffer, error) {
	if err := ValidateEnvName(envVar); err != nil {
		return ProvisionOffer{}, err
	}
	if err := ref.Validate(); err != nil {
		return ProvisionOffer{}, err
	}
	if ttl <= 0 || ttl > 15*time.Minute {
		ttl = 15 * time.Minute
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return ProvisionOffer{}, err
	}
	o := ProvisionOffer{OfferID: contracts.NewID("prov"), RecipientNode: r.NodeID, ConnectionID: connectionID, EnvVar: envVar, Ref: ref.String(), PublicKey: key.PublicKey().Bytes(), ExpiresAt: r.Clock.Now().Add(ttl).UTC()}
	r.mu.Lock()
	r.offers[o.OfferID] = pendingOffer{offer: o, key: key}
	r.mu.Unlock()
	return o, nil
}

// Accept opens sealed exactly once, stores the value at the offer's Ref in
// stores, and records the provision in ix. The offer is consumed even when
// opening fails, so a tampered or replayed ciphertext cannot be retried.
func (r *Recipient) Accept(ctx context.Context, sealed SealedCredential, stores Stores, ix *Index) (Provision, error) {
	r.mu.Lock()
	p, ok := r.offers[sealed.OfferID]
	delete(r.offers, sealed.OfferID)
	r.mu.Unlock()
	if !ok {
		return Provision{}, contracts.Errorf(contracts.ErrNotFound, "no pending provisioning offer %s on this node (offers are single-use)", sealed.OfferID)
	}
	if !r.Clock.Now().Before(p.offer.ExpiresAt) {
		return Provision{}, contracts.Errorf(contracts.ErrExpired, "provisioning offer %s expired", sealed.OfferID)
	}
	ephPub, err := ecdh.X25519().NewPublicKey(sealed.EphemeralKey)
	if err != nil {
		return Provision{}, contracts.Errorf(contracts.ErrInvalid, "sealed credential has an invalid ephemeral key")
	}
	shared, err := p.key.ECDH(ephPub)
	if err != nil {
		return Provision{}, contracts.Errorf(contracts.ErrInvalid, "sealed credential key agreement failed")
	}
	key, err := deriveKey(shared, sealed.EphemeralKey, p.offer.PublicKey, p.offer.OfferID)
	if err != nil {
		return Provision{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Provision{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Provision{}, err
	}
	plain, err := gcm.Open(nil, sealed.Nonce, sealed.Ciphertext, offerAAD(p.offer, sealed.SenderNode))
	if err != nil {
		return Provision{}, contracts.Errorf(contracts.ErrInvalid, "sealed credential failed authentication (wrong offer, recipient, or tampered ciphertext)")
	}
	value := Secret{v: plain}
	defer value.Wipe()
	ref, err := ParseRef(p.offer.Ref)
	if err != nil {
		return Provision{}, err
	}
	st, err := stores.For(ref)
	if err != nil {
		return Provision{}, err
	}
	if err := st.Put(ctx, ref, value); err != nil {
		return Provision{}, err
	}
	prov := Provision{ConnectionID: p.offer.ConnectionID, EnvVar: p.offer.EnvVar, Ref: p.offer.Ref, SourceNode: sealed.SenderNode, ProvisionedAt: r.Clock.Now().UTC()}
	if ix != nil {
		if err := ix.Upsert(prov); err != nil {
			return Provision{}, err
		}
	}
	return prov, nil
}

// Pending reports the number of outstanding offers.
func (r *Recipient) Pending() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.offers) }
