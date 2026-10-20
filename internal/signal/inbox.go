// Package signal bridges the WebRTC interconnect to the keymaster inbox
// signaling (bsv8.webrtc.signal.v1 private messages).
//
// The browser buyer never accepts direct dials: it publishes a hash request
// with a webrtc-sdp locator and expects the seller to send the SDP offer to
// its inbox. This adapter performs that exchange and converts between the
// ChannelProtocol signal body and the bitcoin-libp2p interconnect envelope,
// using the ChannelProtocol session id as the interconnect connection id (the
// browser routes signals by exactly that name).
package signal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	channels "github.com/bsv8/ChannelProtocol"
	"github.com/bsv8/ChannelProtocol/inbox"
	"github.com/bsv8/ChannelProtocol/webrtcsignal"
	"github.com/bsv8/bitcoin-libp2p/identity"
	"github.com/bsv8/bitcoin-libp2p/webrtcinterconnect"

	"github.com/bsv8/keymaster.cc-cli/internal/ssp"
)

// SignalProtocol is the inbox sub-protocol for WebRTC signals.
const SignalProtocol = channels.WebRTCSignalProtocol

// signalLifetime is the protocol's own max private-message lifetime.
func signalLifetime() time.Duration {
	return time.Duration(inbox.PrivateMessageMaxLifetimeMs(SignalProtocol)) * time.Millisecond
}

// Inbox is one seller-side signaling adapter.
type Inbox struct {
	Client     *ssp.Client
	PrivateKey channels.PrivateKey
	PublicKey  channels.PublicKey
	Now        func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	listener func(webrtcinterconnect.Envelope)
}

type session struct {
	sessionID        string
	attemptID        string
	requestMessageID channels.MessageID
	buyerPublicKey   channels.PublicKey
}

// NewInbox creates the adapter for the seller identity.
func NewInbox(client *ssp.Client, privateKey channels.PrivateKey) (*Inbox, error) {
	if client == nil {
		return nil, errors.New("signal: ssp client is required")
	}
	publicKey, err := channels.PublicKeyFromPrivate(privateKey)
	if err != nil {
		return nil, fmt.Errorf("signal: public key: %w", err)
	}
	return &Inbox{
		Client:     client,
		PrivateKey: privateKey,
		PublicKey:  publicKey,
		sessions:   map[string]*session{},
	}, nil
}

// Channel returns the seller's own inbox channel.
func (i *Inbox) Channel() string { return channels.InboxChannel(i.PublicKey) }

// Start subscribes to the seller inbox and installs the delivery handler.
func (i *Inbox) Start(ctx context.Context) error {
	if _, err := i.Client.Subscribe(ctx, i.Channel()); err != nil {
		return fmt.Errorf("signal: subscribe inbox: %w", err)
	}
	i.Client.AddPublishHandler(func(channel string, contentJSON []byte) {
		if channel != i.Channel() {
			return
		}
		_ = i.Handle(contentJSON)
	})
	return nil
}

// OpenSession registers the buyer relation for one interconnect session. The
// connection id must be a ChannelProtocol session id.
func (i *Inbox) OpenSession(connectionID, attemptID string, requestMessageID channels.MessageID, buyerPublicKey channels.PublicKey) error {
	if _, err := channels.ParseSessionID(connectionID); err != nil {
		return fmt.Errorf("signal: connection id must be a session id: %w", err)
	}
	if attemptID == "" {
		return errors.New("signal: attempt id is required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.sessions[connectionID]; exists {
		return errors.New("signal: session already open")
	}
	i.sessions[connectionID] = &session{
		sessionID: connectionID, attemptID: attemptID,
		requestMessageID: requestMessageID, buyerPublicKey: buyerPublicKey,
	}
	return nil
}

// CloseSession forgets one session.
func (i *Inbox) CloseSession(connectionID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.sessions, connectionID)
}

// NewSessionID generates a fresh ChannelProtocol WebRTC session id.
func NewSessionID() (string, error) {
	sessionID, err := channels.NewSessionID(rand.Reader)
	if err != nil {
		return "", err
	}
	return sessionID.String(), nil
}

// Send implements webrtcinterconnect.SignalAdapter.
func (i *Inbox) Send(ctx context.Context, envelope webrtcinterconnect.Envelope) error {
	i.mu.Lock()
	entry := i.sessions[envelope.ConnectionID]
	i.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("signal: unknown session %s", envelope.ConnectionID)
	}
	if envelope.Signal.Type == webrtcinterconnect.SignalClose {
		// The inbox protocol has no close signal; the stream teardown carries it.
		return nil
	}
	body, err := bodyFor(entry, envelope.Signal)
	if err != nil {
		return err
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	messageID, err := channels.NewMessageID(rand.Reader)
	if err != nil {
		return err
	}
	targetChannel := channels.InboxChannel(entry.buyerPublicKey)
	sealed, err := inbox.SignAndSeal(inbox.UnsignedPrivateMessage{
		Channel:       targetChannel,
		FromPublicKey: i.PublicKey,
		Protocol:      SignalProtocol,
		MessageID:     messageID,
		IssuedAtMs:    now.UnixMilli(),
		ExpiresAtMs:   now.Add(signalLifetime()).UnixMilli(),
		Body:          body,
	}, i.PrivateKey)
	if err != nil {
		return fmt.Errorf("signal: seal signal: %w", err)
	}
	encoded, err := inbox.Marshal(sealed)
	if err != nil {
		return fmt.Errorf("signal: encode envelope: %w", err)
	}
	if _, err := i.Client.Publish(ctx, targetChannel, encoded); err != nil {
		return fmt.Errorf("signal: publish signal: %w", err)
	}
	if os.Getenv("KM_SIGNAL_DEBUG") == "1" {
		detail := ""
		if envelope.Signal.Candidate != nil {
			detail = " candidate=" + envelope.Signal.Candidate.Candidate
		} else if envelope.Signal.SDP != "" {
			detail = " sdp=" + strings.ReplaceAll(envelope.Signal.SDP, "\r\n", " | ")
			if len(detail) > 800 {
				detail = detail[:800]
			}
		}
		fmt.Fprintf(os.Stderr, "signal: sent %s session=%s%s\n", envelope.Signal.Type, envelope.ConnectionID, detail)
	}
	return nil
}

// Subscribe implements webrtcinterconnect.SignalAdapter.
func (i *Inbox) Subscribe(listener func(webrtcinterconnect.Envelope)) (func(), error) {
	if listener == nil {
		return nil, errors.New("signal: listener is required")
	}
	i.mu.Lock()
	i.listener = listener
	i.mu.Unlock()
	return func() {
		i.mu.Lock()
		i.listener = nil
		i.mu.Unlock()
	}, nil
}

// Handle opens and verifies one inbox delivery and routes the signal to the
// transport. It returns an error only for diagnostics; unknown sessions are
// ignored silently.
func (i *Inbox) Handle(contentJSON []byte) error {
	opened, err := inbox.Open(i.Channel(), contentJSON, i.PrivateKey)
	if err != nil {
		return fmt.Errorf("signal: open inbox message: %w", err)
	}
	if opened.Protocol() != SignalProtocol {
		return nil
	}
	body, err := webrtcsignal.ParseBody(opened.BodyJSON())
	if err != nil {
		return fmt.Errorf("signal: invalid webrtc body: %w", err)
	}
	i.mu.Lock()
	entry := i.sessions[body.SessionID.String()]
	listener := i.listener
	i.mu.Unlock()
	if entry == nil || listener == nil {
		return nil
	}
	if !body.RequestMessageID.Equal(entry.requestMessageID) {
		return errors.New("signal: signal does not reference the session hash request")
	}
	if !opened.FromPublicKey().Equal(entry.buyerPublicKey) {
		return errors.New("signal: signal sender is not the session buyer")
	}
	fromPeerID, err := identity.PeerIDFromPublicKey(opened.FromPublicKey().Bytes())
	if err != nil {
		return err
	}
	toPeerID, err := identity.PeerIDFromPublicKey(i.PublicKey.Bytes())
	if err != nil {
		return err
	}
	signal, err := signalFrom(body.Signal)
	if err != nil {
		return err
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	if os.Getenv("KM_SIGNAL_DEBUG") == "1" {
		detail := ""
		if signal.Candidate != nil {
			detail = " candidate=" + signal.Candidate.Candidate
		} else if signal.SDP != "" {
			detail = " sdp=" + strings.ReplaceAll(signal.SDP, "\r\n", " | ")
			if len(detail) > 800 {
				detail = detail[:800]
			}
		}
		fmt.Fprintf(os.Stderr, "signal: routed %s session=%s from=%s%s\n", signal.Type, entry.sessionID, opened.FromPublicKey().String(), detail)
	}
	listener(webrtcinterconnect.Envelope{
		ConnectionID: entry.sessionID,
		AttemptID:    entry.attemptID,
		From:         fromPeerID.String(),
		To:           toPeerID.String(),
		Sequence:     0,
		ExpiresAtMs:  now.Add(signalLifetime()).UnixMilli(),
		Signal:       signal,
	})
	return nil
}

func bodyFor(entry *session, signal webrtcinterconnect.Signal) (webrtcsignal.WebRTCSignalV1Body, error) {
	requestID, err := channels.ParseMessageID(entry.requestMessageID.String())
	if err != nil {
		return webrtcsignal.WebRTCSignalV1Body{}, err
	}
	sessionID, err := channels.ParseSessionID(entry.sessionID)
	if err != nil {
		return webrtcsignal.WebRTCSignalV1Body{}, err
	}
	switch signal.Type {
	case webrtcinterconnect.SignalOffer:
		return webrtcsignal.NewOffer(requestID, sessionID, signal.SDP)
	case webrtcinterconnect.SignalAnswer:
		return webrtcsignal.NewAnswer(requestID, sessionID, signal.SDP)
	case webrtcinterconnect.SignalICECandidate:
		if signal.Candidate == nil {
			return webrtcsignal.WebRTCSignalV1Body{}, errors.New("signal: ice candidate is missing")
		}
		var index *int
		if signal.Candidate.SDPMLineIndex != nil {
			value := int(*signal.Candidate.SDPMLineIndex)
			index = &value
		}
		return webrtcsignal.NewICECandidate(requestID, sessionID, webrtcsignal.ICECandidate{
			Candidate:     signal.Candidate.Candidate,
			SDPMid:        signal.Candidate.SDPMid,
			SDPMLineIndex: index,
		})
	case webrtcinterconnect.SignalEndOfCandidates:
		return webrtcsignal.NewEndOfCandidates(requestID, sessionID)
	default:
		return webrtcsignal.WebRTCSignalV1Body{}, fmt.Errorf("signal: unsupported signal type %q", signal.Type)
	}
}

func signalFrom(signal webrtcsignal.Signal) (webrtcinterconnect.Signal, error) {
	switch signal.Type {
	case webrtcsignal.SignalOffer:
		return webrtcinterconnect.Signal{Type: webrtcinterconnect.SignalOffer, SDP: signal.SDP}, nil
	case webrtcsignal.SignalAnswer:
		return webrtcinterconnect.Signal{Type: webrtcinterconnect.SignalAnswer, SDP: signal.SDP}, nil
	case webrtcsignal.SignalICECandidate:
		if signal.Candidate == nil {
			return webrtcinterconnect.Signal{}, errors.New("signal: ice candidate is missing")
		}
		var index *uint16
		if signal.Candidate.SDPMLineIndex != nil {
			value := uint16(*signal.Candidate.SDPMLineIndex)
			index = &value
		}
		return webrtcinterconnect.Signal{
			Type: webrtcinterconnect.SignalICECandidate,
			Candidate: &webrtcinterconnect.ICECandidate{
				Candidate:     signal.Candidate.Candidate,
				SDPMid:        signal.Candidate.SDPMid,
				SDPMLineIndex: index,
			},
		}, nil
	case webrtcsignal.SignalEndOfCandidates:
		return webrtcinterconnect.Signal{Type: webrtcinterconnect.SignalEndOfCandidates}, nil
	default:
		return webrtcinterconnect.Signal{}, fmt.Errorf("signal: unsupported signal type %q", signal.Type)
	}
}

// BuyerPeerID derives the libp2p peer id the browser uses for its identity
// key, so the seller can dial it over WebRTC.
func BuyerPeerID(publicKeyHex string) (string, error) {
	raw, err := hex.DecodeString(strings.ToLower(publicKeyHex))
	if err != nil {
		return "", err
	}
	id, err := identity.PeerIDFromPublicKey(raw)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
