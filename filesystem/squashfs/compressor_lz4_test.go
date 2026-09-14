package squashfs

import (
	"bytes"
	"testing"

	lz4 "github.com/pierrec/lz4/v4"
)

// TestCompressorLz4_DecompressRawBlock proves CompressorLz4.decompress now
// decodes the raw LZ4 BLOCK format SquashFS 4.0 actually stores on disk
// (liblz4's block API -- the same format the Linux kernel's
// fs/squashfs/lz4_wrapper.c produces by calling LZ4_decompress_safe
// directly, and squashfs-tools/lz4_wrapper.c produces by calling
// LZ4_compress_fast/LZ4_compress_HC via its COMPRESS()/COMPRESS_HC() macros
// -- see compressor.go's decompress doc comment for both citations).
//
// The fixture is built with lz4.CompressBlock, the SAME block-level API
// mksquashfs itself calls into (via cgo/liblz4 upstream, pierrec/lz4/v4 here
// is a pure-Go reimplementation of the identical wire format) -- not
// captured from a scanned repo, so this is a format-contract test, not an
// overfit to one corpus file.
func TestCompressorLz4_DecompressRawBlock(t *testing.T) {
	plain := testCompressUncompressed // shared fixture from compressor_internal_test.go

	bound := lz4.CompressBlockBound(len(plain))
	dst := make([]byte, bound)
	var ht [1 << 16]int
	n, err := lz4.CompressBlock(plain, dst, ht[:])
	if err != nil {
		t.Fatalf("lz4.CompressBlock: %v", err)
	}
	if n == 0 {
		// incompressible input: CompressBlock returns n=0, meaning "store
		// the input as-is" per the pierrec/lz4/v4 CompressBlock contract.
		t.Fatalf("lz4.CompressBlock returned n=0 (incompressible) -- fixture needs compressible input")
	}
	raw := dst[:n]

	c := CompressorLz4{}
	out, err := c.decompress(raw, len(plain))
	if err != nil {
		t.Fatalf("decompress raw LZ4 block: %v", err)
	}
	if !bytes.Equal(out, plain) {
		t.Errorf("decompressed mismatch:\ngot:  % x\nwant: % x", out, plain)
	}
}

// TestCompressorLz4_DecompressRawBlock_MaxOutRequired proves the maxOut
// guard clause: raw LZ4 blocks carry no embedded output-length header (unlike
// the frame format), so a maxOut <= 0 can never be satisfied honestly --
// decompress must refuse rather than guess a buffer size.
func TestCompressorLz4_DecompressRawBlock_MaxOutRequired(t *testing.T) {
	c := CompressorLz4{}
	if _, err := c.decompress([]byte{0x00}, 0); err == nil {
		t.Errorf("decompress with maxOut=0: want error, got nil")
	}
}

// TestCompressorLz4_OldFrameReaderFailsOnRawBlock is the PINNED regression
// test for the bug this fix closes: the OLD decompress implementation
// (lz4.NewReader, the LZ4 FRAME format's streaming reader, which requires
// the 4-byte magic number 0x184D2204 at the start of input) can NEVER
// decode a real mksquashfs -comp lz4 block, on ANY instance -- because
// SquashFS never writes that magic number; it writes raw LZ4 BLOCK data.
// This test decodes the SAME raw-block bytes TestCompressorLz4_
// DecompressRawBlock proves decompress() now handles correctly, through the
// OLD frame-reader code path, and asserts it fails with the frame format's
// own "bad magic number" error -- the exact failure this fix's commit
// message and the qiz-sbr consumer's now-removed Unsupported routing both
// quote verbatim. If this test ever fails (frame reader stops rejecting raw
// block data), the bug this fix targets has changed shape and needs
// re-investigation, not a quiet test deletion.
func TestCompressorLz4_OldFrameReaderFailsOnRawBlock(t *testing.T) {
	plain := testCompressUncompressed

	bound := lz4.CompressBlockBound(len(plain))
	dst := make([]byte, bound)
	var ht [1 << 16]int
	n, err := lz4.CompressBlock(plain, dst, ht[:])
	if err != nil {
		t.Fatalf("lz4.CompressBlock: %v", err)
	}
	raw := dst[:n]

	frameReader := lz4.NewReader(bytes.NewReader(raw))
	buf := make([]byte, len(plain))
	_, err = frameReader.Read(buf)
	if err == nil {
		t.Fatalf("OLD frame-reader path unexpectedly succeeded decoding a raw LZ4 block -- the bug this fix targets may no longer reproduce; re-investigate before trusting this test suite")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("bad magic number")) {
		t.Errorf("frame reader error = %q, want it to mention \"bad magic number\" (the specific upstream frame-vs-block mismatch this fix addresses)", err.Error())
	}
}
