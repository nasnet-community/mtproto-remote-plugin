package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

func sha256Sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func itoa(n int) string         { return strconv.Itoa(n) }
