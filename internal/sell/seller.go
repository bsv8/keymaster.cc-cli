// Package sell implements the seller side of BitFS over the catalog: quotes,
// presign, funding verification, content delivery, payment completion and
// negotiated close.
//
// Everything is persist-before-send: exact Kind 1/2/3/4/5/6/7/12/13 bytes and
// the derived pool state are written before they are returned. Repeating a
// request for the same authorization reuses the saved exact answer instead of
// signing a new one.
package sell

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tx "github.com/bsv-blockchain/go-sdk/transaction"
	masterseed "github.com/bsv8/MasterSeed"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/pool"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/go-bitfs/seller"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/pricing"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

const (
	defaultQuoteTTL       = 300 * time.Second
	defaultMaxBlocksBatch = 64
	defaultMediaType      = "application/octet-stream"
	sessionQuoteName      = "quote.bin"
	sessionKind2Name      = "kind2-opening-request.bin"
	sessionKind3Name      = "kind3-opening-response.bin"
	sessionKind4Name      = "kind4-funding-delivery.bin"
	sessionMetaName       = "meta.json"
	sessionOpeningName    = "opening.cbor"
	sessionLatestPayment  = "latest-payment.bin"
	sessionFundingRawName = "funding-transaction.bin"
	sessionKind12Name     = "kind12-close-request.bin"
	sessionKind13Name     = "kind13-close-response.bin"
	sessionCloseTxName    = "close-transaction.bin"
	deliveriesDir         = "deliveries"
	paymentsDir           = "payments"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// Seller serves one catalog root.
type Seller struct {
	indexMu  sync.RWMutex
	Sessions *store.Store
	Signer   protocol.Signer
	Facts    func() protocol.Facts
	Index    *catalog.Index
	Pricing  *pricing.Pricing
	// Arbiters are the arbiter public keys this server offers in quotes.
	Arbiters []protocol.PublicKey
	// Wallet broadcasts the close transaction (optional: without it closes are
	// persisted but not submitted).
	Wallet walletBroadcaster
	// QuoteTTL is the quote validity; defaults to 300s.
	QuoteTTL time.Duration
	// MaxBlocksPerBatch is the quote batch limit; defaults to 64.
	MaxBlocksPerBatch uint64
}

// walletBroadcaster is the minimal chain port the seller needs.
type walletBroadcaster interface {
	BroadcastRaw(ctx context.Context, kind, rawHex string) (string, error)
}

type sessionMeta struct {
	SessionID         string `json:"sessionId"`
	SellerID          string `json:"sellerId"`
	SeedHashHex       string `json:"seedHash"`
	FilePath          string `json:"filePath"`
	ExpiresAt         int64  `json:"expiresAt"`
	MaxBlocksPerBatch uint64 `json:"maxBlocksPerBatch"`
}

type deliveryRecord struct {
	AuthorizationID string `json:"authorizationId"`
	Kind5Path       string `json:"kind5"`
	Kind6Path       string `json:"kind6"`
}

// QuoteRequest is one buyer's demand for a quote.
type QuoteRequest struct {
	SessionID           string
	SellerID            string
	BuyerPublicKey      protocol.PublicKey
	SeedHashHex         string
	RecommendedFilename string
}

// QuoteResult is the persisted quote.
type QuoteResult struct {
	SessionID  string
	Kind1      []byte
	ExpiresAt  int64
	SeedPrice  uint64
	BlockPrice uint64
	FilePath   string
	FileSize   uint64
	BlockCount uint64
}

func (s *Seller) validate() error {
	if s.Sessions == nil || s.Signer == nil || s.Facts == nil || s.currentIndex() == nil || s.Pricing == nil {
		return errors.New("sell: sessions store, signer, facts, index and pricing are required")
	}
	return nil
}

func validSessionID(sessionID string) error {
	if len(sessionID) == 0 || len(sessionID) > 200 || !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("sell: invalid session id %q", sessionID)
	}
	for _, segment := range strings.Split(sessionID, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("sell: invalid session id %q", sessionID)
		}
	}
	return nil
}

func sessionPath(sessionID, name string) string {
	return "sessions/" + sessionID + "/" + name
}

// CreateQuote prices one file and signs exact Kind 1. The quote is persisted
// before it is returned.
func (s *Seller) CreateQuote(ctx context.Context, request QuoteRequest) (*QuoteResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(request.SessionID); err != nil {
		return nil, err
	}
	entry := s.currentIndex().Preferred(request.SeedHashHex)
	if entry == nil {
		return nil, fmt.Errorf("sell: no catalog file for seed %s", request.SeedHashHex)
	}
	if err := ensureCatalogEntryUnchanged(entry); err != nil {
		return nil, err
	}
	if request.RecommendedFilename != "" {
		if sanitized := sanitizeName(request.RecommendedFilename); sanitized != "" {
			entry = &catalog.Entry{
				Path: entry.Path, RelativePath: entry.RelativePath, Name: sanitized,
				Size: entry.Size, SeedHashHex: entry.SeedHashHex, BlockCount: entry.BlockCount, Seed: entry.Seed,
			}
		}
	}
	seedPrice, blockPrice, _ := s.Pricing.Effective(entry.RelativePath)
	seedPriceValue, err := parseSats(seedPrice)
	if err != nil {
		return nil, err
	}
	blockPriceValue, err := parseSats(blockPrice)
	if err != nil {
		return nil, err
	}
	ttl := s.QuoteTTL
	if ttl <= 0 {
		ttl = defaultQuoteTTL
	}
	maxBlocks := s.MaxBlocksPerBatch
	if maxBlocks == 0 {
		maxBlocks = defaultMaxBlocksBatch
	}
	expiresAt := s.Facts().Now.Unix() + int64(ttl/time.Second)
	artifact, _, err := seller.CreateQuote(ctx, s.Facts(), s.Signer, seller.QuoteDraft{
		SeedHash:                   seedHashBytes(entry.SeedHashHex),
		BuyerPublicKey:             request.BuyerPublicKey,
		SeedPriceSatoshis:          protocol.Satoshis(seedPriceValue),
		FullBlockPriceSatoshis:     protocol.Satoshis(blockPriceValue),
		FileSizeBytes:              entry.Size,
		QuoteExpiresAtUnixSeconds:  content.UnixSeconds(expiresAt),
		SupportedArbiterPublicKeys: append([]protocol.PublicKey(nil), s.Arbiters...),
		RecommendedFilename:        entry.Name,
		MaxBlocksPerBatch:          maxBlocks,
		MediaType:                  mediaTypeFor(entry.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("sell: create quote: %w", err)
	}
	kind1 := artifact.Bytes()
	if err := s.Sessions.SaveExact(sessionPath(request.SessionID, sessionQuoteName), kind1); err != nil {
		return nil, err
	}
	meta := sessionMeta{
		SessionID: request.SessionID, SellerID: request.SellerID, SeedHashHex: entry.SeedHashHex,
		FilePath: entry.Path, ExpiresAt: expiresAt, MaxBlocksPerBatch: maxBlocks,
	}
	if err := s.saveMeta(meta); err != nil {
		return nil, err
	}
	return &QuoteResult{
		SessionID: request.SessionID, Kind1: kind1, ExpiresAt: expiresAt,
		SeedPrice: seedPriceValue, BlockPrice: blockPriceValue,
		FilePath: entry.Path, FileSize: entry.Size, BlockCount: entry.BlockCount,
	}, nil
}

// Presign answers exact Kind 2 with exact Kind 3. Repeated calls for the same
// Kind 2 return the saved Kind 3.
func (s *Seller) Presign(ctx context.Context, sessionID string, kind2 []byte) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	if _, err := s.loadMeta(sessionID); err != nil {
		return nil, err
	}
	if existing, err := s.Sessions.Load(sessionPath(sessionID, sessionKind3Name)); err == nil {
		if savedKind2, err := s.Sessions.Load(sessionPath(sessionID, sessionKind2Name)); err == nil && string(savedKind2) == string(kind2) {
			return existing, nil
		}
		return nil, errors.New("sell: session already presigned a different opening request")
	}
	artifact, _, err := seller.PreparePresign(ctx, kind2, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: prepare presign: %w", err)
	}
	kind3 := artifact.Bytes()
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionKind2Name), kind2); err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionKind3Name), kind3); err != nil {
		return nil, err
	}
	return kind3, nil
}

// VerifyFunding verifies exact Kind 4 and persists the opening and pool state.
func (s *Seller) VerifyFunding(ctx context.Context, sessionID string, kind4 []byte) error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := validSessionID(sessionID); err != nil {
		return err
	}
	opening, err := s.loadOpening(sessionID)
	if err != nil {
		return err
	}
	fundingRaw, poolEvidence, err := seller.VerifyFunding(kind4, opening)
	if err != nil {
		return fmt.Errorf("sell: verify funding: %w", err)
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionKind4Name), kind4); err != nil {
		return err
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionFundingRawName), fundingRaw); err != nil {
		return err
	}
	encoded, err := pool.EncodeOpeningProof(poolEvidence.Opening)
	if err != nil {
		return fmt.Errorf("sell: encode opening proof: %w", err)
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionOpeningName), encoded); err != nil {
		return err
	}
	return nil
}

// PrepareDelivery verifies exact Kind 5, serves the content from the catalog
// and persists exact Kind 6. A repeated authorization returns the saved
// delivery byte for byte.
func (s *Seller) PrepareDelivery(ctx context.Context, sessionID string, kind5 []byte) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	authorizationID, err := authorizationIDOf(kind5)
	if err != nil {
		return nil, fmt.Errorf("sell: invalid kind5: %w", err)
	}
	kind6Path := sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/kind6.bin")
	if existing, err := s.Sessions.Load(kind6Path); err == nil {
		return existing, nil
	}
	quoteRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionQuoteName))
	if err != nil {
		return nil, fmt.Errorf("sell: session quote is missing: %w", err)
	}
	poolEvidence, err := s.loadPool(sessionID)
	if err != nil {
		return nil, err
	}
	meta, err := s.loadMeta(sessionID)
	if err != nil {
		return nil, err
	}
	payloads, seed, err := s.servePayloads(meta, kind5)
	if err != nil {
		return nil, err
	}
	// Pre-checking turns protocol rejections into errors before any content is
	// read or signed.
	if _, err := seller.InspectDeliveryRequest(s.Facts(), seller.InspectDeliveryRequestInput{
		QuoteRaw: quoteRaw, Pool: *poolEvidence, RequestRaw: kind5,
	}); err != nil {
		return nil, fmt.Errorf("sell: reject delivery request: %w", err)
	}
	artifact, _, err := seller.PrepareDelivery(ctx, s.Facts(), seller.DeliveryInput{
		QuoteRaw: quoteRaw, Pool: *poolEvidence, RequestRaw: kind5,
		ContentPayloads: payloads, Seed: seed,
	}, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: prepare delivery: %w", err)
	}
	kind6 := artifact.Bytes()
	if err := s.Sessions.SaveExact(sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/kind5.bin"), kind5); err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(kind6Path, kind6); err != nil {
		return nil, err
	}
	record := deliveryRecord{AuthorizationID: authorizationID,
		Kind5Path: sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/kind5.bin"),
		Kind6Path: kind6Path}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/record.json"), encoded); err != nil {
		return nil, err
	}
	return kind6, nil
}

// CompletePayment verifies exact Kind 7 against the saved delivery and
// persists the completed payment transaction as the pool's latest state.
func (s *Seller) CompletePayment(ctx context.Context, sessionID string, kind7 []byte) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	authorizationID, err := authorizationIDOfUpdate(kind7)
	if err != nil {
		return nil, fmt.Errorf("sell: invalid kind7: %w", err)
	}
	paymentPath := sessionPath(sessionID, paymentsDir+"/"+authorizationID+".bin")
	if existing, err := s.Sessions.Load(paymentPath); err == nil {
		return existing, nil
	}
	recordRaw, err := s.Sessions.Load(sessionPath(sessionID, deliveriesDir+"/"+authorizationID+"/record.json"))
	if err != nil {
		return nil, fmt.Errorf("sell: no saved delivery for this payment: %w", err)
	}
	var record deliveryRecord
	if err := json.Unmarshal(recordRaw, &record); err != nil {
		return nil, fmt.Errorf("sell: delivery record is corrupt: %w", err)
	}
	kind5, err := s.Sessions.Load(record.Kind5Path)
	if err != nil {
		return nil, err
	}
	kind6, err := s.Sessions.Load(record.Kind6Path)
	if err != nil {
		return nil, err
	}
	quoteRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionQuoteName))
	if err != nil {
		return nil, err
	}
	poolEvidence, err := s.loadPool(sessionID)
	if err != nil {
		return nil, err
	}
	completeRaw, nextPool, err := seller.CompletePayment(ctx, s.Facts(), seller.CompletePaymentInput{
		Pool: *poolEvidence,
		Delivery: seller.SellerDeliveryEvidence{
			RawKind1: quoteRaw, RawKind5: kind5, RawKind6: kind6,
		},
		RequestRaw: kind5,
		UpdateRaw:  kind7,
	}, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: complete payment: %w", err)
	}
	if err := s.Sessions.SaveExact(paymentPath, completeRaw); err != nil {
		return nil, err
	}
	// The latest payment pointer advances every round, so it is a state
	// snapshot rather than an immutable artifact.
	if err := s.Sessions.Save(sessionPath(sessionID, sessionLatestPayment), completeRaw); err != nil {
		return nil, err
	}
	if nextPool.Opening != nil {
		encoded, err := pool.EncodeOpeningProof(nextPool.Opening)
		if err != nil {
			return nil, err
		}
		if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionOpeningName), encoded); err != nil {
			return nil, err
		}
	}
	return completeRaw, nil
}

// CompleteClose answers exact Kind 12 with exact Kind 13 and broadcasts the
// close transaction (when a wallet port is configured). The buyer only
// observes it; the seller is the submitter.
func (s *Seller) CompleteClose(ctx context.Context, sessionID string, kind12 []byte) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := validSessionID(sessionID); err != nil {
		return nil, err
	}
	kind13Path := sessionPath(sessionID, sessionKind13Name)
	if existing, err := s.Sessions.Load(kind13Path); err == nil {
		if saved, err := s.Sessions.Load(sessionPath(sessionID, sessionKind12Name)); err == nil && string(saved) == string(kind12) {
			return existing, nil
		}
		return nil, errors.New("sell: session already answered a different close request")
	}
	poolEvidence, err := s.loadPool(sessionID)
	if err != nil {
		return nil, err
	}
	artifact, err := seller.CompleteCloseArtifact(ctx, s.Facts(), seller.CompleteCloseArtifactInput{
		Pool: *poolEvidence, RequestRaw: kind12,
	}, s.Signer)
	if err != nil {
		return nil, fmt.Errorf("sell: complete close: %w", err)
	}
	kind13 := artifact.Bytes()
	artifactResponse, err := wire.ParseAs(wire.PoolCloseResponse, kind13)
	if err != nil {
		return nil, err
	}
	response, err := wire.DecodePoolCloseResponse(artifactResponse)
	if err != nil {
		return nil, err
	}
	parsed, err := tx.NewTransactionFromBytes(response.CompleteCloseTransactionRaw)
	if err != nil {
		return nil, fmt.Errorf("sell: parse close transaction: %w", err)
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionKind12Name), kind12); err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(kind13Path, kind13); err != nil {
		return nil, err
	}
	if err := s.Sessions.SaveExact(sessionPath(sessionID, sessionCloseTxName), response.CompleteCloseTransactionRaw); err != nil {
		return nil, err
	}
	if s.Wallet != nil {
		if _, err := s.Wallet.BroadcastRaw(ctx, "pool-close", hex.EncodeToString(response.CompleteCloseTransactionRaw)); err != nil {
			return nil, fmt.Errorf("sell: broadcast close %s: %w", parsed.TxID().String(), err)
		}
	}
	return kind13, nil
}

// servePayloads resolves the requested hashes from the catalog: the seed hash
// serves the seed bytes, block hashes serve their block from the file.
func (s *Seller) servePayloads(meta sessionMeta, kind5 []byte) ([][]byte, []byte, error) {
	hashes, err := contentHashesOf(kind5)
	if err != nil {
		return nil, nil, err
	}
	entry := s.currentIndex().Preferred(meta.SeedHashHex)
	if entry == nil {
		return nil, nil, fmt.Errorf("sell: catalog file for seed %s disappeared", meta.SeedHashHex)
	}
	if err := ensureCatalogEntryUnchanged(entry); err != nil {
		return nil, nil, err
	}
	payloads := make([][]byte, 0, len(hashes))
	needSeed := false
	for _, hash := range hashes {
		hashHex := hex.EncodeToString(hash)
		if hashHex == meta.SeedHashHex {
			payloads = append(payloads, append([]byte(nil), entry.Seed...))
			continue
		}
		payload, err := s.readBlock(entry, hash)
		if err != nil {
			return nil, nil, err
		}
		payloads = append(payloads, payload)
		needSeed = true
	}
	var seed []byte
	if needSeed {
		seed = append([]byte(nil), entry.Seed...)
	}
	return payloads, seed, nil
}

func (s *Seller) readBlock(entry *catalog.Entry, hash []byte) ([]byte, error) {
	for index := uint64(0); index < entry.BlockCount; index++ {
		expected, err := masterseed.ReadBlockHash(context.Background(), bytes.NewReader(entry.Seed), uint64(len(entry.Seed)), index)
		if err != nil {
			return nil, err
		}
		if string(expected.Bytes()) != string(hash) {
			continue
		}
		size, err := masterseed.ExpectedBlockSize(entry.Size, index)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("sell: open catalog file: %w", err)
		}
		defer file.Close()
		buffer := make([]byte, size)
		if _, err := file.ReadAt(buffer, int64(index)*masterseed.BlockSize); err != nil {
			return nil, fmt.Errorf("sell: read block %d: %w", index, err)
		}
		return buffer, nil
	}
	return nil, fmt.Errorf("sell: block %s is not part of this file", hex.EncodeToString(hash))
}

func (s *Seller) saveMeta(meta sessionMeta) error {
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return s.Sessions.Save(sessionPath(meta.SessionID, sessionMetaName), append(encoded, '\n'))
}

func (s *Seller) loadMeta(sessionID string) (sessionMeta, error) {
	raw, err := s.Sessions.Load(sessionPath(sessionID, sessionMetaName))
	if err != nil {
		return sessionMeta{}, fmt.Errorf("sell: load session meta: %w", err)
	}
	var meta sessionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return sessionMeta{}, fmt.Errorf("sell: decode session meta: %w", err)
	}
	if meta.SessionID != sessionID {
		return sessionMeta{}, errors.New("sell: session meta belongs to another session")
	}
	return meta, nil
}

func (s *Seller) loadOpening(sessionID string) (seller.SellerOpeningEvidence, error) {
	kind2, err := s.Sessions.Load(sessionPath(sessionID, sessionKind2Name))
	if err != nil {
		return seller.SellerOpeningEvidence{}, fmt.Errorf("sell: opening request is missing: %w", err)
	}
	kind3, err := s.Sessions.Load(sessionPath(sessionID, sessionKind3Name))
	if err != nil {
		return seller.SellerOpeningEvidence{}, fmt.Errorf("sell: opening response is missing: %w", err)
	}
	return seller.SellerOpeningEvidence{RawKind2: kind2, RawKind3: kind3}, nil
}

func (s *Seller) loadPool(sessionID string) (*seller.SellerPoolEvidence, error) {
	openingRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionOpeningName))
	if err != nil {
		return nil, fmt.Errorf("sell: pool opening is missing: %w", err)
	}
	opening, err := pool.DecodeOpeningProof(openingRaw)
	if err != nil {
		return nil, fmt.Errorf("sell: decode pool opening: %w", err)
	}
	fundingRaw, err := s.Sessions.Load(sessionPath(sessionID, sessionFundingRawName))
	if err != nil {
		return nil, fmt.Errorf("sell: funding bytes are missing: %w", err)
	}
	latest, err := s.Sessions.Load(sessionPath(sessionID, sessionLatestPayment))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &seller.SellerPoolEvidence{Opening: opening, FundingTransactionRaw: fundingRaw, LatestPaymentRawTx: latest}, nil
}

// ensureCatalogEntryUnchanged rejects a quote or delivery whose source file
// changed after the catalog scan, so stale prices or bytes are never served.
func ensureCatalogEntryUnchanged(entry *catalog.Entry) error {
	info, err := os.Stat(entry.Path)
	if err != nil {
		return fmt.Errorf("sell: catalog file %s is unavailable: %w", entry.RelativePath, err)
	}
	if uint64(info.Size()) != entry.Size || !info.ModTime().Equal(entry.ModTime) {
		return fmt.Errorf("sell: catalog file %s changed since the scan; rescan before selling", entry.RelativePath)
	}
	return nil
}

func mediaTypeFor(name string) string {
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if index := strings.IndexByte(mediaType, ';'); index >= 0 {
		mediaType = mediaType[:index]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "" || !strings.Contains(mediaType, "/") || len(mediaType) > 255 {
		return defaultMediaType
	}
	return mediaType
}

func sanitizeName(name string) string {
	if index := strings.LastIndexAny(name, `/\`); index >= 0 {
		name = name[index+1:]
	}
	name = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}

func parseSats(value string) (uint64, error) {
	result, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sell: invalid amount %q", value)
	}
	return result, nil
}

func seedHashBytes(hexValue string) []byte {
	raw, err := hex.DecodeString(hexValue)
	if err != nil {
		return nil
	}
	return raw
}

// authorizationIDOf recomputes the payment authorization id from exact Kind 5.
func authorizationIDOf(kind5 []byte) (string, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, kind5)
	if err != nil {
		return "", err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return "", err
	}
	id, err := content.PaymentAuthorizationID(request.PaymentAuthorizationCBOR)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// authorizationIDOfUpdate reads the referenced authorization id from exact
// Kind 7.
func authorizationIDOfUpdate(kind7 []byte) (string, error) {
	artifact, err := wire.ParseAs(wire.PaymentUpdate, kind7)
	if err != nil {
		return "", err
	}
	update, err := wire.DecodePaymentUpdate(artifact)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(update.PaymentAuthorizationID[:]), nil
}

// contentHashesOf decodes the ordered content hashes of exact Kind 5.
func contentHashesOf(kind5 []byte) ([][]byte, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, kind5)
	if err != nil {
		return nil, err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return nil, err
	}
	authorization, err := content.DecodePaymentAuthorization(request.PaymentAuthorizationCBOR)
	if err != nil {
		return nil, err
	}
	return content.DecodeContentHashes(authorization.ContentHashesCBOR)
}

// ReloadIndex swaps the catalog index after a rescan. New quotes and deliveries
// use the new index; sessions whose source disappeared fail safely (no quote,
// no delivery), and the buyer never pays for undelivered content.
func (s *Seller) ReloadIndex(index *catalog.Index) {
	if index == nil {
		return
	}
	s.indexMu.Lock()
	s.Index = index
	s.indexMu.Unlock()
}

func (s *Seller) currentIndex() *catalog.Index {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.Index
}
