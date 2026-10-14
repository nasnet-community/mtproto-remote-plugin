package link

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
)

// The link token never crosses the wire: each side proves it by MAC over both
// nonces. The roles are part of the MAC so one side's proof cannot be replayed
// back to it.
func mac(token []byte, role string, n1, n2 []byte, agentID string) []byte {
	h := hmac.New(sha256.New, token)
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write(n1)
	h.Write(n2)
	h.Write([]byte(agentID))
	return h.Sum(nil)
}

// tokenID names a token without giving it away: the hub says which one it holds.
func tokenID(token []byte) []byte {
	h := hmac.New(sha256.New, token)
	h.Write([]byte("token id"))
	return h.Sum(nil)[:8]
}

func nonce() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func macEqual(a, b []byte) bool { return hmac.Equal(a, b) && !bytes.Equal(a, nil) }
