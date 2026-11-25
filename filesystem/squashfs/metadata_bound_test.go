package squashfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

// The SquashFS 4.0 format bounds a metadata block to SQUASHFS_METADATA_SIZE (8192) decoded bytes:
// Linux fs/squashfs/squashfs_fs.h:19 (#define SQUASHFS_METADATA_SIZE 8192); fs/squashfs/super.c:320 sizes the
// metadata cache entries with it; fs/squashfs/block.c:324 (squashfs_read_data) refuses a block whose on-disk length
// is <= 0 or > output->length (8192 for metadata) with -EIO. squashfs_fs.h:108 SQUASHFS_COMPRESSED_SIZE maps a
// header size field of 0 to 0x8000, so a size of 0 is also refused. These tests hold the reader to that.

const kernelMetadataSize = 8192

func allocDuring(fn func()) uint64 {
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

func TestDecompressRefusesMoreThanMaxOut(t *testing.T) {
	plain := make([]byte, 64<<20) // 64 MiB of zeros: a few hundred bytes compressed
	exact := make([]byte, kernelMetadataSize)
	over := make([]byte, kernelMetadataSize+1)
	for name, c := range map[string]Compressor{
		"gzip": &CompressorGzip{CompressionLevel: 9},
		"lzma": &CompressorLzma{},
		"xz":   &CompressorXz{},
		"zstd": &CompressorZstd{},
	} {
		t.Run(name, func(t *testing.T) {
			bomb, err := c.compress(plain)
			if err != nil {
				t.Fatal(err)
			}
			var out []byte
			var derr error
			alloc := allocDuring(func() { out, derr = c.decompress(bomb, kernelMetadataSize) })
			if derr == nil {
				t.Errorf("a %d byte block that decodes to %d bytes was accepted with maxOut %d (returned %d bytes)", len(bomb), len(plain), kernelMetadataSize, len(out))
			}
			// not the 64 MiB of output: what remains is each decoder's own window, which its stream header declares
			// (8 MiB here, the writer's default) and which maxOut does not bound.
			if alloc > 16<<20 {
				t.Errorf("decompress allocated %d bytes for maxOut %d", alloc, kernelMetadataSize)
			}
			// exactly maxOut is valid; maxOut+1 is not
			okBlock, err := c.compress(exact)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := c.decompress(okBlock, kernelMetadataSize); err != nil || !bytes.Equal(got, exact) {
				t.Errorf("a block of exactly maxOut bytes: err=%v len=%d", err, len(got))
			}
			overBlock, err := c.compress(over)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.decompress(overBlock, kernelMetadataSize); err == nil {
				t.Errorf("a block of maxOut+1 bytes was accepted")
			}
		})
	}
}

func TestDecompressRefusesNonPositiveMaxOut(t *testing.T) {
	blk, err := (&CompressorGzip{CompressionLevel: 9}).compress([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&CompressorGzip{}).decompress(blk, 0); err == nil {
		t.Errorf("maxOut 0 accepted")
	}
}

func TestReadMetaBlockKernelLimits(t *testing.T) {
	bomb, err := (&CompressorGzip{CompressionLevel: 9}).compress(make([]byte, 8<<20))
	if err != nil || len(bomb) > 0x7fff {
		t.Fatalf("bomb %d err %v", len(bomb), err)
	}
	hdr := func(size int, stored bool) []byte {
		v := uint16(size)
		if stored {
			v |= 0x8000
		}
		return binary.LittleEndian.AppendUint16(nil, v)
	}
	tests := []struct {
		name string
		in   []byte
	}{
		{"stored size 8193", append(hdr(8193, true), make([]byte, 8193)...)},
		{"stored size 0 (kernel: 0x8000)", hdr(0, true)},
		{"compressed size 0", append(hdr(0, false), 0, 0)},
		{"compressed size 8193", append(hdr(8193, false), make([]byte, 8193)...)},
		{"compressed 8 MiB bomb in a valid 8192 or less size", append(hdr(len(bomb), false), bomb...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &FileSystem{}
			var err error
			alloc := allocDuring(func() { _, _, err = fs.readMetaBlock(bytes.NewReader(tt.in), &CompressorGzip{}, 0) })
			if err == nil {
				t.Errorf("accepted")
			}
			if alloc > 1<<20 {
				t.Errorf("allocated %d bytes", alloc)
			}
		})
	}
	// a valid stored block of exactly 8192 bytes is still read
	in := append(hdr(8192, true), make([]byte, 8192)...)
	if b, size, err := (&FileSystem{}).readMetaBlock(bytes.NewReader(in), nil, 0); err != nil || len(b) != 8192 || size != 8194 {
		t.Errorf("valid 8192 stored block: len %d size %d err %v", len(b), size, err)
	}
}

// testdata/dir_read.sqs is a real mksquashfs image with an xattr table (300 ids): xattr_id_table_start 3287,
// xattr_table_start 2683, first xattr id block 2889 (read from its bytes: header at 3287 is <start u64, ids u32, unused u32>,
// the index follows).
func readFixture(t *testing.T) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "dir_read.sqs"))
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint64(b[56:]) != 3287 || binary.LittleEndian.Uint64(b[3287:]) != 2683 ||
		binary.LittleEndian.Uint32(b[3295:]) != 300 || binary.LittleEndian.Uint64(b[3303:]) != 2889 {
		t.Fatalf("fixture is not the image this test was written against")
	}
	return b
}

func readImage(t *testing.T, b []byte) (*FileSystem, error) {
	p := filepath.Join(t.TempDir(), "x.sqs")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Read(file.New(f, true), int64(len(b)), 0, 0)
}

func TestReadXattrTableOfARealImage(t *testing.T) {
	fs, err := readImage(t, readFixture(t))
	if err != nil || fs.xattrs == nil || len(fs.xattrs.list) != 300 {
		t.Fatalf("valid image: err=%v", err)
	}
}

// Linux fs/squashfs/xattr_id.c squashfs_read_xattr_id_table (lines ~79-124): the index entries rise, each gap is at most
// SQUASHFS_METADATA_SIZE + SQUASHFS_BLOCK_OFFSET; the last id block lies before xattr_id_table_start by at most that;
// xattr_table_start is before the first id block. A crafted image that puts xattr data blocks past xattr_id_table_start
// (go-diskfs read them up to the first id block, wherever it was) is refused up front.
func TestReadXattrTableBlocksPastTheIDTable(t *testing.T) {
	base := readFixture(t)
	for len(base)%8 != 0 {
		base = append(base, 0)
	}
	P := int64(len(base))
	const nblk = 4096
	b := append([]byte{}, base...)
	b = append(b, binary.LittleEndian.AppendUint16(nil, 0x8000|26)...) // stored block of 26 bytes: header(16) + index(8) + 2
	x := int64(len(b))
	h := make([]byte, 26)
	binary.LittleEndian.PutUint64(h[0:], uint64(P))
	binary.LittleEndian.PutUint32(h[8:], 1)
	b = append(b, h...)
	blk := append(binary.LittleEndian.AppendUint16(nil, 0x8000|8192), make([]byte, 8192)...)
	for i := 0; i < nblk; i++ {
		b = append(b, blk...)
	}
	locn := int64(len(b))
	b = append(b, binary.LittleEndian.AppendUint16(nil, 0x8000|16)...)
	b = append(b, make([]byte, 16)...)
	binary.LittleEndian.PutUint64(b[x+16:], uint64(locn))
	binary.LittleEndian.PutUint64(b[56:], uint64(x))
	var err error
	alloc := allocDuring(func() { _, err = readImage(t, b) })
	if err == nil || !strings.Contains(err.Error(), "xattr") {
		t.Errorf("image with xattr data past xattr_id_table_start: err=%v", err)
	}
	if alloc > 4<<20 {
		t.Errorf("Read allocated %d bytes on a %d byte image", alloc, len(b))
	}
}

func TestReadXattrTableIndexRules(t *testing.T) {
	// xattr_table_start >= first id block
	b := readFixture(t)
	binary.LittleEndian.PutUint64(b[3287:], 2889)
	if _, err := readImage(t, b); err == nil {
		t.Errorf("xattr_table_start == first id block accepted")
	}
	// first id block at or after xattr_id_table_start
	b = readFixture(t)
	binary.LittleEndian.PutUint64(b[3303:], 3287)
	if _, err := readImage(t, b); err == nil {
		t.Errorf("last id block == xattr_id_table_start accepted")
	}
}

func TestValidateXattrIndex(t *testing.T) {
	tests := []struct {
		name         string
		locns        []uint64
		dataStart    uint64
		idTableStart uint64
		ok           bool
	}{
		{"one block", []uint64{2889}, 2683, 3287, true},
		{"two blocks, gap exactly 8194", []uint64{100, 8294}, 50, 8294 + 100, true},
		{"gap 8195", []uint64{100, 8295}, 50, 9000, false},
		{"not rising", []uint64{200, 200}, 50, 500, false},
		{"falling", []uint64{300, 200}, 50, 500, false},
		{"last block after the id table", []uint64{100, 600}, 50, 500, false},
		{"last block 8195 before the id table", []uint64{100}, 50, 8295, false},
		{"last block exactly 8194 before", []uint64{100}, 50, 8294, true},
		{"data start at first block", []uint64{100}, 100, 500, false},
		{"data start after first block", []uint64{100}, 101, 500, false},
	}
	for _, tt := range tests {
		err := validateXattrIndex(tt.locns, tt.dataStart, tt.idTableStart)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err=%v want ok=%v", tt.name, err, tt.ok)
		}
	}
}
