package market

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", nowMillis())
	}
	return hex.EncodeToString(b)
}
