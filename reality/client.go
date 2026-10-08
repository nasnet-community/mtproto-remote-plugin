// Package reality is the client half of REALITY, ported from xray-core's
// transport/internet/reality (MPL-2.0); the server half is github.com/xtls/reality,
// wrapped in package realityserver. This file stays under MPL-2.0.
package reality

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Version bytes ride in SessionId[0:3]; the server only checks them when it sets Min/MaxClientVer.
var version = [3]byte{25, 9, 11}

type ClientConfig struct {
	ServerName  string  // SNI; must be one of the server's ServerNames
	PublicKey   []byte  // server x25519 public key (32 bytes)
	ShortID     [8]byte // one of the server's ShortIds
	Fingerprint utls.ClientHelloID
}

var ErrNotVerified = errors.New("reality: server did not prove it holds the key (real certificate seen)")

// Client runs the REALITY handshake over c and returns the TLS connection.
func Client(ctx context.Context, c net.Conn, cfg ClientConfig) (*utls.UConn, error) {
	var authKey []byte
	verified := false

	utlsCfg := &utls.Config{
		ServerName:             cfg.ServerName,
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("reality: no certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			if pub, ok := cert.PublicKey.(ed25519.PublicKey); ok {
				h := hmac.New(sha512.New, authKey)
				h.Write(pub)
				if bytes.Equal(h.Sum(nil), cert.Signature) {
					verified = true
				}
			}
			return nil
		},
	}
	id := cfg.Fingerprint
	if id.Client == "" {
		id = utls.HelloChrome_Auto
	}
	u := utls.UClient(c, utlsCfg, id)
	if err := u.BuildHandshakeState(); err != nil {
		return nil, err
	}
	hello := u.HandshakeState.Hello
	hello.SessionId = make([]byte, 32)
	copy(hello.Raw[39:], hello.SessionId) // session id sits at a fixed offset
	copy(hello.SessionId[0:3], version[:])
	binary.BigEndian.PutUint32(hello.SessionId[4:], uint32(time.Now().Unix()))
	copy(hello.SessionId[8:], cfg.ShortID[:])

	pub, err := ecdh.X25519().NewPublicKey(cfg.PublicKey)
	if err != nil {
		return nil, errors.New("reality: bad public key")
	}
	ecdhe := u.HandshakeState.State13.KeyShareKeys.Ecdhe
	if ecdhe == nil {
		ecdhe = u.HandshakeState.State13.KeyShareKeys.MlkemEcdhe
	}
	if ecdhe == nil {
		return nil, errors.New("reality: fingerprint has no TLS 1.3 key share")
	}
	shared, err := ecdhe.ECDH(pub)
	if err != nil {
		return nil, err
	}
	authKey, err = hkdf.Key(sha256.New, shared, hello.Random[:20], "REALITY", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(authKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	aead.Seal(hello.SessionId[:0], hello.Random[20:], hello.SessionId[:16], hello.Raw)
	copy(hello.Raw[39:], hello.SessionId)

	if err := u.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if !verified {
		u.Close()
		return nil, ErrNotVerified
	}
	return u, nil
}

// GenerateKey makes an x25519 keypair for a REALITY server.
func GenerateKey() (priv, pub []byte, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return k.Bytes(), k.PublicKey().Bytes(), nil
}

// PublicFromPrivate derives the public half.
func PublicFromPrivate(priv []byte) ([]byte, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}
