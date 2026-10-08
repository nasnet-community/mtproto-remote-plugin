// Package ctrl is the control-plane RPC that rides on mux streams: one request
// and one response per stream, both as length-prefixed JSON. A method that
// moves bulk bytes (an agent update) follows the request with a raw body until
// the caller's FIN.
package ctrl

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxMessage bounds one JSON envelope so a hostile peer cannot make us buffer
// without limit.
const MaxMessage = 1 << 20

var ErrTooLarge = errors.New("ctrl: message too large")

type Request struct {
	Method string          `json:"m"`
	Body   json.RawMessage `json:"b,omitempty"`
}

type Response struct {
	OK   bool            `json:"ok"`
	Code string          `json:"code,omitempty"` // short machine-readable failure
	Err  string          `json:"err,omitempty"`
	Body json.RawMessage `json:"b,omitempty"`
}

// Error is what Call returns for a failed response.
type Error struct{ Code, Msg string }

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	}
	return e.Msg
}

func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxMessage {
		return ErrTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func ReadMsg(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > MaxMessage {
		return ErrTooLarge
	}
	// grow as bytes arrive: a declared size alone costs nothing
	var b bytes.Buffer
	if _, err := b.ReadFrom(io.LimitReader(r, int64(size))); err != nil {
		return err
	}
	if b.Len() < int(size) {
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(b.Bytes(), v)
}

// Stream is the slice of a mux stream the RPC needs.
type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error
	SetDeadline(time.Time) error
}

// Call sends one request and decodes the response into resp (may be nil).
func Call(ctx context.Context, st Stream, method string, req, resp any) error {
	return call(ctx, st, method, req, nil, resp)
}

// CallRaw is Call plus a raw body streamed after the request header.
func CallRaw(ctx context.Context, st Stream, method string, req any, body io.Reader, resp any) error {
	return call(ctx, st, method, req, body, resp)
}

func call(ctx context.Context, st Stream, method string, req any, body io.Reader, resp any) error {
	if dl, ok := ctx.Deadline(); ok {
		st.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { st.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	r := Request{Method: method}
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return err
		}
		r.Body = b
	}
	if err := WriteMsg(st, r); err != nil {
		return wrapCtx(ctx, err)
	}
	if body != nil {
		if _, err := io.Copy(st, body); err != nil {
			return wrapCtx(ctx, err)
		}
	}
	if err := st.CloseWrite(); err != nil {
		return wrapCtx(ctx, err)
	}
	var out Response
	if err := ReadMsg(st, &out); err != nil {
		return wrapCtx(ctx, err)
	}
	if !out.OK {
		return &Error{Code: out.Code, Msg: out.Err}
	}
	if resp != nil && len(out.Body) > 0 {
		return json.Unmarshal(out.Body, resp)
	}
	return nil
}

func wrapCtx(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Handler serves one method. raw is the bytes after the request header (empty
// for plain calls). Returning *Error sets the response code.
type Handler func(ctx context.Context, body json.RawMessage, raw io.Reader) (any, error)

// Bounds on swallowing a raw body nobody read; an agent update is at most 64 MiB.
const (
	maxDrain     = 64 << 20
	drainTimeout = 2 * time.Minute
)

// Serve reads one request from st, runs its handler and writes the response.
func Serve(ctx context.Context, st Stream, handlers map[string]Handler) {
	defer st.Close()
	var req Request
	if err := ReadMsg(st, &req); err != nil {
		return
	}
	WriteMsg(st, run(ctx, req, st, handlers))
	// the caller reads only after sending its body; closing on unread bytes would reset the reply away
	_ = st.CloseWrite()
	_ = st.SetDeadline(time.Now().Add(drainTimeout))
	_, _ = io.CopyN(io.Discard, st, maxDrain)
}

func run(ctx context.Context, req Request, raw io.Reader, handlers map[string]Handler) Response {
	h, ok := handlers[req.Method]
	if !ok {
		return Response{Code: "unknown_method", Err: "unknown method " + req.Method}
	}
	out, err := h(ctx, req.Body, raw)
	if err != nil {
		var ce *Error
		if errors.As(err, &ce) {
			return Response{Code: ce.Code, Err: ce.Msg}
		}
		return Response{Code: "internal", Err: err.Error()}
	}
	resp := Response{OK: true}
	if out != nil {
		b, err := json.Marshal(out)
		if err != nil {
			return Response{Code: "internal", Err: err.Error()}
		}
		resp.Body = b
	}
	return resp
}
