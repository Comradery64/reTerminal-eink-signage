// Package render turns a room Schedule into a Spectra 6 framebuffer and an MDPF wire payload.
//
// This file mirrors firmware/main/protocol.hpp byte-for-byte. Keep them in lockstep.
package render

import (
	"encoding/binary"
	"hash/crc32"
)

const (
	MDPFVersion    = 1
	headerSize     = 32
	flagCompressed = 1 << 0
	flagFullRefr   = 1 << 1
)

var mdpfMagic = [4]byte{'M', 'D', 'P', 'F'}

// Payload is the fully encoded wire blob plus metadata the HTTP layer needs.
type Payload struct {
	Bytes      []byte // header + body, ready to write to the socket
	CRC32      uint32 // content hash of the *uncompressed* framebuffer == ETag
	NextWakeS  uint32
	Compressed bool
}

// Encode builds an MDPF payload from a packed 4bpp framebuffer (width*height/2 bytes).
// It always PackBits-compresses, but falls back to raw if compression would inflate.
func Encode(packed []byte, width, height int, nextWakeS uint32, fullRefresh bool) Payload {
	crc := crc32.ChecksumIEEE(packed)

	body := PackBits(packed)
	compressed := true
	if len(body) >= len(packed) {
		body = packed
		compressed = false
	}

	flags := uint16(0)
	if compressed {
		flags |= flagCompressed
	}
	if fullRefresh {
		flags |= flagFullRefr
	}

	buf := make([]byte, headerSize+len(body))
	copy(buf[0:4], mdpfMagic[:])
	binary.LittleEndian.PutUint16(buf[4:], MDPFVersion)
	binary.LittleEndian.PutUint16(buf[6:], flags)
	binary.LittleEndian.PutUint16(buf[8:], uint16(width))
	binary.LittleEndian.PutUint16(buf[10:], uint16(height))
	buf[12] = 4 // bpp
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(body)))
	binary.LittleEndian.PutUint32(buf[20:], uint32(len(packed)))
	binary.LittleEndian.PutUint32(buf[24:], nextWakeS)
	binary.LittleEndian.PutUint32(buf[28:], crc)
	copy(buf[headerSize:], body)

	return Payload{Bytes: buf, CRC32: crc, NextWakeS: nextWakeS, Compressed: compressed}
}
