// Package nvs builds an ESP-IDF NVS partition image containing a display's provisioning secrets,
// so the /admin "Add a device" wizard can flash a blank unit over WebSerial without the operator
// running tools/provision_device.sh, nvs_partition_gen.py, or esptool by hand.
//
// Why this exists in Go rather than as browser JS or a shelled-out Python call:
//
//   - The fleet Wi-Fi PSK (config.FleetConfig.WifiPSK) is a *shared* secret — one leak
//     compromises every display — so it must never be served to a browser. Building the image
//     here means the browser only ever receives opaque partition bytes destined for one device's
//     flash over USB.
//   - Shelling out to nvs_partition_gen.py would mean adding Python plus the ESP-IDF generator to
//     a container image that is otherwise a single static Go binary.
//
// Scope is deliberately narrow: a handful of short string entries in one namespace, written into
// a single freshly-erased partition. This is NOT a general NVS implementation — no blobs, no
// encryption, no multi-page garbage collection, no reading or updating an existing image. That
// narrowness is what makes a from-scratch reimplementation of a binary format defensible, and
// nvs_test.go holds it to a byte-for-byte comparison against the real generator's output.
//
// Format reference: ESP-IDF's esp_idf_nvs_partition_gen (and nvs_page.hpp/nvs_item_hpp). A page
// is 4096 bytes: a 32-byte header, a 32-byte entry-state bitmap, then 126 32-byte entries. A
// string occupies one header entry (which carries the key, length, and a CRC of the value) plus
// however many trailing entries hold the NUL-terminated value.
package nvs

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

const (
	// PageSize and EntrySize are fixed by the on-flash format, not tunable.
	PageSize  = 4096
	EntrySize = 32

	entriesPerPage    = 126
	bitmapOffset      = 32
	bitmapSize        = 32
	firstEntryOffset  = bitmapOffset + bitmapSize
	maxKeyLen         = 15 // 16-byte field, always NUL-terminated
	pageStateActive   = 0xFFFFFFFE
	pageVersion2      = 0xFE
	typeUint8         = 0x01
	typeString        = 0x21
	chunkAny          = 0xFF
	nsIndexNamespaces = 0 // entries describing namespaces themselves live under index 0
)

// crcInit matches the generator's zlib.crc32(data, 0xFFFFFFFF) — note the non-standard initial
// value, which is why this can't just be crc32.ChecksumIEEE.
const crcInit = 0xFFFFFFFF

func crc(b []byte) uint32 { return crc32.Update(crcInit, crc32.IEEETable, b) }

// KV is one string entry to store. Only strings are supported; every provisioning secret the
// firmware reads (see firmware/main/nvs_store.cpp) is one.
type KV struct {
	Key   string
	Value string
}

// Build returns a complete NVS partition image of size bytes, holding entries under a single
// namespace. size must be a multiple of PageSize and at least 2 pages (the format reserves the
// last page, so a 1-page partition can hold nothing).
//
// Everything beyond the written entries is 0xFF — identical to a freshly-erased flash region,
// which is exactly what the NVS driver expects to find on first boot.
func Build(size int, namespace string, entries []KV) ([]byte, error) {
	if size <= 0 || size%PageSize != 0 {
		return nil, fmt.Errorf("nvs: size %d must be a positive multiple of %d", size, PageSize)
	}
	if size < 2*PageSize {
		return nil, fmt.Errorf("nvs: size %d is too small; the format reserves a page, so at least %d is required", size, 2*PageSize)
	}
	if err := checkKey(namespace); err != nil {
		return nil, fmt.Errorf("nvs: namespace: %w", err)
	}

	img := make([]byte, size)
	for i := range img {
		img[i] = 0xFF
	}

	p := &page{buf: img[:PageSize]}
	p.writeHeader(0)

	// The namespace is itself an entry: a u8 under namespace index 0 whose key is the namespace
	// name and whose value is the index every subsequent entry references. Omitting it would
	// produce an image the firmware's nvs_open("prov", ...) can't find anything in.
	const nsIndex = 1
	if err := p.writePrimitive(nsIndexNamespaces, namespace, nsIndex); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if err := checkKey(e.Key); err != nil {
			return nil, fmt.Errorf("nvs: key %q: %w", e.Key, err)
		}
		if seen[e.Key] {
			return nil, fmt.Errorf("nvs: duplicate key %q", e.Key)
		}
		seen[e.Key] = true
		if err := p.writeString(nsIndex, e.Key, e.Value); err != nil {
			return nil, fmt.Errorf("nvs: key %q: %w", e.Key, err)
		}
	}
	return img, nil
}

func checkKey(k string) error {
	switch {
	case k == "":
		return fmt.Errorf("must not be empty")
	case len(k) > maxKeyLen:
		return fmt.Errorf("is %d bytes; the limit is %d", len(k), maxKeyLen)
	}
	return nil
}

// page writes entries into one 4096-byte page, tracking how many entry slots are consumed so the
// state bitmap and the next write offset stay in step.
type page struct {
	buf   []byte
	count int // entry slots used so far
}

// writeHeader lays down the page header: state, sequence number, format version, and a CRC over
// the header's own bytes [4,28). Everything else stays 0xFF.
func (p *page) writeHeader(seq uint32) {
	binary.LittleEndian.PutUint32(p.buf[0:], pageStateActive)
	binary.LittleEndian.PutUint32(p.buf[4:], seq)
	p.buf[8] = pageVersion2
	binary.LittleEndian.PutUint32(p.buf[28:], crc(p.buf[4:28]))
}

// reserve marks n consecutive entry slots as written in the state bitmap (2 bits per slot;
// 0b11 = empty, and clearing the low bit marks it written) and returns the offset of the first.
func (p *page) reserve(n int) (int, error) {
	if p.count+n > entriesPerPage {
		return 0, fmt.Errorf("nvs: page full: %d entries used, %d more requested, %d per page",
			p.count, n, entriesPerPage)
	}
	off := firstEntryOffset + p.count*EntrySize
	for i := 0; i < n; i++ {
		bit := (p.count + i) * 2
		p.buf[bitmapOffset+bit/8] &^= 1 << (bit % 8)
	}
	p.count += n
	return off, nil
}

// newEntry returns a 32-byte entry header pre-filled with the fields every type shares. The
// caller fills in the type byte, span, and the 8-byte data field, then calls sealEntry.
func newEntry(nsIndex byte, key string) []byte {
	e := make([]byte, EntrySize)
	for i := range e {
		e[i] = 0xFF
	}
	e[0] = nsIndex
	e[3] = chunkAny
	// The key field is zero-padded, not 0xFF-padded, and the generator relies on that padding
	// being part of the entry CRC.
	for i := 8; i < 24; i++ {
		e[i] = 0
	}
	copy(e[8:24], key)
	return e
}

// sealEntry computes the entry's own CRC, which covers bytes [0,4) and [8,32) — i.e. everything
// but the CRC field itself.
func sealEntry(e []byte) {
	crcData := make([]byte, 0, 28)
	crcData = append(crcData, e[0:4]...)
	crcData = append(crcData, e[8:32]...)
	binary.LittleEndian.PutUint32(e[4:], crc(crcData))
}

// writePrimitive writes a single-entry u8 item. Only used for the namespace descriptor.
func (p *page) writePrimitive(nsIndex byte, key string, val byte) error {
	e := newEntry(nsIndex, key)
	e[1] = typeUint8
	e[2] = 1 // span
	e[24] = val

	sealEntry(e)
	off, err := p.reserve(1)
	if err != nil {
		return err
	}
	copy(p.buf[off:], e)
	return nil
}

// writeString writes a NUL-terminated string as a header entry plus the entries holding the
// value. The header's data field is {uint16 length, uint16 reserved, uint32 CRC of the value};
// the length counts the NUL terminator, and the value is padded to a 32-byte boundary with 0xFF.
func (p *page) writeString(nsIndex byte, key, value string) error {
	data := append([]byte(value), 0)
	dataEntries := (len(data) + EntrySize - 1) / EntrySize

	e := newEntry(nsIndex, key)
	e[1] = typeString
	e[2] = byte(dataEntries + 1) // span covers the header entry plus the value entries
	binary.LittleEndian.PutUint16(e[24:], uint16(len(data)))
	binary.LittleEndian.PutUint32(e[28:], crc(data))

	sealEntry(e)
	off, err := p.reserve(1 + dataEntries)
	if err != nil {
		return err
	}
	copy(p.buf[off:], e)
	copy(p.buf[off+EntrySize:], data)
	return nil
}
