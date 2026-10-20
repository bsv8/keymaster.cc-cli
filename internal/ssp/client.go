// Package ssp is the CLI's SatSubscription client: one authenticated
// libp2p stream to a supplier gateway speaking the /ssp/1.0.0 protocol over
// uvarint frames.
//
// The client owns framing, request/response correlation and delivery dispatch;
// the SSP wire itself comes from SatSubscriptionProtocol.
package ssp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv8/SatSubscriptionProtocol/client"
	sspprotocol "github.com/bsv8/SatSubscriptionProtocol/protocol"
	sspwire "github.com/bsv8/SatSubscriptionProtocol/wire"
	"github.com/bsv8/bitcoin-libp2p/golibp2p"
	"github.com/bsv8/bitcoin-libp2p/signer"
	"github.com/bsv8/bitcoin-libp2p/streamio"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
)

// ProtocolID is the fixed SSP V1 stream protocol.
const ProtocolID protocol.ID = "/ssp/1.0.0"

// Options configures one client connection.
type Options struct {
	// Gateway is the supplier multiaddr including /p2p/<peer id>.
	Gateway string
	// Signer authenticates the libp2p connection (the payer identity).
	Signer signer.Signer
	// MaxWireBytes overrides the 1 MiB wire limit.
	MaxWireBytes int
}

// Result is one successful or rejected SSP action.
type Result struct {
	Success       bool
	ChargedAmount string
	ErrorCode     string
}

// Client is one long-lived SSP stream.
type Client struct {
	host   host.Host
	remote peer.ID
	stream network.Stream
	framer *streamio.UvarintFramer

	writeMu sync.Mutex

	mu        sync.Mutex
	pending   map[sspprotocol.RequestID]chan Result
	handlers  []func(channel string, content []byte)
	err       error
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
}

// Dial connects to the gateway and opens the SSP stream.
func Dial(ctx context.Context, options Options) (*Client, error) {
	if options.Gateway == "" {
		return nil, errors.New("ssp: gateway multiaddr is required")
	}
	if options.Signer == nil {
		return nil, errors.New("ssp: signer is required")
	}
	gateway, err := ma.NewMultiaddr(options.Gateway)
	if err != nil {
		return nil, fmt.Errorf("ssp: invalid gateway multiaddr: %w", err)
	}
	info, err := peer.AddrInfoFromP2pAddr(gateway)
	if err != nil {
		return nil, fmt.Errorf("ssp: gateway multiaddr needs /p2p/<peer id>: %w", err)
	}
	maxWire := options.MaxWireBytes
	if maxWire <= 0 {
		maxWire = sspprotocol.MaxWireBytes
	}
	hostConfig := golibp2p.HostConfig{Signer: options.Signer}
	// Gateways are usually published as /tls/ws; add the websocket transport
	// only when the address needs it, keeping plain TCP dials minimal.
	if strings.Contains(options.Gateway, "/ws") {
		hostConfig.TransportOptions = append(hostConfig.TransportOptions, libp2p.Transport(websocket.New))
	}
	h, err := golibp2p.NewHost(hostConfig)
	if err != nil {
		return nil, fmt.Errorf("ssp: create host: %w", err)
	}
	if err := h.Connect(ctx, *info); err != nil {
		h.Close()
		return nil, fmt.Errorf("ssp: connect gateway: %w", err)
	}
	stream, err := h.NewStream(ctx, info.ID, ProtocolID)
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("ssp: open stream: %w", err)
	}
	framer, err := streamio.NewUvarintFramer(stream, streamio.WithMaxInboundFrameBytes(maxWire))
	if err != nil {
		stream.Close()
		h.Close()
		return nil, fmt.Errorf("ssp: create framer: %w", err)
	}
	client := &Client{
		host:    h,
		remote:  info.ID,
		stream:  stream,
		framer:  framer,
		pending: map[sspprotocol.RequestID]chan Result{},
		done:    make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// SetPublishHandler replaces the delivery callbacks for unsolicited Publish
// frames (channel broadcasts).
func (c *Client) SetPublishHandler(handler func(channel string, content []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = nil
	if handler != nil {
		c.handlers = append(c.handlers, handler)
	}
}

// AddPublishHandler registers an additional delivery callback. Different
// subsystems (demand discovery, inbox signaling) share one stream, so their
// handlers must not overwrite each other.
func (c *Client) AddPublishHandler(handler func(channel string, content []byte)) {
	if handler == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append(c.handlers, handler)
}

// Subscribe subscribes the authenticated identity to channel.
func (c *Client) Subscribe(ctx context.Context, channel string) (Result, error) {
	return c.action(func(requestID sspprotocol.RequestID) ([]byte, error) {
		return client.NewSubscribe(requestID, channel)
	})
}

// Unsubscribe removes a channel subscription.
func (c *Client) Unsubscribe(ctx context.Context, channel string) (Result, error) {
	return c.action(func(requestID sspprotocol.RequestID) ([]byte, error) {
		return client.NewUnsubscribe(requestID, channel)
	})
}

// Publish broadcasts contentJSON on channel.
func (c *Client) Publish(ctx context.Context, channel string, contentJSON []byte) (Result, error) {
	return c.action(func(requestID sspprotocol.RequestID) ([]byte, error) {
		return client.NewPublish(requestID, channel, contentJSON)
	})
}

func (c *Client) action(build func(sspprotocol.RequestID) ([]byte, error)) (Result, error) {
	requestID, err := client.NewRequestID()
	if err != nil {
		return Result{}, err
	}
	payload, err := build(requestID)
	if err != nil {
		return Result{}, err
	}
	resultChannel := make(chan Result, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Result{}, errors.New("ssp: client is closed")
	}
	c.pending[requestID] = resultChannel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}()
	if err := c.writeFrame(payload); err != nil {
		return Result{}, err
	}
	select {
	case result := <-resultChannel:
		if !result.Success {
			return result, fmt.Errorf("ssp: action rejected: %s", result.ErrorCode)
		}
		return result, nil
	case <-c.done:
		return Result{}, c.closedError()
	}
}

func (c *Client) writeFrame(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("ssp: refusing to write an empty wire")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.framer.WriteFrame(payload); err != nil {
		c.fail(fmt.Errorf("ssp: write frame: %w", err))
		return err
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		frame, err := c.framer.ReadFrame()
		if err != nil {
			c.fail(fmt.Errorf("ssp: read frame: %w", err))
			return
		}
		message, err := sspwire.Parse(frame)
		if err != nil {
			c.fail(fmt.Errorf("ssp: invalid wire: %w", err))
			return
		}
		switch message.Kind {
		case sspprotocol.ActionResultKind:
			response, err := client.ParseActionResult(frame)
			if err != nil {
				c.fail(fmt.Errorf("ssp: invalid action result: %w", err))
				return
			}
			c.mu.Lock()
			pending := c.pending[response.RequestID]
			c.mu.Unlock()
			if pending != nil {
				pending <- Result{Success: response.Success, ChargedAmount: response.ChargedAmount.String(), ErrorCode: response.ErrorCode}
			}
		case sspprotocol.PublishKind:
			publish, err := client.ParsePublish(frame)
			if err != nil {
				c.fail(fmt.Errorf("ssp: invalid delivery publish: %w", err))
				return
			}
			c.mu.Lock()
			handlers := append([]func(string, []byte){}, c.handlers...)
			c.mu.Unlock()
			for _, handler := range handlers {
				handler(publish.Channel, publish.ContentJSON)
			}
		}
	}
}

func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Client) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errors.New("ssp: client is closed")
}

// Close closes the stream and host.
func (c *Client) Close() error {
	c.fail(errors.New("ssp: client closed"))
	if c.stream != nil {
		c.stream.Close()
	}
	return c.host.Close()
}

// Remote returns the gateway peer id.
func (c *Client) Remote() peer.ID { return c.remote }

// Err returns the terminal stream error, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
