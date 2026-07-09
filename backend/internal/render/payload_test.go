package render

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"testing"
)

// referenceDecode mirrors firmware mdpf::packbits_decode (protocol.hpp).
func referenceDecode(comp []byte, rawLen int) []byte {
	out := make([]byte, 0, rawLen)
	i := 0
	for i < len(comp) && len(out) < rawLen {
		ctrl := int8(comp[i])
		i++
		switch {
		case ctrl >= 0:
			count := int(ctrl) + 1
			for k := 0; k < count && i < len(comp) && len(out) < rawLen; k++ {
				out = append(out, comp[i])
				i++
			}
		case ctrl != -128:
			count := 1 - int(ctrl)
			if i >= len(comp) {
				return out
			}
			v := comp[i]
			i++
			for k := 0; k < count && len(out) < rawLen; k++ {
				out = append(out, v)
			}
		}
	}
	return out
}

func TestPackBitsRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	mkFrame := func(n int) []byte {
		b := make([]byte, 0, n)
		for len(b) < n {
			if r.Float64() < 0.8 {
				v := []byte{0x11, 0xFF, 0x00}[r.Intn(3)]
				run := r.Intn(296) + 5
				for k := 0; k < run && len(b) < n; k++ {
					b = append(b, v)
				}
			} else {
				for k := 0; k < r.Intn(10)+1 && len(b) < n; k++ {
					b = append(b, byte(r.Intn(256)))
				}
			}
		}
		return b
	}
	for i := 0; i < 200; i++ {
		src := mkFrame(192000)
		if got := referenceDecode(PackBits(src), len(src)); !bytes.Equal(got, src) {
			t.Fatalf("frame %d round-trip mismatch", i)
		}
	}
	// edge cases
	for _, src := range [][]byte{{}, {0}, bytes.Repeat([]byte{0xFF}, 129), {1, 2, 3, 3, 3, 4}} {
		if got := referenceDecode(PackBits(src), len(src)); !bytes.Equal(got, src) {
			t.Fatalf("edge round-trip mismatch for %v", src)
		}
	}
}

func TestEncodeHeader(t *testing.T) {
	packed := bytes.Repeat([]byte{0x11}, 192000)
	p := Encode(packed, 800, 480, 600, true)
	if string(p.Bytes[0:4]) != "MDPF" {
		t.Fatal("bad magic")
	}
	if got := binary.LittleEndian.Uint32(p.Bytes[28:]); got != crc32.ChecksumIEEE(packed) {
		t.Fatalf("crc mismatch")
	}
	if !p.Compressed {
		t.Fatal("a uniform frame must compress")
	}
	if binary.LittleEndian.Uint32(p.Bytes[20:]) != 192000 {
		t.Fatal("raw_len wrong")
	}
}
