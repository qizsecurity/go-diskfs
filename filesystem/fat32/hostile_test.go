package fat32_test

// Hostile FAT32 volumes: every case is a real volume made by this package's own writer, with one field of it changed.
//
// Standard: Microsoft "FAT: General Overview of On-Disk Format" (the FAT32 File System Specification, version 1.03,
// 2000-12-06), the BPB and the FAT sections:
//   - BPB_RsvdSecCnt, BPB_NumFATs, BPB_FATSz32, BPB_TotSec32: the reserved area, the FATs and the data region together
//     are the volume (TotSec32 sectors), so a FAT that does not fit in the volume is not a FAT of that volume;
//   - the FAT, "FAT Type Determination": the valid data clusters are 2 .. CountofClusters+1, and a chain of them ends at
//     an end-of-chain (EOC) mark, so a chain that never meets an EOC revisits a cluster, and a chain longer than
//     the cluster count must revisit one;
//   - the directory structure: a directory's entries name other directories by their first cluster; a directory that
//     names itself or an ancestor makes the tree a cycle, which no valid volume holds.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/fat12"
	"github.com/diskfs/go-diskfs/filesystem/fat32"
)

const (
	hostileVolume  = 40 << 20
	hostileSectors = 512
	// A refused or ended hostile case must stay under this many bytes of total allocation: the volume is 40 MiB and
	// its FAT about 80 KiB, and the unfixed readers allocate 192 MiB (a 64 MiB FAT stated) up to 2 GiB and more.
	hostileAllocBound = 8 << 20
	// The unfixed readers do not return; the test stops the process before the machine runs out of memory.
	hostileHeapLimit = 1 << 30
)

// watchHeap ends the process with a message when the heap passes hostileHeapLimit: the unfixed code does not return.
func watchHeap(t *testing.T) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > hostileHeapLimit {
				fmt.Fprintf(os.Stderr, "HOSTILE %s: heap %d MiB passed %d MiB, the reader does not stop\n", t.Name(), m.HeapAlloc>>20, hostileHeapLimit>>20)
				os.Exit(7)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return func() { close(done) }
}

// hostileBase makes a 40 MiB FAT32 volume with /a/b/one.txt and returns its bytes.
func hostileBase(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "base.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "HOSTILE", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("/a/b"); err != nil {
		t.Fatal(err)
	}
	w, err := fs.OpenFile("/a/b/one.txt", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello world, a file of the test\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hostileOpen reads the volume bytes through fat32.Read, as the scanner does (read only, size = the file size).
func hostileOpen(t *testing.T, b []byte) (*fat32.FileSystem, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hostile.img")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return fat32.Read(file.New(f, true), int64(len(b)), 0, hostileSectors)
}

func hostileGeometry(b []byte) (reserved, spf uint32) {
	le := binary.LittleEndian
	return uint32(le.Uint16(b[0x0E:])), le.Uint32(b[0x24:])
}

// allocDuring returns the bytes allocated in total while fn ran, and how long it took.
func allocDuring(fn func()) (alloc uint64, took time.Duration) {
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	fn()
	took = time.Since(start)
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc, took
}

func requireBounded(t *testing.T, alloc uint64, took time.Duration) {
	t.Helper()
	t.Logf("took %v, allocated %d KiB (bound %d KiB, 1 s)", took, alloc>>10, hostileAllocBound>>10)
	if took > time.Second {
		t.Errorf("took %v, want under 1 s", took)
	}
	if alloc > hostileAllocBound {
		t.Errorf("allocated %d bytes, want at most %d", alloc, hostileAllocBound)
	}
}

// BPB_FATSz32 states a FAT larger than the volume: Read refuses it before it allocates the FAT.
func TestHostileFATLargerThanVolumeIsRefused(t *testing.T) {
	defer watchHeap(t)()
	for _, statedMiB := range []uint32{64, 256} {
		t.Run(fmt.Sprintf("FAT %d MiB in a 40 MiB volume", statedMiB), func(t *testing.T) {
			b := hostileBase(t)
			binary.LittleEndian.PutUint32(b[0x24:], statedMiB<<20/hostileSectors)
			var err error
			alloc, took := allocDuring(func() { _, err = hostileOpen(t, b) })
			t.Logf("Read: err=%v", err)
			requireBounded(t, alloc, took)
			if err == nil {
				t.Fatal("Read accepted a FAT larger than the volume")
			}
			if !strings.Contains(err.Error(), "FAT") {
				t.Errorf("error does not name the FAT: %v", err)
			}
		})
	}
}

// BPB_FATSz32 = 0 is no FAT at all (the FAT holds at least entries 0 and 1).
func TestHostileFATSizeZeroIsRefused(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	binary.LittleEndian.PutUint32(b[0x24:], 0)
	var err error
	alloc, took := allocDuring(func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		_, err = hostileOpen(t, b)
	})
	if err == nil || strings.HasPrefix(err.Error(), "panic") {
		t.Fatalf("want a refusal, got %v", err)
	}
	requireBounded(t, alloc, took)
}

func setBothFATs(b []byte, cluster, value uint32) {
	reserved, spf := hostileGeometry(b)
	f1 := reserved * hostileSectors
	f2 := f1 + spf*hostileSectors
	binary.LittleEndian.PutUint32(b[f1+4*cluster:], value)
	binary.LittleEndian.PutUint32(b[f2+4*cluster:], value)
}

// A cluster chain that never reaches an EOC mark (FAT[root] = root) ends with an error.
func TestHostileClusterChainLoopEnds(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	setBothFATs(b, 2, 2)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var rerr error
	alloc, took := allocDuring(func() { _, rerr = fs.ReadDir(".") })
	if rerr == nil {
		t.Fatal("ReadDir of a looping root chain returned no error")
	}
	t.Logf("error: %v", rerr)
	requireBounded(t, alloc, took)
}

// A two-cluster loop in a file's chain (a -> b -> a) ends with an error when the file is read.
func TestHostileFileChainLoopEnds(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	// one.txt holds one cluster; point its FAT entry at the next cluster and that one back.
	c := hostileFileCluster(t, b)
	setBothFATs(b, c, c+1)
	setBothFATs(b, c+1, c)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var rerr error
	alloc, took := allocDuring(func() {
		f, oerr := fs.OpenFile("/a/b/one.txt", os.O_RDONLY)
		if oerr != nil {
			rerr = oerr
			return
		}
		_, rerr = io.Copy(io.Discard, f)
	})
	if rerr == nil {
		t.Fatal("reading a file with a looping chain returned no error")
	}
	t.Logf("error: %v", rerr)
	requireBounded(t, alloc, took)
}

// rawEntry finds the 32-byte directory entry with the short name sfn (11 bytes) in the cluster dirCluster of the
// volume bytes b and returns its offset and its first cluster.
func rawEntry(t *testing.T, b []byte, dirCluster uint32, sfn string) (off, cluster uint32) {
	t.Helper()
	le := binary.LittleEndian
	reserved, spf := hostileGeometry(b)
	spc := uint32(b[0x0D])
	data := (reserved + 2*spf) * hostileSectors
	base := data + (dirCluster-2)*spc*hostileSectors
	for i := uint32(0); i < spc*hostileSectors; i += 32 {
		if string(b[base+i:base+i+11]) == sfn {
			off = base + i
			return off, uint32(le.Uint16(b[off+20:]))<<16 | uint32(le.Uint16(b[off+26:]))
		}
	}
	t.Fatalf("no %q in cluster %d", sfn, dirCluster)
	return 0, 0
}

func setEntryCluster(b []byte, off, cluster uint32) {
	binary.LittleEndian.PutUint16(b[off+20:], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(b[off+26:], uint16(cluster))
}

func hostileFileCluster(t *testing.T, b []byte) uint32 {
	t.Helper()
	_, ca := rawEntry(t, b, 2, "A          ")
	_, cb := rawEntry(t, b, ca, "B          ")
	_, cf := rawEntry(t, b, cb, "ONE     TXT")
	return cf
}

// walkTree is the walk of a scanner: ReadDir by path, recursing into every directory. It stops at maxDepth (the
// test's own guard, so a reader that does not detect the cycle fails the test instead of running without end).
func walkTree(fs filesystem.FileSystem, dir string, depth, maxDepth int) (deepest int, err error) {
	if depth > maxDepth {
		return depth, fmt.Errorf("walk passed depth %d: the cycle was not detected", maxDepth)
	}
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return depth, err
	}
	deepest = depth
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := walkTree(fs, path.Join(dir, e.Name()), depth+1, maxDepth)
		if d > deepest {
			deepest = d
		}
		if err != nil {
			return deepest, err
		}
	}
	return deepest, nil
}

// A chain that leaves the volume ends with an error. FAT[root] names a cluster past the last data cluster of the
// volume (the FAT has room for more entries than the volume has clusters).
func TestHostileClusterChainOutsideVolumeEnds(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	reserved, spf := hostileGeometry(b)
	_ = reserved
	last := spf*hostileSectors/4 - 1
	setBothFATs(b, 2, last)
	setBothFATs(b, last, 0x0FFFFFFF) // an EOC mark: only the cluster number is wrong
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var rerr error
	alloc, took := allocDuring(func() { _, rerr = fs.ReadDir(".") })
	if rerr == nil {
		t.Fatal("ReadDir of a chain that leaves the volume returned no error")
	}
	t.Logf("error: %v", rerr)
	requireBounded(t, alloc, took)
}

// A directory that holds itself (/a/b names the cluster of /a) ends the walk with an error.
func TestHostileDirectoryHoldingItselfEnds(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	_, ca := rawEntry(t, b, 2, "A          ")
	eb, _ := rawEntry(t, b, ca, "B          ")
	setEntryCluster(b, eb, ca)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var (
		deepest int
		werr    error
	)
	alloc, took := allocDuring(func() { deepest, werr = walkTree(fs, ".", 0, 64) })
	if werr == nil {
		t.Fatalf("walk of a self-holding directory returned no error (deepest %d)", deepest)
	}
	if strings.Contains(werr.Error(), "not detected") {
		t.Fatalf("%v", werr)
	}
	t.Logf("deepest %d, error: %v", deepest, werr)
	requireBounded(t, alloc, took)
}

// A directory that names its own parent chain further up (/a/b names the root) ends the walk with an error too.
func TestHostileDirectoryHoldingAncestorEnds(t *testing.T) {
	defer watchHeap(t)()
	b := hostileBase(t)
	_, ca := rawEntry(t, b, 2, "A          ")
	eb, _ := rawEntry(t, b, ca, "B          ")
	setEntryCluster(b, eb, 2)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var werr error
	alloc, took := allocDuring(func() { _, werr = walkTree(fs, ".", 0, 64) })
	if werr == nil || strings.Contains(werr.Error(), "not detected") {
		t.Fatalf("want a cycle error, got %v", werr)
	}
	t.Logf("error: %v", werr)
	requireBounded(t, alloc, took)
}

// A valid volume is unchanged: the same tree and the same file bytes.
func TestHostileValidVolumeUnchanged(t *testing.T) {
	b := hostileBase(t)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	deepest, werr := walkTree(fs, ".", 0, 64)
	if werr != nil || deepest != 2 {
		t.Fatalf("walk: deepest %d err %v, want 2 and nil", deepest, werr)
	}
	f, err := fs.OpenFile("/a/b/one.txt", os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "hello world, a file of the test\n" {
		t.Fatalf("read %q err %v", got, err)
	}
}

// hostileDAG builds a volume of k levels. Each level holds the directories X and Y, and Y is re-pointed at the cluster
// of its sibling X, so there is no cycle but 2^k paths to the deepest directory, which holds one file.
func hostileDAG(t *testing.T, k int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dag.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "DAG", false)
	if err != nil {
		t.Fatal(err)
	}
	cur := ""
	for i := 0; i < k; i++ {
		for _, d := range []string{"/X", "/Y"} {
			if err := fs.Mkdir(cur + d); err != nil {
				t.Fatalf("mkdir %s%s: %v", cur, d, err)
			}
		}
		cur += "/X"
	}
	w, err := fs.OpenFile(cur+"/one.txt", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := uint32(2)
	for i := 0; i < k; i++ {
		_, x := rawEntry(t, b, dir, "X          ")
		y, _ := rawEntry(t, b, dir, "Y          ")
		setEntryCluster(b, y, x)
		dir = x
	}
	return b
}

// walkOnce is the walk of a scanner with ReadDirOnce: it counts the directories listed, the second paths refused
// (errors.Is ErrDirectoryAlreadyEntered) and the files seen.
func walkOnce(t *testing.T, fs *fat32.FileSystem, dir string, listed, refused, files *int) {
	t.Helper()
	entries, err := fs.ReadDirOnce(dir)
	if errors.Is(err, fat12.ErrDirectoryAlreadyEntered) {
		*refused++
		return
	}
	if err != nil {
		t.Fatalf("ReadDirOnce(%s): %v", dir, err)
	}
	*listed++
	for _, e := range entries {
		if e.IsDir() {
			walkOnce(t, fs, path.Join(dir, e.Name()), listed, refused, files)
		} else {
			*files++
		}
	}
}

// 16 levels, 65,536 paths to one file: each directory cluster is listed once, each second path is refused with the
// named error, and the walk ends in under 1 s. (ReadDir walks every path: 12.8 s and 25 GiB allocated, measured by
// the review of QIZ-6665.)
func TestHostileDirectoryReachedByManyPathsIsEnteredOnce(t *testing.T) {
	defer watchHeap(t)()
	const k = 16
	b := hostileDAG(t, k)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var listed, refused, files int
	alloc, took := allocDuring(func() { walkOnce(t, fs, ".", &listed, &refused, &files) })
	t.Logf("directories listed %d, second paths refused %d, files seen %d", listed, refused, files)
	if listed != k+1 || refused != k || files != 1 {
		t.Errorf("listed %d refused %d files %d, want %d, %d, 1", listed, refused, files, k+1, k)
	}
	requireBounded(t, alloc, took)
}

// The same directory listed twice by ReadDirOnce is refused the second time, with the named error and the path.
func TestHostileReadDirOnceRefusesSecondListing(t *testing.T) {
	fs, err := hostileOpen(t, hostileBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadDirOnce("a"); err != nil {
		t.Fatalf("first listing: %v", err)
	}
	_, err = fs.ReadDirOnce("a")
	if !errors.Is(err, fat12.ErrDirectoryAlreadyEntered) || !strings.Contains(err.Error(), "a") {
		t.Fatalf("second listing: got %v, want ErrDirectoryAlreadyEntered", err)
	}
}

// A FAT of 511 MiB in a 1 GiB volume fits the volume (16 KiB + 2 x 511 MiB < 1 GiB), but the data region after it is
// 2 MiB: only the entries 0 .. CountofClusters+1 are read. Before: +1,535 MiB allocated (3 x the stated FAT).
func TestHostileLargeFATOfLargeVolumeReadsOnlyValidEntries(t *testing.T) {
	defer watchHeap(t)()
	const vol = 1 << 30
	b := hostileBase(t)
	binary.LittleEndian.PutUint32(b[0x20:], vol/hostileSectors)
	binary.LittleEndian.PutUint32(b[0x24:], (vol/2-(64<<10))/hostileSectors)
	p := filepath.Join(t.TempDir(), "big.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(vol); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	g, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	var rerr error
	alloc, took := allocDuring(func() { _, rerr = fat32.Read(file.New(g, true), vol, 0, hostileSectors) })
	t.Logf("Read: %v", rerr)
	requireBounded(t, alloc, took)
}

// HeldBytes states what the reader holds for the FAT: 4 bytes for each entry 0 .. CountofClusters+1, not the FAT size.
func TestHostileHeldBytesIsTheValidEntries(t *testing.T) {
	b := hostileBase(t)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	reserved, spf := hostileGeometry(b)
	spc := uint32(b[0x0D])
	count := (uint32(len(b)) - (reserved+2*spf)*hostileSectors) / (spc * hostileSectors)
	want := int64(count+2+1) * 4 // entries 0 .. count+1, one more slot as the table keeps it
	if got := fs.HeldBytes(); got != want {
		t.Errorf("HeldBytes = %d, want %d (FAT is %d bytes)", got, want, spf*hostileSectors)
	}
}

// hostileCrossLinked builds a volume with /BIG.BIN (4 MiB) and /D with n small files, then points every entry of D
// at the first cluster and the size of BIG.BIN: n directory entries name one cluster chain.
func hostileCrossLinked(t *testing.T, n int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xlink.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "XLINK", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("/D"); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	w, err := fs.OpenFile("/BIG.BIN", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	for i := 0; i < n; i++ {
		w, err := fs.OpenFile(fmt.Sprintf("/D/F%05d.TXT", i), os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
	}
	_ = f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	reserved, _ := hostileGeometry(b)
	spc := uint32(b[0x0D])
	_, spf := hostileGeometry(b)
	data := (reserved + 2*spf) * hostileSectors
	cl := func(c uint32) uint32 { return data + (c-2)*spc*hostileSectors }
	bigOff, bigCluster := rawEntry(t, b, 2, "BIG     BIN")
	bigSize := le.Uint32(b[bigOff+28:])
	_, dc := rawEntry(t, b, 2, "D          ")
	patched := 0
	for c := dc; c >= 2 && c < 0x0FFFFFF8; c = le.Uint32(b[reserved*hostileSectors+4*c:]) & 0x0FFFFFFF {
		for i := uint32(0); i < spc*hostileSectors; i += 32 {
			o := cl(c) + i
			if b[o] == 'F' && string(b[o+8:o+11]) == "TXT" && b[o+11]&0x18 == 0 {
				setEntryCluster(b, o, bigCluster)
				le.PutUint32(b[o+28:], bigSize)
				patched++
			}
		}
	}
	if patched != n {
		t.Fatalf("patched %d entries, want %d", patched, n)
	}
	return b
}

// countingWriter is the sink of the test: it counts the bytes written to it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// walkFilesOnce lists D with ReadDirOnce and opens each file with OpenOnce, as a scanner does; it returns the number
// of files whose bytes were written, the refusals (errors.Is ErrFileAlreadyEntered), the bytes written and the first
// refusal's text.
func walkFilesOnce(t *testing.T, fs *fat32.FileSystem) (written, refused int, bytes int64, firstRefusal string) {
	t.Helper()
	entries, err := fs.ReadDirOnce("D")
	if err != nil {
		t.Fatal(err)
	}
	sink := &countingWriter{}
	for _, e := range entries {
		f, err := fs.OpenOnce(path.Join("D", e.Name()))
		if errors.Is(err, fat12.ErrFileAlreadyEntered) {
			if refused == 0 {
				firstRefusal = err.Error()
			}
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("OpenOnce(%s): %v", e.Name(), err)
		}
		if _, err := io.Copy(sink, f); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		written++
	}
	return written, refused, sink.n, firstRefusal
}

// 1,024 entries name one 4 MiB cluster chain. The chain is written once; the other 1,023 entries are refused, each
// with the path and the first cluster. (Open on each: 21.6 s and 4 GiB through the sink, measured by the review of
// QIZ-6665.) The FAT32 specification gives each cluster to one chain, so the second entry is a cross-link.
func TestHostileCrossLinkedFilesAreWrittenOnce(t *testing.T) {
	defer watchHeap(t)()
	const n = 1024
	b := hostileCrossLinked(t, n)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	_, bigCluster := rawEntry(t, b, 2, "BIG     BIN")
	var written, refused int
	var bytes int64
	var first string
	alloc, took := allocDuring(func() { written, refused, bytes, first = walkFilesOnce(t, fs) })
	t.Logf("files written %d, refused %d, bytes through the sink %d, first refusal: %s", written, refused, bytes, first)
	if written != 1 || refused != n-1 || bytes != 4<<20 {
		t.Errorf("written %d refused %d bytes %d, want 1, %d, %d", written, refused, bytes, n-1, 4<<20)
	}
	if !strings.Contains(first, "D/F") || !strings.Contains(first, fmt.Sprintf("first cluster %d", bigCluster)) {
		t.Errorf("refusal %q does not name the path and the first cluster %d", first, bigCluster)
	}
	// The bound is on the chain, not on the directory: every Open reads directory D again (1,024 entries), which alone
	// costs about 0.9 s and 1.9 GiB of allocation for 1,024 opens, with no data read (measured at e774f13, load 19);
	// that cost is the same for distinct files and is not the cross-link. The cross-link cost was 4 GiB through the
	// sink and 9 to 13 s more; the bytes above are the exact check, the time a coarse one.
	t.Logf("took %v, allocated %d KiB", took, alloc>>10)
	if took > 5*time.Second {
		t.Errorf("took %v, want under 5 s", took)
	}
}

// The same file opened twice by OpenOnce is refused the second time; a volume of distinct files is not refused.
func TestHostileOpenOnceDistinctFilesUnchanged(t *testing.T) {
	fs, err := hostileOpen(t, hostileBase(t))
	if err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenOnce("a/b/one.txt")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "hello world, a file of the test\n" {
		t.Fatalf("read %q err %v", got, err)
	}
	if _, err := fs.OpenOnce("a/b/one.txt"); !errors.Is(err, fat12.ErrFileAlreadyEntered) {
		t.Fatalf("second open: got %v, want ErrFileAlreadyEntered", err)
	}
}

// HeldBytes counts the sets ReadDirOnce and OpenOnce keep: one entry for each directory listed and each file opened.
func TestHostileHeldBytesCountsEnteredSets(t *testing.T) {
	fs, err := hostileOpen(t, hostileBase(t))
	if err != nil {
		t.Fatal(err)
	}
	before := fs.HeldBytes()
	if _, err := fs.ReadDirOnce("a/b"); err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenOnce("a/b/one.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got, want := fs.HeldBytes()-before, int64(2*24); got != want {
		t.Errorf("HeldBytes grew by %d, want %d (one directory, one file)", got, want)
	}
}
