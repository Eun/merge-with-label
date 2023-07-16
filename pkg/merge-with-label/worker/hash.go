package worker

import (
	"crypto/sha512"
	"encoding/hex"
)

func hashForKV(name string) string {
	h := sha512.Sum512([]byte(name))
	return hex.EncodeToString(h[:])
}
