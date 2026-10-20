// Package discovery publishes and consumes BitFS hash demands on the
// SatSubscription hash-request channel.
//
// A buyer publishes a signed public Hash request carrying its connection
// locators; a seller hears verified demands, checks whether it has the content,
// and then establishes the BitFS transport to the buyer. Nothing here signs a
// quote: the quote travels over the transport session.
package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	channels "github.com/bsv8/ChannelProtocol"
	"github.com/bsv8/ChannelProtocol/hashrequest"
)

// HashRequestChannel is the exact supplier channel for public Hash demands.
const HashRequestChannel = channels.HashRequestChannel

// MaxDemandLifetime is the protocol's longest accepted Hash request lifetime.
const MaxDemandLifetime = 10 * time.Minute

// Demand is one verified public Hash request.
type Demand struct {
	SeedHashHex   string
	FromPublicKey string
	MessageID     string
	ExpiresAt     time.Time
	Locators      []hashrequest.Locator
}

// Publisher signs and publishes the buyer's Hash demands.
type Publisher struct {
	// Publish sends exact JSON bytes to one supplier channel.
	Publish func(ctx context.Context, channel string, contentJSON []byte) error
	// PrivateKey is the buyer's channel identity.
	PrivateKey channels.PrivateKey
	// PublicKey must match PrivateKey.
	PublicKey channels.PublicKey
	// Locators are advertised in preference order (at least one).
	Locators []hashrequest.Locator
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
}

// PublishDemand publishes one signed Hash request and returns it.
func (p *Publisher) PublishDemand(ctx context.Context, seedHashHex string, lifetime time.Duration) (*Demand, error) {
	if p.Publish == nil {
		return nil, errors.New("discovery: publish port is required")
	}
	if len(p.Locators) == 0 {
		return nil, errors.New("discovery: at least one locator is required")
	}
	hash, err := channels.ParseSHA256Hash(seedHashHex)
	if err != nil {
		return nil, fmt.Errorf("discovery: invalid seed hash: %w", err)
	}
	messageID, err := channels.NewMessageID(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("discovery: message id: %w", err)
	}
	if lifetime <= 0 || lifetime > MaxDemandLifetime {
		lifetime = MaxDemandLifetime
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	signed, err := hashrequest.Sign(hashrequest.UnsignedMessage{
		FromPublicKey: p.PublicKey,
		MessageID:     messageID,
		IssuedAtMs:    now.UnixMilli(),
		ExpiresAtMs:   now.Add(lifetime).UnixMilli(),
		Body: hashrequest.HashRequestBody{
			Hash:     hash,
			Locators: append([]hashrequest.Locator(nil), p.Locators...),
		},
	}, p.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("discovery: sign hash request: %w", err)
	}
	encoded, err := hashrequest.Marshal(signed)
	if err != nil {
		return nil, fmt.Errorf("discovery: encode hash request: %w", err)
	}
	if err := p.Publish(ctx, HashRequestChannel, encoded); err != nil {
		return nil, fmt.Errorf("discovery: publish hash request: %w", err)
	}
	return &Demand{
		SeedHashHex:   seedHashHex,
		FromPublicKey: hex.EncodeToString(p.PublicKey.Bytes()),
		MessageID:     messageID.String(),
		ExpiresAt:     now.Add(lifetime),
		Locators:      append([]hashrequest.Locator(nil), p.Locators...),
	}, nil
}

// Listener consumes verified demands for the seller.
type Listener struct {
	// Subscribe subscribes the seller identity to the channel.
	Subscribe func(ctx context.Context, channel string) error
	// Deliveries adds the supplier delivery callback (multiple subscribers
	// share one stream).
	Deliveries func(handler func(channel string, contentJSON []byte))
	// OnDemand is invoked for every fresh, verified demand.
	OnDemand func(Demand)
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]bool
}

// Start subscribes and installs the delivery handler.
func (l *Listener) Start(ctx context.Context) error {
	if l.Subscribe == nil || l.Deliveries == nil || l.OnDemand == nil {
		return errors.New("discovery: subscribe, deliveries and OnDemand are required")
	}
	l.mu.Lock()
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.mu.Unlock()
	if err := l.Subscribe(ctx, HashRequestChannel); err != nil {
		return err
	}
	l.Deliveries(func(channel string, contentJSON []byte) {
		_ = l.Handle(channel, contentJSON)
	})
	return nil
}

// Handle verifies and dispatches one delivery frame.
func (l *Listener) Handle(channel string, contentJSON []byte) error {
	if channel != HashRequestChannel {
		return nil
	}
	message, err := hashrequest.ParseAndVerify(HashRequestChannel, contentJSON)
	if err != nil {
		return fmt.Errorf("discovery: invalid hash request: %w", err)
	}
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	if message.ExpiresAtMs() <= now.UnixMilli() {
		return nil
	}
	key := hex.EncodeToString(message.FromPublicKey().Bytes()) + ":" + message.MessageID().String()
	l.mu.Lock()
	if l.seen[key] {
		l.mu.Unlock()
		return nil
	}
	l.seen[key] = true
	l.mu.Unlock()
	body := message.Body()
	demand := Demand{
		SeedHashHex:   body.Hash.String(),
		FromPublicKey: hex.EncodeToString(message.FromPublicKey().Bytes()),
		MessageID:     message.MessageID().String(),
		ExpiresAt:     time.UnixMilli(message.ExpiresAtMs()),
		Locators:      append([]hashrequest.Locator(nil), body.Locators...),
	}
	l.OnDemand(demand)
	return nil
}
