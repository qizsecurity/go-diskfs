package squashfs

import (
	"encoding/binary"
	"fmt"
)

const (
	xAttrIDEntrySize      uint32 = 16
	xAttrHeaderSize       uint32 = 16
	noXattrInodeFlag      uint32 = 0xffffffff
	noXattrSuperblockFlag uint64 = 0xffffffffffffffff
)

type xAttrIndex struct {
	pos   uint64
	count uint32
	size  uint32
}

func parseXAttrIndex(b []byte, offsetMap map[uint32]uint32) (*xAttrIndex, error) {
	if len(b) < int(xAttrIDEntrySize) {
		return nil, fmt.Errorf("cannot parse xAttr Index of size %d less than minimum %d", len(b), xAttrIDEntrySize)
	}

	offsetKey := binary.LittleEndian.Uint32(b[2:6])
	offset, ok := offsetMap[offsetKey]
	if !ok {
		return nil, fmt.Errorf("cannot parse xAttr Index invalid offset key %d", offsetKey)
	}
	toReturn := &xAttrIndex{
		pos:   uint64(binary.LittleEndian.Uint16(b[0:2])) + uint64(offset),
		count: binary.LittleEndian.Uint32(b[8:12]),
		size:  binary.LittleEndian.Uint32(b[12:16]),
	}

	return toReturn, nil
}

type xAttrTable struct {
	list []*xAttrIndex
	data []byte
}

func (x *xAttrTable) find(pos int) (map[string]string, error) {
	if pos >= len(x.list) {
		return nil, fmt.Errorf("position %d is greater than list size %d", pos, len(x.list))
	}
	entry := x.list[pos]
	if int(entry.pos) >= len(x.data) {
		return nil, fmt.Errorf("entry position %d is greater than list size %d", entry.pos, len(x.data))
	}
	b := x.data[entry.pos:]
	count := entry.count
	ptr := 0
	xattrs := map[string]string{}
	for i := 0; i < int(count); i++ {
		// must be 4 bytes for header
		if len(b[ptr:]) < 4 {
			return nil, fmt.Errorf("insufficient bytes %d to read the xattr at position %d", len(b[ptr:]), ptr)
		}
		// get the type and size
		//   xType := binary.LittleEndian.Uint16(b[ptr : ptr+2])
		xSize := int(binary.LittleEndian.Uint16(b[ptr+2 : ptr+4]))
		nameStart := ptr + 4
		valHeaderStart := nameStart + xSize
		valStart := valHeaderStart + 4
		// make sure we have enough bytes
		if len(b[nameStart:]) < xSize {
			return nil, fmt.Errorf("xattr header has size %d, but only %d bytes available to read at position %d", xSize, len(b[ptr+4:]), ptr)
		}
		if xSize < 1 {
			return nil, fmt.Errorf("no name given for xattr at position %d", ptr)
		}
		key := string(b[nameStart : nameStart+xSize])
		// read the size of the value
		if len(b[valHeaderStart:]) < 4 {
			return nil, fmt.Errorf("insufficient bytes %d to read the xattr value at position %d", len(b[valHeaderStart:]), ptr)
		}
		valSize := int(binary.LittleEndian.Uint32(b[valHeaderStart:valStart]))
		if len(b[valStart:]) < valSize {
			return nil, fmt.Errorf("xattr value has size %d, but only %d bytes available to read at position %d", valSize, len(b[valStart:]), ptr)
		}
		val := string(b[valStart : valStart+valSize])
		xattrs[key] = val

		// increment the position pointer
		ptr += valStart + valSize
	}
	return xattrs, nil
}

// validateXattrIndex applies the checks of Linux fs/squashfs/xattr_id.c squashfs_read_xattr_id_table (v6.12, lines 79-124)
// to the locations of the xattr id blocks (the index that follows the table header): each location is above the one
// before, and the gap is at most SQUASHFS_METADATA_SIZE + SQUASHFS_BLOCK_OFFSET (8192 + 2); the last id block is before
// idTableStart (the superblock's xattr_id_table_start) by at most that; dataStart (the xattr table's start) is before
// the first id block. So the xattr data, read from dataStart up to the first id block, lies wholly before idTableStart.
func validateXattrIndex(locns []uint64, dataStart, idTableStart uint64) error {
	maxGap := uint64(standardMetadataBlocksize) + 2
	if len(locns) == 0 {
		return fmt.Errorf("xattr id table has no index entries")
	}
	for i := 0; i+1 < len(locns); i++ {
		if locns[i] >= locns[i+1] || locns[i+1]-locns[i] > maxGap {
			return fmt.Errorf("xattr id block %d at %d and block %d at %d: locations must rise by at most %d", i, locns[i], i+1, locns[i+1], maxGap)
		}
	}
	last := locns[len(locns)-1]
	if last >= idTableStart || idTableStart-last > maxGap {
		return fmt.Errorf("last xattr id block at %d must be before the xattr id table at %d by at most %d", last, idTableStart, maxGap)
	}
	if dataStart >= locns[0] {
		return fmt.Errorf("xattr table start %d must be before the first xattr id block at %d", dataStart, locns[0])
	}
	return nil
}
