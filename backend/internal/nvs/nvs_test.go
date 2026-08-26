package nvs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden fixtures in testdata/ are the whole point of this file. internal/nvs is a
// from-scratch reimplementation of a binary format that a device's ability to boot depends on, so
// it is held to byte-for-byte equality with the tool it replaces — ESP-IDF's own generator —
// rather than to a test that merely agrees with the implementation's idea of the format.
//
// Fixtures contain only dummy credentials. Regenerate them with ESP-IDF exported (adjust the
// python path if your IDF version differs):
//
//	cd backend/internal/nvs/testdata
//	python -m esp_idf_nvs_partition_gen generate provision.csv provision.bin 0x6000
//	for n in 1 31 32 33 63 64 200; do
//	  printf 'key,type,encoding,value\nprov,namespace,,\nk,data,string,%s\n' \
//	    "$(python3 -c "print('x'*$n)")" > /tmp/strlen-$n.csv
//	  python -m esp_idf_nvs_partition_gen generate /tmp/strlen-$n.csv strlen-$n.bin 0x6000
//	done
//
// Generated with esp_idf_nvs_partition_gen from ESP-IDF 5.5 (format V2, multipage blob support).

const testPartitionSize = 0x6000 // matches the nvs partition in firmware/partitions.csv

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// diffAt reports the first differing offset, so a failure points at the field that broke rather
// than dumping 24 KB of hex.
func diffAt(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// TestBuildMatchesGeneratorForProvisioning is the parity gate for the exact image the wizard
// ships: the four provisioning keys the firmware reads, in the `prov` namespace, at the real
// partition size.
func TestBuildMatchesGeneratorForProvisioning(t *testing.T) {
	got, err := Build(testPartitionSize, "prov", []KV{
		{"device_id", "rt-example"},
		{"token", "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJKKK"},
		{"wifi_ssid", "Example_Fleet_WiFi"},
		{"wifi_psk", "dummy-psk-value"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := golden(t, "provision.bin")
	if i := diffAt(got, want); i != -1 {
		t.Fatalf("image differs from nvs_partition_gen.py at offset 0x%x: got 0x%02x, want 0x%02x",
			i, got[i], want[i])
	}
}

// TestBuildMatchesGeneratorAtEntryBoundaries covers the lengths where the 32-byte entry packing
// changes shape: values that exactly fill an entry, that spill one byte past it, and that span
// several. The NUL terminator counts toward the length, so len 31 is the last single-entry value
// and len 32 is the first that needs two.
func TestBuildMatchesGeneratorAtEntryBoundaries(t *testing.T) {
	for _, n := range []int{1, 31, 32, 33, 63, 64, 200} {
		name := "strlen-" + itoa(n)
		t.Run(name, func(t *testing.T) {
			got, err := Build(testPartitionSize, "prov", []KV{{"k", strings.Repeat("x", n)}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			want := golden(t, name+".bin")
			if i := diffAt(got, want); i != -1 {
				t.Fatalf("value length %d differs at offset 0x%x: got 0x%02x, want 0x%02x",
					n, i, got[i], want[i])
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestBuildErasesUnusedSpace(t *testing.T) {
	img, err := Build(testPartitionSize, "prov", []KV{{"device_id", "rt-1"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(img) != testPartitionSize {
		t.Fatalf("image is %d bytes, want %d", len(img), testPartitionSize)
	}
	// Everything past the first page must look like freshly-erased flash, or the NVS driver would
	// try to interpret whatever we left there as further pages.
	tail := img[PageSize:]
	if want := bytes.Repeat([]byte{0xFF}, len(tail)); !bytes.Equal(tail, want) {
		t.Errorf("bytes past the first page are not all 0xFF")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		ns      string
		entries []KV
	}{
		{"size not a page multiple", 0x6001, "prov", nil},
		{"size zero", 0, "prov", nil},
		{"size below the reserved-page minimum", PageSize, "prov", nil},
		{"empty namespace", testPartitionSize, "", nil},
		{"namespace too long", testPartitionSize, "aaaaaaaaaaaaaaaaa", nil},
		{"empty key", testPartitionSize, "prov", []KV{{"", "v"}}},
		{"key too long", testPartitionSize, "prov", []KV{{"aaaaaaaaaaaaaaaaa", "v"}}},
		{"duplicate key", testPartitionSize, "prov", []KV{{"k", "a"}, {"k", "b"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.size, tc.ns, tc.entries); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// TestBuildRejectsOverfullPage guards the one silent-corruption risk in a single-page builder:
// running past the 126 entry slots must be an error, never a wrapped or truncated write.
func TestBuildRejectsOverfullPage(t *testing.T) {
	var entries []KV
	for i := 0; i < 130; i++ {
		entries = append(entries, KV{Key: "k" + itoa(i), Value: "v"})
	}
	if _, err := Build(testPartitionSize, "prov", entries); err == nil {
		t.Fatal("expected a page-full error, got nil")
	}
}
