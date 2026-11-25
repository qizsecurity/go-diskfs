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
