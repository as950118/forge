package tool

import (
	"crypto/sha256"
	"encoding/hex"
)

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
