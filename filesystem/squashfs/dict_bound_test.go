package squashfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// The dictionary a decoder allocates is the one its stream declares, so the caller decides, before the decoder allocates it, whether it fits:
// the decoder reads the declared size (xz: the LZMA2 filter properties of the block header, xz file format 3.1.3 and 5.3.4 "dictionary size
// = (2 | (bits & 1)) << (bits / 2 + 11)"; LZMA alone: the 5 properties bytes, props u8 + dictionary size u32), and asks the DictionaryLimit.
// The kernel allocates one dictionary of dict_size and refuses a larger one (Linux fs/squashfs/xz_wrapper.c:69-70 default
// max(block_size, SQUASHFS_METADATA_SIZE), :93 xz_dec_init(XZ_PREALLOC, dict_size)); here the caller states what it accepts.
// A block_size above SQUASHFS_FILE_MAX_SIZE (1 MiB) is refused at the superblock as the kernel does (super.c:260).

type limitLog struct {
	declared []int64
	open     int
	max      int64 // refuse a dictionary above it
}

func (l *limitLog) limit() DictionaryLimit {
	return func(declared int64) (func(), error) {
		l.declared = append(l.declared, declared)
		if declared > l.max {
			return nil, errors.New("does not fit")
		}
		l.open++
		return func() { l.open-- }, nil
	}
}

// withDictLimit sets an accept-all limit on a compressor a test makes by hand; it returns c.
func withDictLimit(c Compressor, _ uint32) Compressor {
	if dl, ok := c.(dictionaryLimited); ok {
		dl.setDictionaryLimit(func(int64) (func(), error) { return func() {}, nil })
	}
	return c
}

func decodeWith(c Compressor, l *limitLog, in []byte, maxOut int) (out []byte, err error, alloc uint64) {
	c.(dictionaryLimited).setDictionaryLimit(l.limit())
	alloc = allocDuring(func() { out, err = c.decompress(in, maxOut) })
	return
}

func TestLzmaDeclaredDictionaryIsAskedBeforeItIsAllocated(t *testing.T) {
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
	}{{1 << 12, true}, {1 << 20, true}, {1<<20 + 1, false}, {8 << 20, false}, {1<<31 - 1, false}} {
		x := append([]byte(nil), lb.Bytes()...)
		binary.LittleEndian.PutUint32(x[1:], tt.dict) // props u8, dictionary size u32, size u64
		l := &limitLog{max: 1 << 20}
		out, err, alloc := decodeWith(&CompressorLzma{}, l, x, 8192)
		if (err == nil) != tt.ok || (tt.ok && !bytes.Equal(out, plain)) {
			t.Errorf("dict %d: err=%v ok=%v", tt.dict, err, tt.ok)
		}
		if len(l.declared) != 1 || l.declared[0] != int64(tt.dict) {
			t.Errorf("dict %d: the limit was asked %v", tt.dict, l.declared)
		}
		if l.open != 0 {
			t.Errorf("dict %d: %d dictionaries still charged after the decode", tt.dict, l.open)
		}
		var de *DictionaryError
		if !tt.ok && (!errors.As(err, &de) || de.Declared != int64(tt.dict)) {
			t.Errorf("dict %d: error %v is not a DictionaryError carrying the declared size", tt.dict, err)
		}
		if !tt.ok && alloc > 4<<20 {
			t.Errorf("dict %d: refused, yet allocated %d", tt.dict, alloc)
		}
	}
}

func TestXzDeclaredDictionaryIsAskedBeforeItIsAllocated(t *testing.T) {
	plain := bytes.Repeat([]byte("qiz review "), 700)[:7700]
	for _, tt := range []struct {
		dict int
		ok   bool
	}{{1 << 12, true}, {1 << 17, true}, {1 << 20, true}, {1 << 21, false}, {8 << 20, false}, {64 << 20, false}} {
		var xb bytes.Buffer
		xw, err := xz.WriterConfig{DictCap: tt.dict}.NewWriter(&xb)
		if err != nil {
			t.Fatal(err)
		}
		xw.Write(plain)
		xw.Close()
		l := &limitLog{max: 1 << 20}
		out, err, alloc := decodeWith(&CompressorXz{}, l, xb.Bytes(), 8192)
		if (err == nil) != tt.ok || (tt.ok && !bytes.Equal(out, plain)) {
			t.Errorf("dict %d: err=%v ok=%v", tt.dict, err, tt.ok)
		}
		if len(l.declared) != 1 || l.declared[0] != int64(tt.dict) {
			t.Errorf("dict %d: the limit was asked %v", tt.dict, l.declared)
		}
		if l.open != 0 {
			t.Errorf("dict %d: %d dictionaries still charged after the decode", tt.dict, l.open)
		}
		var de *DictionaryError
		if !tt.ok && (!errors.As(err, &de) || de.Declared != int64(tt.dict)) {
			t.Errorf("dict %d: error %v is not a DictionaryError carrying the declared size", tt.dict, err)
		}
		if !tt.ok && alloc > 4<<20 {
			t.Errorf("dict %d: refused, yet allocated %d", tt.dict, alloc)
		}
	}
}

// The filter chain of a block with the x86 BCJ filter (mksquashfs -Xbcj x86) ends in LZMA2: its dictionary is the declared one (128 KiB here).
func TestXzDeclaredDictionaryOfABCJBlock(t *testing.T) {
	compressed, err := os.ReadFile(filepath.Join("testdata", "xz-bcj", "hello-amd64-4096.bin.xz"))
	if err != nil {
		t.Fatal(err)
	}
	l := &limitLog{max: 1 << 20}
	if _, err, _ := decodeWith(&CompressorXz{}, l, compressed, 4096); err != nil {
		t.Fatal(err)
	}
	if len(l.declared) != 1 || l.declared[0] != 131072 {
		t.Errorf("declared %v, want [131072] (xz -lvv: --lzma2=dict=128KiB)", l.declared)
	}
}

func TestDecoderWithoutALimitRefusesToRun(t *testing.T) {
	for name, c := range map[string]Compressor{"xz": &CompressorXz{}, "lzma": &CompressorLzma{}} {
		if _, err := c.decompress([]byte("x"), 8192); err == nil {
			t.Errorf("%s ran with no dictionary limit", name)
		}
	}
}

func TestBlockSizeAboveTheKernelMaximumIsRefusedAtTheSuperblock(t *testing.T) {
	for _, bs := range []uint32{1 << 12, 1 << 17, 1 << 20, 1 << 21, 1 << 30} {
		b, err := os.ReadFile(filepath.Join("testdata", "tree.xz.sqs"))
		if err != nil {
			t.Fatal(err)
		}
		log := uint16(0)
		for v := bs; v > 1; v >>= 1 {
			log++
		}
		binary.LittleEndian.PutUint32(b[12:], bs)
		binary.LittleEndian.PutUint16(b[22:], log)
		_, err = parseSuperblock(b[:96])
		if (err == nil) != (bs <= 1<<20) {
			t.Errorf("block_size %d: err=%v", bs, err)
		}
	}
}

func readImageWith(t *testing.T, b []byte, l DictionaryLimit) (*FileSystem, error) {
	p := filepath.Join(t.TempDir(), "x.sqs")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return Read(file.New(f, true), int64(len(b)), 0, 0, WithDictionaryLimit(l))
}

// A real mksquashfs xz image (alpine 3.20 squashfs-tools, testdata/tree.xz.sqs, 128 KiB blocks) reads, and the limit was asked for each block.
func TestReadRealXzImage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "tree.xz.sqs"))
	if err != nil {
		t.Fatal(err)
	}
	l := &limitLog{max: 1 << 20}
	fs, err := readImageWith(t, b, l.limit())
	if err != nil {
		t.Fatal(err)
	}
	// the paths and digests are those of the fixture's oracle (qiz-sbr handlers/testdata/squashfs/tree.xz.sqsh.oracle.txt)
	for p, want := range map[string]string{
		"a/small1.txt":  "323138f0a90f330f0f85b251c1b5e02c93633b1d8a339a1839b0000c04e1feb7", // a fragment
		"a/b/c/big.bin": "fac590c320b8577b488299754229ba60d824cb1528a72890d307a773c739ae52",  // data blocks
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
	if len(l.declared) == 0 || l.open != 0 {
		t.Errorf("limit asked %d times, %d still open", len(l.declared), l.open)
	}
	// a limit that refuses: Read fails with the typed error
	_, err = readImageWith(t, b, func(int64) (func(), error) { return nil, errors.New("no room") })
	var de *DictionaryError
	if !errors.As(err, &de) || de.Declared <= 0 {
		t.Errorf("a refusing limit: err=%v", err)
	}
	// no limit at all: the decoder refuses to run
	if _, err = readImageWith(t, b, nil); err == nil {
		t.Errorf("Read of an xz image with no dictionary limit succeeded")
	}
}

// Images go-diskfs writes itself: xz (its default dictionary, above the block size) and lzma. The product read these before the dictionary was capped
// at the block size (review round 3).
func TestReadAnImageTheWriterMade(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Compressor
		bs   int64
	}{
		{"xz-default", &CompressorXz{}, 128 << 10},
		{"xz-dict=bs", &CompressorXz{DictionarySize: 128 << 10}, 128 << 10},
		{"lzma", &CompressorLzma{}, 128 << 10},
		{"lzma-1MiB-blocks", &CompressorLzma{}, 1 << 20},
	} {
		p := filepath.Join(t.TempDir(), "w.sqs")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := Create(file.New(f, false), 0, 0, tc.bs)
		if err != nil {
			t.Fatal(err)
		}
		data := bytes.Repeat([]byte("qiz review writer "), 20000)
		w, err := fs.OpenFile("a.txt", os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		if err := fs.Finalize(FinalizeOptions{Compression: tc.c}); err != nil {
			t.Fatalf("%s finalize: %v", tc.name, err)
		}
		f.Close()
		raw, _ := os.ReadFile(p)
		l := &limitLog{max: 64 << 20}
		rfs, err := readImageWith(t, raw, l.limit())
		if err != nil {
			t.Errorf("%s: Read: %v (limit asked %v)", tc.name, err, l.declared)
			continue
		}
		fh, err := rfs.OpenFile("a.txt", os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(fh)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: a.txt read err=%v equal=%v", tc.name, err, bytes.Equal(got, data))
		}
		t.Logf("%s: dictionaries declared: %v", tc.name, l.declared[:min(len(l.declared), 3)])
	}
}
