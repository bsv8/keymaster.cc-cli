// Package arbitration queries authenticated BitFS arbitration fee schedules.
// The service directory is local policy; a seller cannot nominate an unknown
// identity or supply a fee schedule. Wire v1 and info v2 are separate versions.
package arbitration

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bsv8/bitcoin-libp2p/streamio"
	bitfsprotocol "github.com/bsv8/go-bitfs/protocol"
	"github.com/fxamacker/cbor/v2"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

const InfoProtocol protocol.ID = "/bitfs/arbitration/info/1.0.0"
const maxInfoBytes = 4096
const maxSafeInteger uint64 = 9007199254740991

type Endpoint struct {
	Name         string `json:"name,omitempty"`
	PublicKeyHex string `json:"publicKeyHex"`
	Address      string `json:"address"`
}

type Info struct {
	BaseSat          uint64
	PerItemSat       uint64
	RetentionSeconds uint64
}

// Resolver selects an offered, locally trusted arbiter and queries its fee.
// Query failure must not fall back to fabricated fees or an untrusted key.
type Resolver interface {
	Select(context.Context, [][]byte) (bitfsprotocol.PublicKey, Info, error)
}

type Client struct {
	Host      host.Host
	Endpoints []Endpoint
	Timeout   time.Duration
}

// PeerID derives the transport identity from the business public key.
func PeerID(publicKeyHex string) (peer.ID, error) {
	raw, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return "", fmt.Errorf("arbitration: public key hex: %w", err)
	}
	key, err := crypto.UnmarshalSecp256k1PublicKey(raw)
	if err != nil || len(raw) != 33 || (raw[0] != 2 && raw[0] != 3) || hex.EncodeToString(raw) != publicKeyHex {
		return "", errors.New("arbitration: lower-case compressed secp256k1 public key required")
	}
	return peer.IDFromPublicKey(key)
}

// AddressInfo rejects an endpoint whose /p2p identity differs from its key.
func (e Endpoint) AddressInfo() (*peer.AddrInfo, error) {
	expected, err := PeerID(e.PublicKeyHex)
	if err != nil {
		return nil, err
	}
	address, err := ma.NewMultiaddr(e.Address)
	if err != nil {
		return nil, fmt.Errorf("arbitration: invalid address: %w", err)
	}
	info, err := peer.AddrInfoFromP2pAddr(address)
	if err != nil {
		return nil, fmt.Errorf("arbitration: full /p2p address required: %w", err)
	}
	if info.ID != expected {
		return nil, errors.New("arbitration: address peer does not match public key")
	}
	return info, nil
}

func (c *Client) Select(ctx context.Context, offered [][]byte) (bitfsprotocol.PublicKey, Info, error) {
	for _, endpoint := range c.Endpoints {
		for _, key := range offered {
			if hex.EncodeToString(key) != endpoint.PublicKeyHex {
				continue
			}
			publicKey, err := bitfsprotocol.PublicKeyFromBytes(key)
			if err != nil {
				return bitfsprotocol.PublicKey{}, Info{}, err
			}
			info, err := c.Query(ctx, endpoint)
			return publicKey, info, err
		}
	}
	return bitfsprotocol.PublicKey{}, Info{}, errors.New("arbitration: quote offers no locally trusted arbiter")
}

// Query uses the buyer's existing host, with bounded connect/write/read and
// cancellation. libp2p authenticates the pinned peer during the connection.
func (c *Client) Query(ctx context.Context, endpoint Endpoint) (Info, error) {
	if c.Host == nil {
		return Info{}, errors.New("arbitration: host required")
	}
	remote, err := endpoint.AddressInfo()
	if err != nil {
		return Info{}, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Host.Connect(ctx, *remote); err != nil {
		return Info{}, fmt.Errorf("arbitration: connect: %w", err)
	}
	stream, err := c.Host.NewStream(ctx, remote.ID, InfoProtocol)
	if err != nil {
		return Info{}, fmt.Errorf("arbitration: info stream: %w", err)
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Reset() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	framer, err := streamio.NewUvarintFramer(stream, streamio.WithMaxInboundFrameBytes(maxInfoBytes))
	if err != nil {
		return Info{}, err
	}
	if err := framer.WriteFrame([]byte{1}); err != nil {
		_ = stream.Reset()
		return Info{}, fmt.Errorf("arbitration: info write: %w", err)
	}
	raw, err := framer.ReadFrame()
	if err != nil {
		_ = stream.Reset()
		return Info{}, fmt.Errorf("arbitration: info read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	return DecodeInfo(raw, remote.ID)
}

// DecodeInfo validates the external canonical response envelope and info v2.
// Legacy per-KiB info is deliberately not accepted as per-item pricing.
func DecodeInfo(raw []byte, expected peer.ID) (Info, error) {
	mode, err := (cbor.DecOptions{IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden,
		MaxArrayElements: 32, MaxMapPairs: 16, MaxNestedLevels: 8}).DecMode()
	if err != nil {
		return Info{}, err
	}
	var envelope []cbor.RawMessage
	if err := mode.Unmarshal(raw, &envelope); err != nil {
		return Info{}, fmt.Errorf("arbitration: invalid info envelope: %w", err)
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return Info{}, err
	}
	var value any
	if err := mode.Unmarshal(raw, &value); err != nil {
		return Info{}, err
	}
	canonical, err := enc.Marshal(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Info{}, errors.New("arbitration: noncanonical envelope")
	}
	var version, status, kind uint64
	if len(envelope) < 2 || mode.Unmarshal(envelope[0], &version) != nil || version != 1 || mode.Unmarshal(envelope[1], &status) != nil {
		return Info{}, errors.New("arbitration: invalid envelope version/status")
	}
	if status == 1 {
		var code, trace string
		var retryable bool
		var delay uint64
		if len(envelope) != 6 || mode.Unmarshal(envelope[2], &code) != nil || mode.Unmarshal(envelope[3], &retryable) != nil || mode.Unmarshal(envelope[4], &delay) != nil || mode.Unmarshal(envelope[5], &trace) != nil {
			return Info{}, errors.New("arbitration: invalid error envelope")
		}
		return Info{}, fmt.Errorf("arbitration: service rejected info: %s (retryable=%t, retryAfterMs=%d)", code, retryable, delay)
	}
	if status != 0 || len(envelope) != 4 || mode.Unmarshal(envelope[2], &kind) != nil || kind != 0 {
		return Info{}, errors.New("arbitration: expected info success envelope Kind 0")
	}
	var body []byte
	if err := mode.Unmarshal(envelope[3], &body); err != nil {
		return Info{}, errors.New("arbitration: info body must be bytes")
	}
	var fields []cbor.RawMessage
	if err := mode.Unmarshal(body, &fields); err != nil || len(fields) != 9 {
		return Info{}, errors.New("arbitration: info v2 requires 9 fields; per-KiB info is unsupported")
	}
	var infoVersion uint64
	var id, unit string
	if mode.Unmarshal(fields[0], &infoVersion) != nil || infoVersion != 2 {
		return Info{}, errors.New("arbitration: info v2/content-item required; legacy pricing rejected")
	}
	if mode.Unmarshal(fields[1], &id) != nil || id != expected.String() {
		return Info{}, errors.New("arbitration: info peer identity mismatch")
	}
	if mode.Unmarshal(fields[8], &unit) != nil || unit != "content-item" {
		return Info{}, errors.New("arbitration: content-item fee unit required")
	}
	var info Info
	for i, target := range map[int]*uint64{3: &info.BaseSat, 4: &info.PerItemSat, 5: &info.RetentionSeconds} {
		if mode.Unmarshal(fields[i], target) != nil || *target > maxSafeInteger {
			return Info{}, fmt.Errorf("arbitration: invalid info field %d", i)
		}
	}
	if info.BaseSat == 0 {
		return Info{}, errors.New("arbitration: base fee must be positive")
	}
	return info, nil
}
