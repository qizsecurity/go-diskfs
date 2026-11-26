package iso9660

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"unsafe"

	"github.com/diskfs/go-diskfs/backend"
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
	return volumeSpaceBytesOf(fsm.volumes.primary, fsm.size)
}

// volumeSpaceBytesOf is volumeSpaceBytes for a primary volume descriptor and a size the caller gave (0: none).
func volumeSpaceBytesOf(pvd *primaryVolumeDescriptor, size int64) (int64, bool) {
	var bound int64
	known := false
	if pvd != nil && pvd.volumeSize != 0 && pvd.blocksize != 0 {
		bound, known = int64(pvd.volumeSize)*int64(pvd.blocksize), true
	}
	if size > 0 && (!known || size < bound) {
		bound, known = size, true
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

// ErrFileExtent is wrapped by the error OpenFile returns when a file's extent does not fit the volume.
var ErrFileExtent = errors.New("file extent does not fit the volume")

// checkFileExtent refuses a file whose extent the volume does not hold: its Directory Record states the first Logical
// Block (9.1.3, page 36) and the data length of the File Section (9.1.4, page 36), and the extent, location x Logical
// Block Size + data length, must lie within the Volume Space (8.4.8 x 8.4.12, pages 25 and 26; the reader's size when the
// caller gave a smaller one). A file with no bytes names no extent. Nothing is read for a refused file: bytes the volume
// does not hold would be made up.
func (fsm *FileSystem) checkFileExtent(p string, location, size uint32) error {
	if size == 0 {
		return nil
	}
	bound, known := fsm.volumeSpaceBytes()
	if !known {
		return nil
	}
	if end := int64(location)*fsm.blocksize + int64(size); end > bound {
		return fmt.Errorf("file %s: its extent (%d bytes from block %d of %d bytes) ends at byte %d, past the %d bytes of the volume (Volume Space Size x Logical Block Size, ECMA-119 8.4.8, 8.4.12): %w",
			p, size, location, fsm.blocksize, end, bound, ErrFileExtent)
	}
	return nil
}

// ErrPathTableExtent is wrapped by the error Read returns when a path table's extent does not fit the volume.
var ErrPathTableExtent = errors.New("path table extent does not fit the volume")

// ErrContinuationArea is wrapped by the error a directory read returns when a Rock Ridge / SUSP continuation area
// does not fit the volume, or the continuation entries make a loop.
var ErrContinuationArea = errors.New("continuation area does not fit the volume or loops")

// maxPathTableRecordBytes is the longest path table record: 8 bytes, a Directory Identifier of 255 bytes at most (the
// length is an 8-bit number, ECMA-119 9.4.1, and 7.6.3 bounds it lower), and one padding byte when the length is odd.
const maxPathTableRecordBytes = 8 + 255 + 1

// readPathTable reads the path table the volume descriptor states: Path Table Size (8.4.13, page 26; 8.5.7), a 32-bit
// byte count, at the Logical Block Number of its first Logical Block (8.4.14, page 26; 8.5.8). The extent must lie in the
// volume (volumeSpaceBytesOf): else ErrPathTableExtent and nothing is allocated for the stated size. It is read through
// one window of one logical block plus the longest record, and the records are parsed as they complete: a record that
// runs past the window is carried to the next one. The parsed records are what the reader keeps (HeldBytes). A record
// of name length 0 ends the table, as in parsePathTable.
func readPathTable(b backend.Storage, pvd *primaryVolumeDescriptor, size int64, location uint32, tableSize uint32, blocksize int64, joliet bool) (*pathTable, error) {
	start := int64(location) * blocksize
	if bound, known := volumeSpaceBytesOf(pvd, size); known && start+int64(tableSize) > bound {
		return nil, fmt.Errorf("its extent (%d bytes from block %d of %d bytes) ends at byte %d, past the %d bytes of the volume (Volume Space Size x Logical Block Size, ECMA-119 8.4.8, 8.4.12): %w",
			tableSize, location, blocksize, start+int64(tableSize), bound, ErrPathTableExtent)
	}
	parse := parsePathTable
	if joliet {
		parse = parseJolietPathTable
	}
	win := make([]byte, blocksize+maxPathTableRecordBytes)
	var records []*pathTableEntry
	held, off := 0, int64(0) // bytes carried at the start of win, bytes of the table consumed
	for off < int64(tableSize) {
		want := int64(tableSize) - off
		if want > blocksize {
			want = blocksize
		}
		n, err := b.ReadAt(win[held:held+int(want)], start+off)
		if err != nil && !(errors.Is(err, io.EOF) && n == int(want)) {
			return nil, fmt.Errorf("unable to read path table of size %d at location %d: %v", tableSize, start, err)
		}
		if n != int(want) {
			return nil, fmt.Errorf("read %d bytes of path table instead of expected %d at location %d", n, want, start)
		}
		off += want
		chunk := win[:held+int(want)]
		// the whole records in the chunk; a zero name length ends the table
		end, done := 0, false
		for end < len(chunk) {
			if chunk[end] == 0 {
				done = true
				break
			}
			rec := 8 + int(chunk[end])
			if chunk[end]%2 != 0 {
				rec++
			}
			if end+rec > len(chunk) {
				break
			}
			end += rec
		}
		records = append(records, parse(chunk[:end]).records...)
		if done {
			break
		}
		held = copy(win, chunk[end:])
	}
	return &pathTable{records: records}, nil
}

// readContinuation reads the SUSP continuation area a CE entry states: location (the block of the area), offset (to its
// start in that block) and length (IEEE P1281, SUSP 1.12, section 5.1, the "CE" System Use Entry: BP 5 to 12, 13 to 20,
// 21 to 28). The area must lie in the volume: block x block size + offset + length must not pass it (ErrContinuationArea).
// It is read in the library's copy chunks into a buffer that grows with the bytes read, so the stated length allocates
// nothing: a length past the end of the file ends with the bytes the file holds. The areas a record's chain names are
// kept in seen: one named twice is a loop, which no list that ends holds.
func (fsm *FileSystem) readContinuation(location, offset int64, length uint32, seen map[[3]int64]struct{}) ([]byte, error) {
	pos := location*fsm.blocksize + offset
	if bound, known := fsm.volumeSpaceBytes(); known && pos+int64(length) > bound {
		return nil, fmt.Errorf("its area (%d bytes at offset %d of block %d of %d bytes) ends at byte %d, past the %d bytes of the volume (SUSP section 5.1; ECMA-119 8.4.8, 8.4.12): %w",
			length, offset, location, fsm.blocksize, pos+int64(length), bound, ErrContinuationArea)
	}
	key := [3]int64{location, offset, int64(length)}
	if _, again := seen[key]; again {
		return nil, fmt.Errorf("the area (%d bytes at offset %d of block %d) is named again by a later CE entry of the same record: %w", length, offset, location, ErrContinuationArea)
	}
	seen[key] = struct{}{}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.NewSectionReader(fsm.backend, pos, int64(length)))
	if err != nil {
		return nil, fmt.Errorf("error reading continuation entry data at %d: %v", location, err)
	}
	if n != int64(length) {
		return nil, fmt.Errorf("read continuation entry data %d bytes instead of expected %d", n, length)
	}
	return buf.Bytes(), nil
}
