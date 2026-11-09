package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	qrcode "github.com/skip2/go-qrcode"
)

func computeHMAC(privateKey []byte, network, publicKeyHex string) string {
	mac := hmac.New(sha256.New, privateKey)
	_, _ = mac.Write([]byte("keymaster.network-identity.v1:" + network + ":" + publicKeyHex))
	return hex.EncodeToString(mac.Sum(nil))
}

func computeInitAuth(_ []byte, network, publicKeyHex string) string {
	// Placeholder kept for readability: the real binding is computed after the
	// password re-verification in cmdInit.
	return network + ":" + publicKeyHex
}

func write0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func printQR(value string) {
	code, err := qrcode.New(value, qrcode.Low)
	if err != nil {
		return
	}
	bitmap := code.Bitmap()
	for _, row := range bitmap {
		for _, v := range row {
			if v {
				fmt.Print("██")
			} else {
				fmt.Print("  ")
			}
		}
		fmt.Println()
	}
}
