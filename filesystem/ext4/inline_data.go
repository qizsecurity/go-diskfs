package ext4

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/diskfs/go-diskfs/filesystem"
)

// Inline data (the inline_data feature, inode flag EXT4_INLINE_DATA_FL 0x10000000) stores the data of a small
// file or directory inside the inode.
//
// Layout, from the kernel documentation (Documentation/filesystems/ext4/inlinedata.rst, "Inline Data" and
// "Inline Directories"; ifork.rst, "Inline Data"; attributes.rst for the xattr name index):
//   - the first 60 bytes are in inode.i_block (offset 0x28);
//   - the rest is the value of the in-inode ("ibody") extended attribute "system.data" (name index 7, name
//     "data"), even when its value has zero length;
//   - a directory has no "." and no ".." entry: the first four bytes of i_block are the inode number of the
//     parent, the 56 bytes after them are an array of ext4_dir_entry_2, and the value of "system.data", if any,
//     is a second array of ext4_dir_entry_2. The two are separate directory blocks: no entry spans both.
//     Inline directory entries have no checksum (the inode checksum covers them).
//
// Followed in source: Linux fs/ext4/inline.c ext4_read_inline_data (60 bytes of i_block, then the attribute
// value; inline size is 60 + e_value_size of the in-inode attribute) and ext4_read_inline_dir, and e2fsprogs
// lib/ext2fs/inline_data.c ext2fs_inline_data_dir_iterate (".", then ".." from i_block[0], then the 56 bytes of
// i_block after the parent number, then the attribute value, each as one directory block) and
// ext2fs_inline_data_get.

const (
	// inlineIBlockSize is EXT4_MIN_INLINE_DATA_SIZE: the size of inode.i_block.
	inlineIBlockSize = 60
	// inlineDotDotSize is EXT4_INLINE_DATA_DOTDOT_SIZE: the parent inode number at the start of i_block.
	inlineDotDotSize = 4
	// inlineIBlockOffset is the offset of i_block in the inode.
	inlineIBlockOffset = 0x28
	// inlineXattrName is the full name of the attribute that holds the data after i_block.
	inlineXattrName = "system.data"
	// dirEntryHeaderSize is the size of ext4_dir_entry_2 before the name (inode, rec_len, name_len, file_type).
	dirEntryHeaderSize = 8
)

// readInlineData returns the two parts of the inline data of an inode: the 60 bytes of i_block and the value of
// the in-inode attribute "system.data" (empty when the attribute has no value or is not there).
// Only the in-inode attribute is read, as Linux does (ext4_find_inline_data_nolock uses ext4_xattr_ibody_find).
func (fs *FileSystem) readInlineData(inodeNumber uint32) (iblock, ea []byte, err error) {
	raw, err := fs.readInodeRaw(inodeNumber)
	if err != nil {
		return nil, nil, err
	}
	iblock = raw[inlineIBlockOffset : inlineIBlockOffset+inlineIBlockSize]
	xattrs, err := fs.readIbodyXattrs(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("inode %d: reading the in-inode attributes: %w", inodeNumber, err)
	}
	return iblock, xattrs[inlineXattrName], nil
}

// readInlineFile returns the content of a regular file whose data is inline: its first size bytes of i_block
// followed by the attribute value. A size that the inline data cannot hold is an error, never padded.
func (fs *FileSystem) readInlineFile(inodeNumber uint32, size uint64) ([]byte, error) {
	iblock, ea, err := fs.readInlineData(inodeNumber)
	if err != nil {
		return nil, err
	}
	have := uint64(len(iblock) + len(ea))
	if size > have {
		return nil, fmt.Errorf("inode %d: file size %d is larger than its inline data (%d bytes in i_block and system.data)", inodeNumber, size, have)
	}
	data := make([]byte, 0, have)
	data = append(data, iblock...)
	data = append(data, ea...)
	return data[:size], nil
}

// readInlineDirectory returns the entries of a directory whose data is inline: "." (this inode), ".." (the
// number in the first four bytes of i_block), the entries in the rest of i_block, then those in system.data.
func (fs *FileSystem) readInlineDirectory(inodeNumber uint32) ([]*directoryEntry, error) {
	iblock, ea, err := fs.readInlineData(inodeNumber)
	if err != nil {
		return nil, err
	}
	entries := []*directoryEntry{
		{inode: inodeNumber, filename: ".", fileType: dirFileTypeDirectory},
		{inode: binary.LittleEndian.Uint32(iblock[:inlineDotDotSize]), filename: "..", fileType: dirFileTypeDirectory},
	}
	for _, area := range [][]byte{iblock[inlineDotDotSize:], ea} {
		more, err := parseInlineDirArea(area)
		if err != nil {
			return nil, fmt.Errorf("inline directory inode %d: %w", inodeNumber, err)
		}
		entries = append(entries, more...)
	}
	return entries, nil
}

// parseInlineDirArea reads one array of ext4_dir_entry_2 that fills area exactly. An entry with inode 0 is
// unused and is left out. The checks are those of Linux ext4_check_dir_entry: the entry header is there, rec_len
// is a multiple of 4, at least the header and name, and ends inside the area.
func parseInlineDirArea(area []byte) ([]*directoryEntry, error) {
	var entries []*directoryEntry
	for pos := 0; pos < len(area); {
		if len(area)-pos < dirEntryHeaderSize {
			return nil, fmt.Errorf("directory entry at %d: %d bytes left, header is %d", pos, len(area)-pos, dirEntryHeaderSize)
		}
		inode := binary.LittleEndian.Uint32(area[pos : pos+4])
		recLen := int(binary.LittleEndian.Uint16(area[pos+4 : pos+6]))
		nameLen := int(area[pos+6])
		fileType := directoryFileType(area[pos+7])
		switch {
		case recLen < dirEntryHeaderSize, recLen%4 != 0:
			return nil, fmt.Errorf("directory entry at %d: rec_len %d is not a multiple of 4 of at least %d", pos, recLen, dirEntryHeaderSize)
		case dirEntryHeaderSize+nameLen > recLen:
			return nil, fmt.Errorf("directory entry at %d: name length %d does not fit in rec_len %d", pos, nameLen, recLen)
		case pos+recLen > len(area):
			return nil, fmt.Errorf("directory entry at %d: rec_len %d goes past the end of the area (%d)", pos, recLen, len(area))
		}
		if inode != 0 {
			entries = append(entries, &directoryEntry{
				inode:    inode,
				filename: string(area[pos+dirEntryHeaderSize : pos+dirEntryHeaderSize+nameLen]),
				fileType: fileType,
			})
		}
		pos += recLen
	}
	return entries, nil
}

// openInlineFile opens a regular file whose data is inline, for reading. The data is read when the file is
// opened. Writing inline data is not supported.
func (fs *FileSystem) openInlineFile(in *inode, flag int, filename string, fileType directoryFileType) (filesystem.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, fmt.Errorf("cannot open inode %d for writing: inline data is read only in this package", in.number)
	}
	data, err := fs.readInlineFile(in.number, in.size)
	if err != nil {
		return nil, err
	}
	return &File{
		inode:      in,
		filesystem: fs,
		filename:   filename,
		fileType:   fileType,
		inline:     data,
	}, nil
}
