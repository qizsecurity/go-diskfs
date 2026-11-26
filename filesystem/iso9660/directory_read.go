package iso9660

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// ErrDirectoryExtent is wrapped by the error a directory read returns when the extent its record states does not fit the
// volume.
var ErrDirectoryExtent = errors.New("directory extent does not fit the volume")

// minDirectoryRecordBytes is the shortest Directory Record: the 33 bytes before the File Identifier (ECMA-119 4th edition,
// 2019, 9.1, Table 8, page 36: BP 1 to 33) and a File Identifier of at least one byte (7.6.1, page 20: "one or more
// d-characters"; the root and parent records have one (00) or (01) byte, 6.8.2.2, page 12).
const minDirectoryRecordBytes = 34

// directoryReader is what a FileSystem keeps to read directories: one window, one logical block, allocated by the
// first directory read and reused by every later one (HeldBytes counts it).
type directoryReader struct {
	mu     sync.Mutex
	window []byte
}

// volumeSpaceBytes is the size of the volume space the primary volume descriptor states: Volume Space Size, "the number
// of Logical Blocks in which the Volume Space of the volume is recorded" (ECMA-119 8.4.8, page 25), times the Logical
// Block Size, "the size in bytes of a Logical Block" (8.4.12, page 26). The reader's size, when the caller gave one, can
// be smaller (a truncated file): a record cannot name bytes the file does not hold. It returns false when neither is
// known (a volume descriptor that states no size, and a size of 0).
func (fsm *FileSystem) volumeSpaceBytes() (int64, bool) {
	var bound int64
	known := false
	if pvd := fsm.volumes.primary; pvd != nil && pvd.volumeSize != 0 && pvd.blocksize != 0 {
		bound, known = int64(pvd.volumeSize)*int64(pvd.blocksize), true
	}
	if fsm.size > 0 && (!known || fsm.size < bound) {
		bound, known = fsm.size, true
	}
	return bound, known
}

// readDirExtent reads the directory whose Directory Record states location (9.1.3, page 36: the Logical Block Number of
// the first Logical Block of the Extent) and size (9.1.4, page 36: the 32-bit data length), through one window of one
// logical block, and parses each window with parse. A directory is one file in one Extent in non-interleaved mode
// (6.8.1, page 11; 6.4.1 Extent, page 6: a continuous ascending sequence of Logical Blocks) and "each Directory Record
// shall end in the Logical Sector in which it begins" (6.8.1.1, page 11), so a window of a whole number of Logical Blocks
// holds whole records, and a window is parsed by itself. Before anything is read, the extent must lie within the
// volume: location x block size + size must not pass the volume space (volumeSpaceBytes). An extent that does is
// refused with ErrDirectoryExtent and nothing is allocated for it: a record is a u32, so one record can state 4 GiB.
func (fsm *FileSystem) readDirExtent(what string, location, size uint32, parse func([]byte) ([]*directoryEntry, error)) ([]*directoryEntry, error) {
	// When neither the descriptor nor the caller states a size there is no bound to check against; the window still holds
	// one logical block at a time, so nothing is allocated for the stated size, and the read ends where the bytes end.
	if bound, known := fsm.volumeSpaceBytes(); known {
		if end := int64(location)*fsm.blocksize + int64(size); end > bound {
			return nil, fmt.Errorf("%s: its extent (%d bytes from block %d of %d bytes) ends at byte %d, past the %d bytes of the volume (Volume Space Size x Logical Block Size, ECMA-119 8.4.8, 8.4.12): %w",
				what, size, location, fsm.blocksize, end, bound, ErrDirectoryExtent)
		}
	}
	fsm.dirs.mu.Lock()
	defer fsm.dirs.mu.Unlock()
	if fsm.dirs.window == nil {
		fsm.dirs.window = make([]byte, fsm.blocksize)
	}
	var entries []*directoryEntry
	for off := int64(0); off < int64(size); off += fsm.blocksize {
		w := fsm.dirs.window
		if left := int64(size) - off; left < int64(len(w)) {
			w = w[:left]
		}
		n, err := fsm.backend.ReadAt(w, int64(location)*fsm.blocksize+off)
		if err != nil {
			return nil, fmt.Errorf("could not read %s: %v", what, err)
		}
		if n != len(w) {
			return nil, fmt.Errorf("reading %s returned %d bytes read instead of expected %d", what, n, len(w))
		}
		part, err := parse(w)
		if err != nil {
			return nil, fmt.Errorf("could not parse %s: %v", what, err)
		}
		entries = append(entries, part...)
	}
	return entries, nil
}

// HeldBytes is what the reader holds between calls: the parsed path tables (each record's struct and its name), and the
// directory window, one logical block, once a directory has been read. A ReadDir also holds a listing while it runs and
// returns it: the caller's, at most size / minDirectoryRecordBytes records for an extent of size bytes, the size bounded
// by the volume (readDirExtent).
func (fsm *FileSystem) HeldBytes() int64 {
	var n int64
	for _, pt := range []*pathTable{fsm.pathTable, fsm.jolietPathTable} {
		if pt == nil {
			continue
		}
		for _, r := range pt.records {
			n += int64(unsafe.Sizeof(*r)) + 8 + int64((len(r.dirname)+15)&^15)
		}
	}
	fsm.dirs.mu.Lock()
	n += int64(cap(fsm.dirs.window))
	fsm.dirs.mu.Unlock()
	return n
}
