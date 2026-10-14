package drill

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// Protocol tags at init[56:60].
const (
	tagAbridged     uint32 = 0xefefefef
	tagIntermediate uint32 = 0xeeeeeeee
	tagPadded       uint32 = 0xdddddddd
)

// o2Conn is one obfuscated2 connection: AES-256-CTR each way.
type o2Conn struct {
	net.Conn
	enc, dec cipher.Stream
	Tag      uint32
	DC       int16
}

func (c *o2Conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.dec.XORKeyStream(p[:n], p[:n])
	return n, err
}

func (c *o2Conn) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	c.enc.XORKeyStream(b, p)
	return c.Conn.Write(b)
}

func ctr(key, iv []byte, secret []byte) cipher.Stream {
	if secret != nil {
		h := sha256.Sum256(append(append([]byte(nil), key...), secret...))
		key = h[:]
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return cipher.NewCTR(blk, iv)
}

func reversed(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// badStart is what the first bytes of an init must not look like, so a proxy
// never mistakes it for HTTP or another transport.
func badStart(b []byte) bool {
	if b[0] == 0xef {
		return true
	}
	first := binary.LittleEndian.Uint32(b[0:4])
	for _, s := range []string{"HEAD", "POST", "GET ", "OPTI", "\x16\x03\x01\x02"} {
		if string(b[0:4]) == s {
			return true
		}
	}
	if first == tagPadded || first == tagIntermediate {
		return true
	}
	return binary.LittleEndian.Uint32(b[4:8]) == 0
}

// o2Dial runs the client half over c: a 64-byte init in which bytes 56-63 are
// sent encrypted, then CTR streams that carry on for the whole connection.
func o2Dial(c net.Conn, tag uint32, dc int16, secret []byte) (*o2Conn, error) {
	init := make([]byte, 64)
	for {
		if _, err := rand.Read(init); err != nil {
			return nil, err
		}
		if !badStart(init) {
			break
		}
	}
	binary.LittleEndian.PutUint32(init[56:60], tag)
	binary.LittleEndian.PutUint16(init[60:62], uint16(dc))
	rev := reversed(init[8:56])
	enc := ctr(init[8:40], init[40:56], secret)
	dec := ctr(rev[0:32], rev[32:48], secret)
	sealed := make([]byte, 64)
	enc.XORKeyStream(sealed, init)
	wire := append(append([]byte(nil), init[:56]...), sealed[56:]...)
	if _, err := c.Write(wire); err != nil {
		return nil, err
	}
	return &o2Conn{Conn: c, enc: enc, dec: dec, Tag: tag, DC: dc}, nil
}

var errBadTag = errors.New("obfuscated2: unknown protocol tag")

// o2Accept runs the receiving half: the proxy side with the user's secret, or
// a datacentre (secret nil), which is what telemt dials in direct mode.
func o2Accept(c net.Conn, secret []byte) (*o2Conn, error) {
	init := make([]byte, 64)
	if _, err := io.ReadFull(c, init); err != nil {
		return nil, err
	}
	rev := reversed(init[8:56])
	dec := ctr(init[8:40], init[40:56], secret)
	enc := ctr(rev[0:32], rev[32:48], secret)
	plain := make([]byte, 64)
	dec.XORKeyStream(plain, init)
	tag := binary.LittleEndian.Uint32(plain[56:60])
	if tag != tagAbridged && tag != tagIntermediate && tag != tagPadded {
		return nil, fmt.Errorf("%w %08x", errBadTag, tag)
	}
	return &o2Conn{Conn: c, enc: enc, dec: dec, Tag: tag, DC: int16(binary.LittleEndian.Uint16(plain[60:62]))}, nil
}

// writeFrame and readFrame speak the intermediate transport: a 4-byte little
// endian length, then the payload.
func writeFrame(w io.Writer, p []byte) error {
	b := make([]byte, 4+len(p))
	binary.LittleEndian.PutUint32(b, uint32(len(p)))
	copy(b[4:], p)
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader, max int) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(h[:]) &^ 0x80000000) // top bit is the quick-ack flag
	if n > max {
		return nil, fmt.Errorf("frame of %d bytes", n)
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return p, err
}

// The two halves must agree with each other before they are worth pointing
// at telemt; the drill is what proves they agree with telemt.
func TestObfuscated2HalvesAgree(t *testing.T) {
	for _, secret := range [][]byte{nil, bytes.Repeat([]byte{0x5a}, 16)} {
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() {
			s, err := o2Accept(b, secret)
			if err != nil {
				done <- err
				return
			}
			if s.Tag != tagIntermediate || s.DC != -2 {
				done <- fmt.Errorf("server saw tag %08x dc %d", s.Tag, s.DC)
				return
			}
			p, err := readFrame(s, 1<<16)
			if err == nil {
				err = writeFrame(s, p)
			}
			done <- err
		}()
		c, err := o2Dial(a, tagIntermediate, -2, secret)
		if err != nil {
			t.Fatal(err)
		}
		a.SetDeadline(time.Now().Add(5 * time.Second))
		msg := bytes.Repeat([]byte("ping"), 16)
		if err := writeFrame(c, msg); err != nil {
			t.Fatal(err)
		}
		got, err := readFrame(c, 1<<16)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("echo %q %v", got, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		a.Close()
		b.Close()
	}
}

// A wrong secret must not decode to a valid tag (except by a 3-in-2^32 fluke).
func TestObfuscated2WrongSecretIsRefused(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	errc := make(chan error, 1)
	go func() { _, err := o2Accept(b, bytes.Repeat([]byte{1}, 16)); errc <- err }()
	if _, err := o2Dial(a, tagIntermediate, 2, bytes.Repeat([]byte{2}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, errBadTag) {
		t.Fatalf("err = %v", err)
	}
}
