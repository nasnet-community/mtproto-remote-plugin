package ctrl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/mux"
)

// duplex is one end of an in-memory stream with the slice of mux.Stream the RPC uses.
type duplex struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func pipe() (duplex, duplex) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return duplex{ar, aw}, duplex{br, bw}
}

func (d duplex) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) CloseWrite() error           { return d.w.Close() }
func (d duplex) Close() error                { d.w.Close(); return d.r.Close() }

// A deadline in the past unblocks a pending read, which is what ctx cancellation relies on.
func (d duplex) SetDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		d.r.CloseWithError(os.ErrDeadlineExceeded)
	}
	return nil
}

func serve(handlers map[string]Handler) duplex {
	client, server := pipe()
	go Serve(context.Background(), server, handlers)
	return client
}

func TestCallRoundTrip(t *testing.T) {
	st := serve(map[string]Handler{
		"echo": func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var in struct{ N int }
			if err := json.Unmarshal(b, &in); err != nil {
				return nil, err
			}
			return map[string]int{"n": in.N * 2}, nil
		},
	})
	var out struct{ N int }
	if err := Call(context.Background(), st, "echo", map[string]int{"N": 21}, &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 42 {
		t.Fatalf("got %+v", out)
	}
}

func TestHandlerErrorKeepsItsCode(t *testing.T) {
	st := serve(map[string]Handler{
		"nope": func(context.Context, json.RawMessage, io.Reader) (any, error) {
			return nil, &Error{Code: "bad_state", Msg: "a user has no secret"}
		},
	})
	err := Call(context.Background(), st, "nope", nil, nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "bad_state" || ce.Msg != "a user has no secret" {
		t.Fatalf("err = %v", err)
	}
}

func TestPlainErrorsBecomeInternal(t *testing.T) {
	st := serve(map[string]Handler{
		"boom": func(context.Context, json.RawMessage, io.Reader) (any, error) { return nil, errors.New("disk on fire") },
	})
	err := Call(context.Background(), st, "boom", nil, nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "internal" {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownMethod(t *testing.T) {
	st := serve(map[string]Handler{})
	err := Call(context.Background(), st, "missing", nil, nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "unknown_method" {
		t.Fatalf("err = %v", err)
	}
}

// An update is a JSON header followed by raw bytes until the caller's FIN.
func TestCallRawStreamsTheBodyAfterTheHeader(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	st := serve(map[string]Handler{
		"put": func(_ context.Context, b json.RawMessage, raw io.Reader) (any, error) {
			h := sha256.New()
			n, err := io.Copy(h, raw)
			if err != nil {
				return nil, err
			}
			return map[string]any{"n": n, "sha": hex.EncodeToString(h.Sum(nil)), "hdr": string(b)}, nil
		},
	})
	var out struct {
		N   int64
		Sha string
		Hdr string
	}
	if err := CallRaw(context.Background(), st, "put", map[string]string{"kind": "agent"}, bytes.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if out.N != int64(len(body)) || out.Sha != hex.EncodeToString(want[:]) {
		t.Fatalf("handler saw %d bytes, sha %s", out.N, out.Sha)
	}
	if out.Hdr != `{"kind":"agent"}` {
		t.Fatalf("header = %s", out.Hdr)
	}
}

func TestOversizedMessagesAreRefusedBothWays(t *testing.T) {
	if err := WriteMsg(io.Discard, map[string]string{"x": string(make([]byte, MaxMessage))}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write: %v", err)
	}
	var hostile bytes.Buffer
	binary.Write(&hostile, binary.BigEndian, uint32(MaxMessage+1))
	var v any
	if err := ReadMsg(&hostile, &v); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read: %v (a hostile length must not allocate)", err)
	}
}

func TestCancelledContextUnblocksACall(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	st := serve(map[string]Handler{
		"hang": func(context.Context, json.RawMessage, io.Reader) (any, error) { <-block; return nil, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Call(ctx, st, "hang", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call ignored its context")
	}
}

// muxPair returns a client and a server session over an in-memory conn.
func muxPair(t *testing.T) (*mux.Session, *mux.Session) {
	t.Helper()
	a, b := net.Pipe()
	srv := make(chan *mux.Session, 1)
	go func() { s, _ := mux.Server(b, nil); srv <- s }()
	c, err := mux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := <-srv
	if s == nil {
		t.Fatal("server session failed")
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

// Refusing before reading the raw body used to reset the stream, hiding the reason from the caller.
func TestEarlyRejectionReachesARawCaller(t *testing.T) {
	c, s := muxPair(t)
	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			go Serve(context.Background(), st, map[string]Handler{
				"put": func(context.Context, json.RawMessage, io.Reader) (any, error) {
					return nil, &Error{Code: "bad_update", Msg: "size 0"}
				},
			})
		}
	}()
	st, err := c.Open(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = CallRaw(ctx, st, "put", nil, bytes.NewReader(make([]byte, 1<<20)), nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "bad_update" {
		t.Fatalf("err = %v, want the bad_update reply", err)
	}
}

// Declaring a full-size message and sending four bytes must not cost a megabyte.
func TestReadMsgGrowsWithTheBytesThatArrive(t *testing.T) {
	var hostile bytes.Buffer
	binary.Write(&hostile, binary.BigEndian, uint32(MaxMessage))
	hostile.WriteString("{}{}")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var v any
	err := ReadMsg(&hostile, &v)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 128<<10 {
		t.Fatalf("allocated %d bytes for 4 bytes of payload", got)
	}
	// a real message near the limit still decodes
	var msg bytes.Buffer
	big := strings.Repeat("x", MaxMessage-16)
	if err := WriteMsg(&msg, big); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := ReadMsg(&msg, &got); err != nil || got != big {
		t.Fatalf("large message: %d bytes, err %v", len(got), err)
	}
}
