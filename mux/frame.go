package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame types. The wire format is 8 bytes of header followed by `length` bytes:
//
//	+------+-------+-----------+--------+
//	| type | flags | stream id | length |
//	|  u8  |  u8   |    u32    |  u16   |   big endian
//	+------+-------+-----------+--------+
const (
	typeOpen     byte = 1 // payload: metaLen u8 | meta | first data
	typeData     byte = 2 // payload: bytes
	typeCredit   byte = 3 // payload: n u32 (receiver grants n more bytes)
	typeReset    byte = 4 // payload: reason u8 (optional)
	typePing     byte = 5 // payload: 8 opaque bytes, echoed by Pong
	typePong     byte = 6
	typeGoAway   byte = 7 // no new streams; existing ones finish
	typeSettings byte = 8 // payload: see settings
)

// flagFIN on Open/Data: the sender will send nothing more on this stream.
const flagFIN byte = 1

const (
	headerLen  = 8
	maxPayload = 1<<16 - 1
	settingsV  = 1
	// the largest window a peer may announce or grant: keeps credit math inside int32
	maxWindowSetting = 16 << 20
)

// Reset reasons (informational).
const (
	ResetCancel   byte = 0
	ResetRefused  byte = 1 // too many streams / accept queue full
	ResetProtocol byte = 2 // peer broke flow control
)

var (
	ErrProtocol = errors.New("mux: protocol error")
	errBadFrame = fmt.Errorf("%w: malformed frame", ErrProtocol)
)

type header struct {
	typ    byte
	flags  byte
	stream uint32
	length uint16
}

func (h header) put(b []byte) {
	b[0] = h.typ
	b[1] = h.flags
	binary.BigEndian.PutUint32(b[2:6], h.stream)
	binary.BigEndian.PutUint16(b[6:8], h.length)
}

func parseHeader(b []byte) header {
	return header{
		typ:    b[0],
		flags:  b[1],
		stream: binary.BigEndian.Uint32(b[2:6]),
		length: binary.BigEndian.Uint16(b[6:8]),
	}
}

// validate checks the header against the frame-type rules before the payload
// is read, so a hostile length never makes us allocate.
func (h header) validate(maxFrame int) error {
	if int(h.length) > maxFrame && h.typ != typeSettings {
		return fmt.Errorf("%w: frame of %d bytes exceeds %d", ErrProtocol, h.length, maxFrame)
	}
	switch h.typ {
	case typeOpen:
		if h.stream == 0 || h.length < 1 {
			return errBadFrame
		}
	case typeData:
		if h.stream == 0 {
			return errBadFrame
		}
	case typeCredit:
		if h.stream == 0 || h.length != 4 {
			return errBadFrame
		}
	case typeReset:
		if h.stream == 0 || h.length > 1 {
			return errBadFrame
		}
	case typePing, typePong:
		if h.stream != 0 || h.length != 8 {
			return errBadFrame
		}
	case typeGoAway:
		if h.stream != 0 || h.length != 0 {
			return errBadFrame
		}
	case typeSettings:
		if h.stream != 0 || h.length < settingsLen || h.length > 256 {
			return errBadFrame
		}
	default:
		return fmt.Errorf("%w: unknown frame type %d", ErrProtocol, h.typ)
	}
	return nil
}

// settings is the first frame each side sends. The peer's values bound what we
// may send: window is our initial send credit for every new stream.
type settings struct {
	version    byte
	window     uint32 // per-stream receive window the sender guarantees
	maxFrame   uint16
	maxStreams uint32
}

const settingsLen = 1 + 4 + 2 + 4

func (s settings) marshal() []byte {
	b := make([]byte, settingsLen)
	b[0] = s.version
	binary.BigEndian.PutUint32(b[1:5], s.window)
	binary.BigEndian.PutUint16(b[5:7], s.maxFrame)
	binary.BigEndian.PutUint32(b[7:11], s.maxStreams)
	return b
}

func parseSettings(b []byte) (settings, error) {
	if len(b) < settingsLen {
		return settings{}, errBadFrame
	}
	s := settings{
		version:    b[0],
		window:     binary.BigEndian.Uint32(b[1:5]),
		maxFrame:   binary.BigEndian.Uint16(b[5:7]),
		maxStreams: binary.BigEndian.Uint32(b[7:11]),
	}
	if s.version != settingsV {
		return settings{}, fmt.Errorf("%w: peer speaks mux v%d", ErrProtocol, s.version)
	}
	if s.window == 0 || s.maxFrame < 256 {
		return settings{}, errBadFrame
	}
	if s.window > maxWindowSetting {
		return settings{}, fmt.Errorf("%w: peer window %d exceeds %d", ErrProtocol, s.window, maxWindowSetting)
	}
	return s, nil
}
