package fat32_test

// Hostile FAT32 volumes whose cluster chains overlap: two directory entries whose chains are not the same chain but
// share clusters (they start in the middle of one chain, or they merge into one partway).
//
// Standard: Microsoft FAT32 File System Specification (fatgen103, the 2004 edition, "Microsoft Confidential" header):
//   - Section 4 "FAT", page 16: "The FAT defines a singly linked list of the 'extents' (clusters) of a file and thereby
//     maps the data region of the volume by cluster number." One entry gives one next cluster, so a cluster is in the
//     list of one file; two lists that share a cluster are two files that hold the same bytes, which no volume made by
//     the specification's own algorithms holds. The page does not say "cross-link"; refusing it is a reading of that
//     sentence, not a quotation.
//   - Section 6, page 26, ATTR_DIRECTORY: a directory is a file too ("A FAT directory or file container is simply a
//     regular file with a special attribute", page 16 note), so a directory's chain and a file's chain are disjoint.
//   - Section 3.5, page 15: MAX = CountofClusters + 1; page 16: entries from MAX+1 "must not be used".
//
// Inputs: a volume made by this package's own writer with entries of it re-pointed (the writer cannot make a cross-link).

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
	"github.com/diskfs/go-diskfs/filesystem/fat12"
	"github.com/diskfs/go-diskfs/filesystem/fat32"
)

// overlapVolume makes a volume with /BIG.BIN (4 MiB) and a directory /D of n one-byte files, and returns its bytes.
func overlapVolume(t *testing.T, n int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "overlap.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "OVERLAP", false)
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
	return b
}

// fatChain reads the cluster chain that starts at first from the first FAT of the volume bytes.
func fatChain(b []byte, first uint32) []uint32 {
	reserved, _ := hostileGeometry(b)
	var out []uint32
	for c := first; c >= 2 && c < 0x0FFFFFF8 && len(out) < 1<<20; {
		out = append(out, c)
		c = binary.LittleEndian.Uint32(b[reserved*hostileSectors+4*c:]) & 0x0FFFFFFF
	}
	return out
}

// dirFileOffsets returns the offsets of the F*.TXT directory entries in directory D, in order.
func dirFileOffsets(t *testing.T, b []byte) []uint32 {
	t.Helper()
	reserved, spf := hostileGeometry(b)
	spc := uint32(b[0x0D])
	data := (reserved + 2*spf) * hostileSectors
	_, dc := rawEntry(t, b, 2, "D          ")
	var offs []uint32
	for _, c := range fatChain(b, dc) {
		base := data + (c-2)*spc*hostileSectors
		for i := uint32(0); i < spc*hostileSectors; i += 32 {
			o := base + i
			if b[o] == 'F' && string(b[o+8:o+11]) == "TXT" && b[o+11]&0x18 == 0 {
				offs = append(offs, o)
			}
		}
	}
	return offs
}

// walkD lists D with ReadDirOnce and opens each file with OpenOnce, reading each opened file to its end: what a scanner
// does. It returns the number of files read, the refusals that are ErrChainsOverlap, the other refusals, the bytes read
// and the first overlap refusal's text.
func walkD(t *testing.T, fs *fat32.FileSystem) (read, overlap, same int, bytes int64, first string) {
	t.Helper()
	entries, err := fs.ReadDirOnce("D")
	if err != nil {
		t.Fatal(err)
	}
	sink := &countingWriter{}
	for _, e := range entries {
		f, err := fs.OpenOnce(path.Join("D", e.Name()))
		switch {
		case errors.Is(err, fat12.ErrChainsOverlap):
			if overlap == 0 {
				first = err.Error()
			}
			overlap++
			continue
		case errors.Is(err, fat12.ErrFileAlreadyEntered):
			same++
			continue
		case err != nil:
			t.Fatalf("OpenOnce(%s): %v", e.Name(), err)
		}
		if _, err := io.Copy(sink, f); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		read++
	}
	return read, overlap, same, sink.n, first
}

// Entry i starts at the i-th cluster of one 4 MiB chain: the chains overlap partway, and OpenOnce does not see a
// repeated first cluster. The first entry reads the chain; every later entry names a cluster that chain holds and is
// refused. Without the refusal the walk reads about n x chain/2 bytes (measured by the QIZ-6665 review at f1e90a7: n=1,024
// gave 3,840 MiB read, 20.6 s, +56 GiB allocated).
func TestHostileOverlappingChainsAreReadOnce(t *testing.T) {
	defer watchHeap(t)()
	const n = 256
	b := overlapVolume(t, n)
	bigOff, bigCluster := rawEntry(t, b, 2, "BIG     BIN")
	bigSize := binary.LittleEndian.Uint32(b[bigOff+28:])
	chain := fatChain(b, bigCluster)
	spc := uint32(b[0x0D])
	bpc := spc * hostileSectors
	offs := dirFileOffsets(t, b)
	if len(offs) != n || len(chain) < n {
		t.Fatalf("offsets %d chain %d, want %d offsets and a chain of at least that", len(offs), len(chain), n)
	}
	for i, o := range offs {
		setEntryCluster(b, o, chain[i])
		binary.LittleEndian.PutUint32(b[o+28:], bigSize-uint32(i)*bpc)
	}
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	var read, overlap, same int
	var bytes int64
	var first string
	alloc, took := allocDuring(func() { read, overlap, same, bytes, first = walkD(t, fs) })
	t.Logf("files read %d, overlap refusals %d, other refusals %d, bytes read %d, took %v, allocated %d KiB; first refusal: %s",
		read, overlap, same, bytes, took, alloc>>10, first)
	if read != 1 || overlap != n-1 || same != 0 || bytes != int64(bigSize) {
		t.Errorf("read %d overlap %d same %d bytes %d, want 1, %d, 0, %d", read, overlap, same, bytes, n-1, bigSize)
	}
	if !strings.Contains(first, "D/F00001.TXT") || !strings.Contains(first, fmt.Sprintf("first cluster %d", chain[1])) ||
		!strings.Contains(first, fmt.Sprintf("cluster %d", chain[1])) {
		t.Errorf("refusal %q does not name the path and the cluster %d", first, chain[1])
	}
	if took > 5*time.Second {
		t.Errorf("took %v, want under 5 s", took)
	}
}

// Two files with different first clusters whose chains merge: the second file's head is its own, its tail is the first
// file's tail. The second is refused with the merge cluster named; the first is read whole.
func TestHostileChainsMergingPartwayAreRefused(t *testing.T) {
	b := overlapVolume(t, 2)
	_, bigCluster := rawEntry(t, b, 2, "BIG     BIN")
	chain := fatChain(b, bigCluster)
	offs := dirFileOffsets(t, b)
	// F00000 is the long file: it takes BIG's chain. F00001 keeps its own one cluster and links it into the middle.
	setEntryCluster(b, offs[0], chain[0])
	binary.LittleEndian.PutUint32(b[offs[0]+28:], 4<<20)
	own := uint32(binary.LittleEndian.Uint16(b[offs[1]+20:]))<<16 | uint32(binary.LittleEndian.Uint16(b[offs[1]+26:]))
	merge := chain[len(chain)/2]
	setBothFATs(b, own, merge)
	binary.LittleEndian.PutUint32(b[offs[1]+28:], 1<<20)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	read, overlap, same, bytes, first := walkD(t, fs)
	t.Logf("read %d overlap %d same %d bytes %d first %q", read, overlap, same, bytes, first)
	if read != 1 || overlap != 1 || same != 0 || bytes != 4<<20 {
		t.Errorf("read %d overlap %d same %d bytes %d, want 1, 1, 0, %d", read, overlap, same, bytes, 4<<20)
	}
	if !strings.Contains(first, fmt.Sprintf("cluster %d", merge)) {
		t.Errorf("refusal %q does not name the merge cluster %d", first, merge)
	}
}

// A file entry that names a cluster of a directory already listed is refused: a directory is a file, so its chain is
// not another file's chain.
func TestHostileFileOverlappingListedDirectoryIsRefused(t *testing.T) {
	b := overlapVolume(t, 1)
	_, dc := rawEntry(t, b, 2, "D          ")
	offs := dirFileOffsets(t, b)
	setEntryCluster(b, offs[0], dc)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadDirOnce("D"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.OpenOnce("D/F00000.TXT"); !errors.Is(err, fat12.ErrChainsOverlap) {
		t.Fatalf("got %v, want ErrChainsOverlap", err)
	}
}

// The claims are bytes the reader holds: HeldBytes grows by the bitmap (one bit for each data cluster) once a chain
// is claimed, and by the 24 bytes of the first-cluster set only for the next chain.
func TestHostileHeldBytesCountsClaimedClusters(t *testing.T) {
	fs, err := hostileOpen(t, overlapVolume(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	before := fs.HeldBytes()
	if _, err := fs.ReadDirOnce("D"); err != nil {
		t.Fatal(err)
	}
	afterDir := fs.HeldBytes()
	f, err := fs.OpenOnce("D/F00000.TXT")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	afterFile := fs.HeldBytes()
	t.Logf("HeldBytes %d -> %d (directory) -> %d (file)", before, afterDir, afterFile)
	// ReadDirOnce adds the directory's entry of the first-cluster set (24 bytes), the listing it keeps (the path
	// stack) and the bitmap, one bit for each data cluster: a 32nd of the table, which holds 4 bytes for each cluster
	// (calculated). The file's chain and its listing are held already.
	if bitmap := afterDir - before - 24; bitmap < before/32 {
		t.Errorf("claiming the first chain grew HeldBytes by %d, want at least the bitmap of %d", afterDir-before, before/32)
	}
	if afterFile-afterDir != 24 {
		t.Errorf("second chain grew HeldBytes by %d, want 24 (its first-cluster entry only)", afterFile-afterDir)
	}
}

// A wide directory: OpenOnce on each of n distinct files reads the directory once, not once for each file. Before the
// path stack, every OpenOnce read and parsed directory D again from the root: n=1,024 took 0.81 s and allocated
// 1,873 MiB, n=4,096 took 12.9 s and 29.8 GiB (measured with the overlap check in place, load 19; the growth is n
// squared). The bound is on allocation, which does not depend on the machine.
func TestHostileOpenOnceOfAWideDirectoryReadsItOnce(t *testing.T) {
	defer watchHeap(t)()
	const n = 1024
	fs, err := hostileOpen(t, overlapVolume(t, n))
	if err != nil {
		t.Fatal(err)
	}
	var opened int
	alloc, took := allocDuring(func() {
		entries, err := fs.ReadDirOnce("D")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			f, err := fs.OpenOnce(path.Join("D", e.Name()))
			if err != nil {
				t.Fatalf("OpenOnce(%s): %v", e.Name(), err)
			}
			_ = f.Close()
			opened++
		}
	})
	t.Logf("opened %d, took %v, allocated %d MiB", opened, took, alloc>>20)
	if opened != n {
		t.Errorf("opened %d, want %d", opened, n)
	}
	if alloc > 128<<20 {
		t.Errorf("allocated %d MiB, want at most 128 (1,873 before)", alloc>>20)
	}
}

// crossedDirs makes /X (holding ONE.TXT) and /Y, then re-points Y at the cluster of X: two paths, one directory, one file.
func crossedDirs(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "crossed.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "CROSSED", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/X", "/Y"} {
		if err := fs.Mkdir(d); err != nil {
			t.Fatal(err)
		}
	}
	w, err := fs.OpenFile("/X/ONE.TXT", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("one file, two paths\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, xc := rawEntry(t, b, 2, "X          ")
	yOff, _ := rawEntry(t, b, 2, "Y          ")
	setEntryCluster(b, yOff, xc)
	return b
}

// The same file reached through a second path (a directory entry that names a directory already reached) is read
// once, whichever path comes first, and the second is refused with an error that names its path.
func TestHostileSecondPathToTheSameFileIsRefused(t *testing.T) {
	for _, order := range [][2]string{{"X/ONE.TXT", "Y/ONE.TXT"}, {"Y/ONE.TXT", "X/ONE.TXT"}} {
		fs, err := hostileOpen(t, crossedDirs(t))
		if err != nil {
			t.Fatal(err)
		}
		f, err := fs.OpenOnce(order[0])
		if err != nil {
			t.Fatalf("first path %s: %v", order[0], err)
		}
		got, err := io.ReadAll(f)
		if err != nil || string(got) != "one file, two paths\n" {
			t.Fatalf("read %q err %v", got, err)
		}
		_, err = fs.OpenOnce(order[1])
		if !errors.Is(err, fat12.ErrFileAlreadyEntered) || !strings.Contains(err.Error(), order[1]) {
			t.Errorf("second path %s: got %v, want ErrFileAlreadyEntered naming the path", order[1], err)
		}
	}
}

// A directory that holds itself, reached through the path stack, is refused with the cycle error on every call,
// also when the stack holds the first levels of the path from the call before.
func TestHostileDirectoryLoopThroughTheStackIsRefused(t *testing.T) {
	b := crossedDirs(t)
	_, xc := rawEntry(t, b, 2, "X          ")
	// The entry ONE.TXT of X becomes a directory that names X itself.
	off, _ := rawEntry(t, b, xc, "ONE     TXT")
	b[off+11] |= 0x10
	setEntryCluster(b, off, xc)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := fs.ReadDirOnce("X/ONE.TXT"); err == nil || !strings.Contains(err.Error(), "directory cycle") {
			t.Fatalf("call %d: got %v, want a directory cycle error", i, err)
		}
	}
}

// A write to a directory drops the held path: a file made after ReadDirOnce is found by the next OpenOnce.
func TestHostileResolvedPathIsRefreshedAfterAWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rw.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(hostileVolume); err != nil {
		t.Fatal(err)
	}
	fs, err := fat32.Create(file.New(f, false), hostileVolume, 0, hostileSectors, "RW", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("/D"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadDirOnce("D"); err != nil {
		t.Fatal(err)
	}
	w, err := fs.OpenFile("/D/NEW.TXT", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("made after the listing")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	r, err := fs.OpenOnce("D/NEW.TXT")
	if err != nil {
		t.Fatalf("OpenOnce of a file made after ReadDirOnce: %v", err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "made after the listing" {
		t.Errorf("read %q", got)
	}
}

// HeldBytes counts the directories of the held path: within 5 percent of the heap that listing holds.
func TestHostileHeldBytesCountsTheStack(t *testing.T) {
	fs, err := hostileOpen(t, overlapVolume(t, 1024))
	if err != nil {
		t.Fatal(err)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	h0 := fs.HeldBytes()
	if _, err := fs.ReadDirOnce("D"); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	held, heap := fs.HeldBytes()-h0, int64(m1.HeapAlloc)-int64(m0.HeapAlloc)
	t.Logf("HeldBytes grew by %d, heap in use by %d", held, heap)
	if d := held - heap; d < -heap/20 || d > heap/20 {
		t.Errorf("HeldBytes grew by %d, the heap by %d: more than 5 percent apart", held, heap)
	}
}

// A file whose DIR_FileSize is larger than its cluster chain ends with an error, not a panic: the second Read found no
// cluster for its offset and indexed past the chain (measured: "index out of range [1] with length 1" for a size of
// 1 MiB over a chain of one cluster). Specification, section 6, page 24: DIR_FileSize is "the size in bytes of
// file/directory described by this entry"; the chain is what holds those bytes.
func TestHostileFileSizeLargerThanItsChainEndsWithAnError(t *testing.T) {
	b := overlapVolume(t, 1)
	offs := dirFileOffsets(t, b)
	binary.LittleEndian.PutUint32(b[offs[0]+28:], 1<<20)
	fs, err := hostileOpen(t, b)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenOnce("D/F00000.TXT")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, f)
	if !errors.Is(err, fat12.ErrFileSizeBeyondChain) || !strings.Contains(err.Error(), "larger than its cluster chain") {
		t.Fatalf("read %d bytes, err %v, want an error that the size is larger than the chain", n, err)
	}
	if n > 1<<20 {
		t.Errorf("read %d bytes, more than the size 1 MiB", n)
	}
}
