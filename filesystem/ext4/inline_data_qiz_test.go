package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

// qizWalk lists every directory reachable from the root with ReadDir. A panic is returned as
// an error. Each directory that ReadDir refused is returned with its error.
func qizWalk(fsys *FileSystem) (listed []string, refused map[string]error, panicked any) {
	refused = map[string]error{}
	defer func() { panicked = recover() }()
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			refused[dir] = err
			return
		}
		for _, e := range entries {
			p := e.Name()
			if dir != "." {
				p = dir + "/" + e.Name()
			}
			listed = append(listed, p)
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk(".")
	return listed, refused, nil
}

// gpt-ext4 is made with inline_data (EXT4_INLINE_DATA_FL 0x10000000, Linux fs/ext4/ext4.h): the
// inode has no extent tree, its data is in i_block and the system.data attribute. Reading such a
// directory used to dereference the nil extent tree (panic in readDirectory). It must return
// ErrInlineData; inline data is not read by this package.
func TestQizInlineDataDirectoryNoPanic(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt4)
	if err != nil {
		t.Fatalf("open gpt-ext4: %v", err)
	}
	listed, refused, panicked := qizWalk(fsys)
	if panicked != nil {
		t.Fatalf("walk panicked: %v", panicked)
	}
	if len(listed) == 0 {
		t.Fatalf("walk listed nothing")
	}
	if len(refused) == 0 {
		t.Fatalf("fixture check: expected inline directories in gpt-ext4, but every directory was read")
	}
	for dir, err := range refused {
		if !errors.Is(err, ErrInlineData) {
			t.Errorf("ReadDir(%q): got %v, want ErrInlineData", dir, err)
		}
	}
	// the root directory has an extent tree and is listed
	if _, bad := refused["."]; bad {
		t.Errorf("the root directory was refused")
	}
}

// An inline file cannot be opened: ErrInlineData, not the "special file" message.
func TestQizInlineDataFileOpen(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt4)
	if err != nil {
		t.Fatalf("open gpt-ext4: %v", err)
	}
	// tiny.txt: 5 bytes, inode 21, flags 0x10000000 (measured)
	if _, err := fsys.OpenFile("tiny.txt", os.O_RDONLY); !errors.Is(err, ErrInlineData) {
		t.Errorf("OpenFile(tiny.txt): got %v, want ErrInlineData", err)
	}
}

// Every oracle row of gpt-ext4 either matches, or fails only with ErrInlineData (the row, or a
// directory on its path, has inline data). No row differs for another reason.
func TestQizInlineDataOracleRows(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt4)
	if err != nil {
		t.Fatalf("open gpt-ext4: %v", err)
	}
	rows := qizLoadOracle(t, qizGPTExt4)
	var matched, inline []string
	for _, r := range rows {
		var diffs []qizDiff
		func() {
			defer func() {
				if p := recover(); p != nil {
					diffs = []qizDiff{{"panic", fmt.Sprint(p), nil}}
				}
			}()
			diffs = qizCompareRow(fsys, r)
		}()
		switch {
		case len(diffs) == 0:
			matched = append(matched, r.path)
		default:
			onlyInline := true
			for _, d := range diffs {
				if d.err == nil || !errors.Is(d.err, ErrInlineData) {
					onlyInline = false
					t.Errorf("%s: %v", r.path, d)
				}
			}
			if onlyInline {
				inline = append(inline, r.path)
			}
		}
	}
	sort.Strings(matched)
	sort.Strings(inline)
	t.Logf("%d rows: %d match the oracle %v; %d refused with ErrInlineData %v", len(rows), len(matched), matched, len(inline), inline)
	if len(matched) == 0 {
		t.Errorf("no row matched")
	}
}

// A symlink with inline data (target of 60 bytes or more) has no extent tree either; readInode
// used to dereference it. No fixture disk has one, so this takes the real inline inode 30 of
// gpt-ext4 (a 5-byte file) and changes it on a copy of the image to a symlink of size 107, with
// the checksum calculated again. The check is that readInode returns ErrInlineData, not a panic.
func TestQizInlineDataSymlinkInode(t *testing.T) {
	const ino = 30
	img := qizOpenDisk(t, qizGPTExt4)
	open := func() *FileSystem {
		t.Helper()
		b, err := file.OpenFromPath(img, true)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		fsys, err := Read(b, qizGPTExt4.size, qizGPTExt4.offset, 512)
		if err != nil {
			t.Fatal(err)
		}
		return fsys
	}
	raw, err := open().readInodeRaw(ino)
	if err != nil {
		t.Fatal(err)
	}
	orig := append([]byte(nil), raw...)
	fsys := open()
	// the position of the inode in the image, as readInodeRaw finds it
	sb := fsys.superblock
	gd := fsys.groupDescriptors.descriptors[(ino-1)/sb.inodesPerGroup]
	pos := qizGPTExt4.offset + int64(gd.inodeTableLocation)*int64(sb.blockSize) + int64((ino-1)%sb.inodesPerGroup)*int64(sb.inodeSize)
	data := make([]byte, len(orig))
	rf, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.ReadAt(data, pos); err != nil || !bytes.Equal(data, orig) {
		t.Fatalf("fixture check: the bytes at %d are not inode %d (err %v)", pos, ino, err)
	}
	rf.Close()
	mut := append([]byte(nil), orig...)
	binary.LittleEndian.PutUint16(mut[0:2], 0o120777)
	binary.LittleEndian.PutUint32(mut[4:8], 107)
	mut[0x7c], mut[0x7d], mut[0x82], mut[0x83] = 0, 0, 0, 0
	sum := inodeChecksum(mut, fsys.superblock.checksumSeed, ino, binary.LittleEndian.Uint32(mut[0x64:0x68]))
	binary.LittleEndian.PutUint16(mut[0x7c:0x7e], uint16(sum))
	binary.LittleEndian.PutUint16(mut[0x82:0x84], uint16(sum>>16))
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(mut, int64(pos)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fsys = open()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("readInode panicked: %v", p)
		}
	}()
	if _, err := fsys.readInode(ino); !errors.Is(err, ErrInlineData) {
		t.Errorf("readInode of an inline symlink: got %v, want ErrInlineData", err)
	}
}
