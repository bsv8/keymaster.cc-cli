package collector

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

var testRecords = []Record{
	{SeedHashHex: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", FileName: "example.pdf", Description: "", FileSizeBytes: "123456", MediaType: "application/pdf"},
	{SeedHashHex: "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", FileName: "notes.txt", Description: "lorem ipsum", FileSizeBytes: "42", MediaType: "text/plain"},
}

// 固定向量：与协议 Go SDK、工程仓 parity 向量同一记录集。
const pinnedCanonical = `{"version":1,"files":[{"seedHashHex":"2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae","fileName":"notes.txt","description":"lorem ipsum","fileSizeBytes":"42","mediaType":"text/plain"},{"seedHashHex":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","fileName":"example.pdf","description":"","fileSizeBytes":"123456","mediaType":"application/pdf"}]}`

func testSigner(t *testing.T) SignFunc {
	t.Helper()
	sign, err := HostPrivateKeySigner(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sign
}

func TestCanonicalPinned(t *testing.T) {
	got, err := CanonicalJSON([]Record{testRecords[1], testRecords[0]})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pinnedCanonical {
		t.Fatalf("canonical mismatch:\n%s", got)
	}
}

func TestDigestSignVerify(t *testing.T) {
	canonical, err := CanonicalJSON(testRecords)
	if err != nil {
		t.Fatal(err)
	}
	gzipBytes, err := GzipJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := GunzipJSON(gzipBytes)
	if err != nil || !bytes.Equal(plain, canonical) {
		t.Fatalf("gzip round-trip failed: %v", err)
	}
	sign := testSigner(t)
	digest := DigestGzip(gzipBytes)
	der, pubkeyHex, err := sign(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDigestSignature(pubkeyHex, digest, der); err != nil {
		t.Fatal(err)
	}
	// low-S 断言。
	parsed, err := ec.ParseDERSignature(der)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.S.Cmp(halfOrder) == 1 {
		t.Fatal("signature is high-S")
	}
	evil := append([]byte(nil), gzipBytes...)
	evil[len(evil)/2] ^= 0x01
	if err := VerifyDigestSignature(pubkeyHex, DigestGzip(evil), der); err == nil {
		t.Fatal("tampered bytes must fail")
	}
}

func TestQueueMergeAndConfirm(t *testing.T) {
	dir := t.TempDir()
	svc, err := New(filepath.Join(dir, "collector.json"), Options{Endpoint: "http://example.invalid", Enabled: true, Sign: testSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	svc.NotifyStored(testRecords[0])
	svc.NotifyStored(testRecords[0])
	if got := len(svc.store.Snapshot().Pending); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	// 改名合并为最新。
	renamed := testRecords[0]
	renamed.FileName = "renamed.pdf"
	svc.NotifyStored(renamed)
	doc := svc.store.Snapshot()
	if doc.Pending[renamed.SeedHashHex].FileName != "renamed.pdf" {
		t.Fatalf("merge failed: %+v", doc.Pending)
	}
	// 介绍编辑附着到已有记录。
	if err := svc.SetDescription(renamed.SeedHashHex, "d1"); err != nil {
		t.Fatal(err)
	}
	if got := svc.store.Snapshot().Pending[renamed.SeedHashHex].Description; got != "d1" {
		t.Fatalf("description = %q", got)
	}
}

func TestSubmitRetryReusesBytes(t *testing.T) {
	var bodies [][]byte
	fails := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		buf.ReadFrom(r.Body)
		bodies = append(bodies, buf.Bytes())
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		digest := DigestGzip(buf.Bytes())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":1,"archiveId":"x1","receivedAt":"2026-01-01T00:00:00Z","pubkeyHex":"` + r.Header.Get(HeaderPubkey) + `","digestHex":"` + hex.EncodeToString(digest[:]) + `","bytes":"1"}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	svc, err := New(filepath.Join(dir, "collector.json"), Options{Endpoint: server.URL, Enabled: true, Sign: testSigner(t), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	svc.NotifyStored(testRecords[0])
	ctx := context.Background()
	svc.Flush(ctx)
	if got := len(svc.store.Snapshot().Pending); got != 1 {
		t.Fatalf("failed submit must keep pending, got %d", got)
	}
	if svc.store.Snapshot().Inflight == nil {
		t.Fatal("failed submit must keep inflight")
	}
	svc.RetryNow(ctx)
	doc := svc.store.Snapshot()
	if doc.Inflight != nil || len(doc.Pending) != 0 {
		t.Fatalf("retry must confirm: %+v", doc.Pending)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatal("retry must reuse exact gzip bytes")
	}
	if doc.Meta.LastSuccessAt == nil {
		t.Fatal("missing lastSuccessAt")
	}
	// 重启恢复：新实例读同一状态，发送中批次已清理。
	svc2, err := New(filepath.Join(dir, "collector.json"), Options{Endpoint: server.URL, Enabled: true, Sign: testSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	if svc2.store.Snapshot().Inflight != nil {
		t.Fatal("unexpected inflight after clean shutdown")
	}
	// 状态落盘 0600。
	info, err := os.Stat(filepath.Join(dir, "collector.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file perm = %o", info.Mode().Perm())
	}
}
