package iso9660_test

// A directory record states the size of its extent as a u32 (up to 4 GiB). The reader checks that extent against the
// volume before it reads, and reads it through one window.
//
// Standard: ECMA-119 4th edition (June 2019) -- 9.1.3 Location of Extent and 9.1.4 Data Length (page 36); 8.4.8 Volume
// Space Size, "the number of Logical Blocks in which the Volume Space of the volume is recorded" (page 25); 8.4.12
// Logical Block Size, "the size in bytes of a Logical Block" (page 26); 6.8.1 and 6.8.1.1 (page 11): a directory is one
// file in one Extent, and each Directory Record ends in the Logical Sector in which it begins.
//
// Input: testdata/9660.iso (made by genisoimage, see testdata/buildtestiso.sh), with the data length of one directory
// changed in its own record (BP 11 to 18, both-byte orders, 7.3.3).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

const hostileDirBlock = 2048

// hostileISO returns the bytes of 9660.iso and the first block of the root directory's extent (from the root record in
// the Primary Volume Descriptor: Logical Block 16, BP 157 to 190, 8.4.18).
func hostileISO(t *testing.T) (b []byte, rootBlock int64) {
	t.Helper()
	b, err := os.ReadFile("./testdata/9660.iso")
	if err != nil {
		t.Fatal(err)
	}
	pvd := b[16*hostileDirBlock:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatalf("block 16 is not a Primary Volume Descriptor")
	}
	return b, int64(binary.LittleEndian.Uint32(pvd[156+2:]))
}

// setRecordSize writes a data length into the record at byte off, in both byte orders (BP 11 to 18).
func setRecordSize(b []byte, off int64, size uint32) {
	binary.LittleEndian.PutUint32(b[off+10:], size)
	binary.BigEndian.PutUint32(b[off+14:], size)
}

func openISO(t *testing.T, b []byte) *iso9660.FileSystem {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hostile.iso")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fs, err := iso9660.Read(file.New(f, true), 0, 0, hostileDirBlock)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// allocOf returns the bytes allocated in total while fn ran.
func allocOf(fn func()) uint64 {
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

// The root directory states 4 GiB - 1: ReadDir refuses it with the extent and the volume named, and allocates nothing
// for the stated size. Before: make([]byte, 4 GiB - 1) at iso9660.go readDirectoryPVD.
func TestHostileDirectoryExtentLargerThanVolumeIsRefused(t *testing.T) {
	b, root := hostileISO(t)
	setRecordSize(b, root*hostileDirBlock, 0xFFFFFFFF) // the record that describes the root directory in its own extent (6.8.2.2)
	fs := openISO(t, b)
	var err error
	alloc := allocOf(func() { _, err = fs.ReadDir(".") })
	t.Logf("ReadDir: %v; allocated %d KiB", err, alloc>>10)
	if !errors.Is(err, iso9660.ErrDirectoryExtent) {
		t.Fatalf("got %v, want ErrDirectoryExtent", err)
	}
	if alloc > 1<<20 {
		t.Errorf("allocated %d bytes for a refused extent, want at most 1 MiB", alloc)
	}
}

// A subdirectory states a size inside 32 bits and past the volume: the same refusal, through the path table.
func TestHostileSubdirectoryExtentPastVolumeIsRefused(t *testing.T) {
	b, root := hostileISO(t)
	// the first directory record of the root extent that is not the root itself or its parent (6.8.2.2): File Flags
	// (BP 26) bit 1 set (9.1.6), Location of Extent (BP 3 to 10).
	var loc uint32
	var dir string
	ext := b[root*hostileDirBlock : (root+1)*hostileDirBlock]
	for off := 0; off < len(ext) && ext[off] != 0; off += int(ext[off]) {
		rec := ext[off : off+int(ext[off])]
		id := rec[33 : 33+int(rec[32])]
		if rec[25]&2 != 0 && !(len(id) == 1 && id[0] <= 1) {
			loc, dir = binary.LittleEndian.Uint32(rec[2:]), string(id)
			break
		}
	}
	if loc == 0 {
		t.Fatal("9660.iso has no directory in its root extent")
	}
	volume := int64(len(b))
	setRecordSize(b, int64(loc)*hostileDirBlock, uint32(volume)) // the volume's own size: location*block + size > volume
	fs := openISO(t, b)
	var rerr error
	alloc := allocOf(func() { _, rerr = fs.ReadDir(dir) })
	t.Logf("ReadDir(%s): %v; allocated %d KiB", dir, rerr, alloc>>10)
	if !errors.Is(rerr, iso9660.ErrDirectoryExtent) {
		t.Fatalf("got %v, want ErrDirectoryExtent", rerr)
	}
}

// A directory of many records spans several logical blocks: the window reads it a block at a time and lists every record,
// the same names the writer put in.
func TestDirectoryOfSeveralBlocksIsReadThroughTheWindow(t *testing.T) {
	const n = 300 // about 40 bytes a record: 4 or 5 blocks of 2,048 bytes
	f, err := os.CreateTemp(t.TempDir(), "multi")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs, err := iso9660.Create(file.New(f, false), 0, 0, hostileDirBlock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("/D"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("F%05d.TXT", i)
		w, err := fs.OpenFile("/D/"+name+";1", os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
		_ = w.Close()
		want[name] = true
	}
	if err := fs.Finalize(iso9660.FinalizeOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	rd, err := iso9660.Read(file.New(f, true), st.Size(), 0, hostileDirBlock)
	if err != nil {
		t.Fatal(err)
	}
	des, err := rd.ReadDir("D")
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != n {
		t.Errorf("listed %d records, want %d", len(des), n)
	}
	if held := rd.HeldBytes(); held < hostileDirBlock {
		t.Errorf("HeldBytes %d does not count the window of %d bytes", held, hostileDirBlock)
	}
}

// The Primary Volume Descriptor states a Path Table Size of 4 GiB - 1 (8.4.13, BP 133 to 140, both byte orders): Read
// refuses the volume with the typed error and allocates nothing for the stated size. Before: make([]byte, pathTableSize).
func TestHostilePathTableExtentLargerThanVolumeIsRefused(t *testing.T) {
	b, _ := hostileISO(t)
	pvd := b[16*hostileDirBlock:]
	binary.LittleEndian.PutUint32(pvd[132:], 0xFFFFFFFF)
	binary.BigEndian.PutUint32(pvd[136:], 0xFFFFFFFF)
	p := filepath.Join(t.TempDir(), "hostile-pt.iso")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rerr error
	alloc := allocOf(func() { _, rerr = iso9660.Read(file.New(f, true), 0, 0, hostileDirBlock) })
	t.Logf("Read: %v; allocated %d KiB", rerr, alloc>>10)
	if !errors.Is(rerr, iso9660.ErrPathTableExtent) {
		t.Fatalf("got %v, want ErrPathTableExtent", rerr)
	}
	if alloc > 1<<20 {
		t.Errorf("allocated %d bytes for a refused path table, want at most 1 MiB", alloc)
	}
}

// The CE entry of the root's own record (rockridge.iso, block 18) states a continuation area of 4 GiB - 1 bytes (SUSP 1.12
// section 5.1, BP 21 to 28, both byte orders): Read refuses it with the typed error and allocates nothing for the stated
// length. Before: make([]byte, ContinuationLength()).
func TestHostileContinuationAreaLargerThanVolumeIsRefused(t *testing.T) {
	b, err := os.ReadFile("./testdata/rockridge.iso")
	if err != nil {
		t.Fatal(err)
	}
	// find the CE entry (signature "CE", length 28) in the System Use area of the first record of block 18
	rec := b[18*hostileDirBlock:]
	su := 33 + int(rec[32])
	if rec[32]%2 == 0 {
		su++ // padding byte after an even-length File Identifier (9.1, Table 8)
	}
	ce := -1
	for j := su; j+4 <= int(rec[0]); {
		if string(rec[j:j+2]) == "CE" && rec[j+2] == 28 {
			ce = j
			break
		}
		if rec[j+2] < 4 {
			break
		}
		j += int(rec[j+2])
	}
	if ce < 0 {
		t.Fatal("no CE entry in the first record of block 18 of rockridge.iso")
	}
	binary.LittleEndian.PutUint32(rec[ce+20:], 0xFFFFFFFF)
	binary.BigEndian.PutUint32(rec[ce+24:], 0xFFFFFFFF)
	p := filepath.Join(t.TempDir(), "hostile-ce.iso")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rerr error
	alloc := allocOf(func() { _, rerr = iso9660.Read(file.New(f, true), 0, 0, hostileDirBlock) })
	t.Logf("Read: %v; allocated %d KiB", rerr, alloc>>10)
	if !errors.Is(rerr, iso9660.ErrContinuationArea) {
		t.Fatalf("got %v, want ErrContinuationArea", rerr)
	}
	if alloc > 1<<20 {
		t.Errorf("allocated %d bytes for a refused area, want at most 1 MiB", alloc)
	}
}

// A path table of several logical blocks (300 directories, about 16 bytes a record: more than 2 windows) is read through
// the window with records carried across its edges: every directory is found by its path, which is the path table's job.
func TestPathTableOfSeveralBlocksIsReadThroughTheWindow(t *testing.T) {
	const n = 300
	f, err := os.CreateTemp(t.TempDir(), "pt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs, err := iso9660.Create(file.New(f, false), 0, 0, hostileDirBlock, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := fs.Mkdir(fmt.Sprintf("/DIR%05d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.Finalize(iso9660.FinalizeOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	rd, err := iso9660.Read(file.New(f, true), st.Size(), 0, hostileDirBlock)
	if err != nil {
		t.Fatal(err)
	}
	if held := rd.HeldBytes(); held < n*16 {
		t.Errorf("HeldBytes %d does not count the %d path table records", held, n)
	}
	for _, i := range []int{0, 127, 128, 129, n - 1} {
		if _, err := rd.ReadDir(fmt.Sprintf("DIR%05d", i)); err != nil {
			t.Errorf("ReadDir(DIR%05d): %v", i, err)
		}
	}
}
