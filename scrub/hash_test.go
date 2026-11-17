package scrub

import (
	"crypto/sha256"
	"encoding/hex"
)

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
