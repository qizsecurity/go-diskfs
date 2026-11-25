package squashfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// The decoder's dictionary is bounded by the file system, not by the stream that declares it.
// xz: Linux fs/squashfs/xz_wrapper.c (v6.12) squashfs_xz_comp_opts, lines 58-69: dict_size is the compressor option, else
// max(block_size, SQUASHFS_METADATA_SIZE); squashfs_xz_init, line 93: xz_dec_init(XZ_PREALLOC, dict_size), which refuses a
// stream with a larger dictionary. squashfs-tools xz_wrapper.c:199-209 refuses -Xdict-size above block_size.
// lzma: not in the mainline kernel; squashfs-tools lzma_xz_wrapper.c:49 sets opt.dict_size = block_size for every block it writes
// (data blocks with the file system's block size, metadata blocks with SQUASHFS_METADATA_SIZE). The cap is the same as xz's.

// withDictLimit sets the bound a file system sets at Read, on a compressor a test makes by hand; it returns c.
func withDictLimit(c Compressor, n uint32) Compressor {
	if dl, ok := c.(dictLimited); ok {
		dl.setDictLimit(n)
	}
	return c
}

func dictAlloc(c Compressor, in []byte, maxOut int) (out []byte, err error, alloc uint64) {
	alloc = allocDuring(func() { out, err = c.decompress(in, maxOut) })
	return
}

func TestDictionaryOfAnLzmaBlockIsCapped(t *testing.T) {
	plain := bytes.Repeat([]byte("qiz review "), 700)[:7700]
	var lb bytes.Buffer
	lw, err := lzma.WriterConfig{DictCap: 1 << 12}.NewWriter(&lb)
	if err != nil {
		t.Fatal(err)
	}
	lw.Write(plain)
	lw.Close()
	for _, tt := range []struct {
		dict uint32
		ok   bool
	}{{1 << 12, true}, {1 << 17, true}, {1<<17 + 1, false}, {8 << 20, false}, {1<<31 - 1, false}} {
		x := append([]byte(nil), lb.Bytes()...)
		binary.LittleEndian.PutUint32(x[1:], tt.dict) // LZMA alone header: props u8, dictionary size u32, size u64
		c := &CompressorLzma{}
		c.setDictLimit(1 << 17)
		out, err, alloc := dictAlloc(c, x, 8192)
		if (err == nil) != tt.ok || (tt.ok && !bytes.Equal(out, plain)) {
			t.Errorf("dict %d: err=%v ok=%v", tt.dict, err, tt.ok)
		}
		if alloc > 4<<20 {
			t.Errorf("dict %d: allocated %d", tt.dict, alloc)
		}
	}
}

func TestDictionaryOfAnXzBlockIsCapped(t *testing.T) {
	plain := bytes.Repeat([]byte("qiz review "), 700)[:7700]
	for _, tt := range []struct {
		dict int
		ok   bool
	}{{1 << 12, true}, {1 << 17, true}, {1 << 18, false}, {8 << 20, false}, {64 << 20, false}} {
		var xb bytes.Buffer
		xw, err := xz.WriterConfig{DictCap: tt.dict}.NewWriter(&xb)
		if err != nil {
			t.Fatal(err)
		}
		xw.Write(plain)
		xw.Close()
		c := &CompressorXz{}
		c.setDictLimit(1 << 17)
		out, err, alloc := dictAlloc(c, xb.Bytes(), 8192)
		if (err == nil) != tt.ok || (tt.ok && !bytes.Equal(out, plain)) {
			t.Errorf("dict %d: err=%v ok=%v", tt.dict, err, tt.ok)
		}
		if alloc > 4<<20 {
			t.Errorf("dict %d: allocated %d", tt.dict, alloc)
		}
	}
}

func TestDictLimitIsTheKernelsDefault(t *testing.T) {
	// max(block_size, 8192)
	for bs, want := range map[int64]uint32{4096: 8192, 8192: 8192, 131072: 131072, 1 << 20: 1 << 20} {
		if got := dictLimitFor(bs); got != want {
			t.Errorf("block size %d: %d want %d", bs, got, want)
		}
	}
}

// A real mksquashfs xz image (alpine 3.20 squashfs-tools, testdata/tree.xz.sqs, 128 KiB blocks) still reads, its dictionary within the cap.
func TestReadRealXzImageWithTheCap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "tree.xz.sqs"))
	if err != nil {
		t.Fatal(err)
	}
	fs, err := readImage(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.compressor.(*CompressorXz); !ok {
		t.Fatalf("compressor %T", fs.compressor)
	}
	if fs.compressor.(*CompressorXz).dictLimit != 1<<17 {
		t.Errorf("dict limit %d", fs.compressor.(*CompressorXz).dictLimit)
	}
	// the paths and digests are those of the fixture's oracle (qiz-sbr handlers/testdata/squashfs/tree.xz.sqsh.oracle.txt)
	for p, want := range map[string]string{
		"a/small1.txt":  "323138f0a90f330f0f85b251c1b5e02c93633b1d8a339a1839b0000c04e1feb7", // a fragment
		"a/b/c/big.bin": "fac590c320b8577b488299754229ba60d824cb1528a72890d307a773c739ae52", // data blocks
	} {
		f, err := fs.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f)
		if sum := sha256.Sum256(got); err != nil || hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s: sha256 %x, err %v", p, sum, err)
		}
	}
}
