package utxo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

var txidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckTxid validates a display-order txid.
func CheckTxid(txid string) (string, error) {
	if !txidPattern.MatchString(txid) {
		return "", fail(ErrTxInvalid, "invalid txid")
	}
	return txid, nil
}

// Sha256 computes SHA-256, matching the public TS helper.
func Sha256(data []byte) []byte {
	value := sha256.Sum256(data)
	return value[:]
}

// BytesToHex returns canonical lowercase hex.
func BytesToHex(data []byte) string { return hex.EncodeToString(data) }

// Dsha256 computes double SHA-256.
func Dsha256(data []byte) []byte {
	first := sha256.Sum256(data)
	second := sha256.Sum256(first[:])
	return second[:]
}

// CalcTxid returns the canonical display-order txid of raw tx bytes.
func CalcTxid(raw []byte) string {
	hash := Dsha256(raw)
	reversed := make([]byte, 32)
	for i := 0; i < 32; i++ {
		reversed[i] = hash[31-i]
	}
	return hex.EncodeToString(reversed)
}

func checkHexEven(value string) (string, error) {
	clean := value
	if len(clean) >= 2 && (clean[:2] == "0x" || clean[:2] == "0X") {
		clean = clean[2:]
	}
	if len(clean)%2 != 0 {
		return "", fmt.Errorf("invalid hex")
	}
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", fmt.Errorf("invalid hex")
		}
	}
	return clean, nil
}

// HexToBytes decodes hex (optional 0x prefix).
func HexToBytes(value string) ([]byte, error) {
	clean, err := checkHexEven(value)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(clean)
}
