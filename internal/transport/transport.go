// Package transport binds BitFS artifacts to libp2p streams.
//
// The stream protocol and framing are shared with the browser client
// (/bitfs/wire/1.0.0, uvarint length + exact artifact bytes), so a Go peer can
// talk to the browser plugin and to another Go peer with the same code.
package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"

	bitfstransport "github.com/bsv8/go-bitfs/transport"
	"github.com/bsv8/go-bitfs/wire"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// ProtocolID is the shared BitFS stream protocol.
const ProtocolID = bitfstransport.ProtocolID

// Session is one exact-artifact stream.
type Session struct {
	stream  network.Stream
	framer  *bitfstransport.Framer
	counted *countingStream
}

// countingStream counts bytes read from the underlying stream so hosts can
// report real transfer progress while a large artifact is still arriving.
type countingStream struct {
	stream network.Stream
	mu     sync.Mutex
	read   uint64
	onRead func(uint64)
}

func (c *countingStream) Read(buffer []byte) (int, error) {
	n, err := c.stream.Read(buffer)
	if n > 0 {
		c.mu.Lock()
		c.read += uint64(n)
		total := c.read
		callback := c.onRead
		c.mu.Unlock()
		if callback != nil {
			callback(total)
		}
	}
	return n, err
}

func (c *countingStream) Write(buffer []byte) (int, error) { return c.stream.Write(buffer) }

func (c *countingStream) setProgress(callback func(uint64)) {
	c.mu.Lock()
	c.onRead = callback
	c.mu.Unlock()
}

func (c *countingStream) readBytes() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read
}

// NewSession wraps an accepted or dialed stream.
func NewSession(stream network.Stream) (*Session, error) {
	if stream == nil {
		return nil, errors.New("transport: stream is required")
	}
	counted := &countingStream{stream: stream}
	framer, err := bitfstransport.NewFramer(counted)
	if err != nil {
		return nil, err
	}
	return &Session{stream: stream, framer: framer, counted: counted}, nil
}

// Read returns the next exact artifact; a clean EOF is reported as io.EOF.
func (s *Session) Read() (wire.Artifact, error) { return s.framer.ReadArtifact() }

// Write sends one exact artifact.
func (s *Session) Write(artifact wire.Artifact) error { return s.framer.WriteArtifact(artifact) }

// Close closes the stream.
func (s *Session) Close() error { return s.stream.Close() }

// SetReadProgress installs a callback receiving cumulative stream bytes read.
func (s *Session) SetReadProgress(callback func(uint64)) {
	if s.counted != nil {
		s.counted.setProgress(callback)
	}
}

// ReadBytes returns the cumulative bytes read from the stream.
func (s *Session) ReadBytes() uint64 {
	if s.counted == nil {
		return 0
	}
	return s.counted.readBytes()
}

// RemotePublicKey returns the authenticated remote peer id.
func (s *Session) RemotePeer() peer.ID { return s.stream.Conn().RemotePeer() }

// Listen installs the BitFS stream handler on host. Every inbound stream is
// handed to handler in its own goroutine; the handler owns the session.
func Listen(h host.Host, handler func(*Session)) error {
	if h == nil || handler == nil {
		return errors.New("transport: host and handler are required")
	}
	h.SetStreamHandler(ProtocolID, func(stream network.Stream) {
		session, err := NewSession(stream)
		if err != nil {
			stream.Reset()
			return
		}
		handler(session)
	})
	return nil
}

// Dial connects to address and opens a BitFS stream. The address must carry a
// /p2p/<peer id> component so the remote identity is explicit.
func Dial(ctx context.Context, h host.Host, address string) (*Session, error) {
	if h == nil {
		return nil, errors.New("transport: host is required")
	}
	parsed, err := ma.NewMultiaddr(address)
	if err != nil {
		return nil, fmt.Errorf("transport: invalid address: %w", err)
	}
	info, err := peer.AddrInfoFromP2pAddr(parsed)
	if err != nil {
		return nil, fmt.Errorf("transport: address needs /p2p/<peer id>: %w", err)
	}
	if err := h.Connect(ctx, *info); err != nil {
		return nil, fmt.Errorf("transport: connect %s: %w", info.ID, err)
	}
	stream, err := h.NewStream(ctx, info.ID, ProtocolID)
	if err != nil {
		return nil, fmt.Errorf("transport: open stream: %w", err)
	}
	return NewSession(stream)
}
