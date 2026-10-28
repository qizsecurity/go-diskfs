package squashfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCompressorXz_DecompressBCJx86 decodes an xz stream whose filter chain is
// BCJ x86 + LZMA2, the form mksquashfs -comp xz -Xbcj x86 stores a data block
// in (unsquashfs -s: "Filters selected: x86"). ulikunitz/xz rejects such a
// stream with "xz: unsupported filter count".
//
// The input is built from code compiled here, not copied from a third-party
// binary: hello is `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath
// -ldflags="-s -w -buildid="` (go1.27.2) of a one-line fmt.Println program, an
// ELF x86-64 of 1507452 bytes, sha256
// 646e608a6ebd032e264129fe3cdb5df24fbd36053cab39125e6fdd27ea61b769.
// testdata/xz-bcj/hello-amd64-4096.bin is its 4096 bytes at offset 0x16000
// (90112), the 4 KiB window with the most 0xE8 CALL opcodes (200), and the .xz
// is `xz --format=xz --x86 --lzma2=dict=131072 --check=crc32 -k -c` of those
// bytes (xz 5.8.3): `xz -lvv` shows the filters --x86 --lzma2=dict=128KiB.
func TestCompressorXz_DecompressBCJx86(t *testing.T) {
	dir := filepath.Join("testdata", "xz-bcj")
	plain, err := os.ReadFile(filepath.Join(dir, "hello-amd64-4096.bin"))
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := os.ReadFile(filepath.Join(dir, "hello-amd64-4096.bin.xz"))
	if err != nil {
		t.Fatal(err)
	}
	// block header at offset 12: size byte 0x04, flags 0xc1 = two filters
	if len(compressed) < 14 || compressed[13]&3 != 1 {
		t.Fatalf("testdata is not a two-filter xz block (header %x)", compressed[12:14])
	}

	got, err := (&CompressorXz{}).decompress(compressed, len(plain))
	if err != nil {
		t.Fatalf("decompress BCJ x86 + LZMA2 block: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decompressed %d bytes differ from the %d plaintext bytes", len(got), len(plain))
	}
}
