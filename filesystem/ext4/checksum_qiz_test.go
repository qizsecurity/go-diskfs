package ext4

import (
	"io"
	"os"
	"strings"
	"testing"
)

// qizSuperblockBytes returns the 1024 superblock bytes of a fixture disk.
func qizSuperblockBytes(t *testing.T, d qizDisk) []byte {
	t.Helper()
	img := qizOpenDisk(t, d)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, SuperblockSize)
	if _, err := f.ReadAt(b, d.offset+int64(BootSectorSize)); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return b
}

// QIZ-4910: the checksum type (s_checksum_type) and the inode checksum are only used when the
// metadata_csum feature is on (Linux fs/ext4/super.c ext4_verify_csum_type,
// fs/ext4/inode.c ext4_inode_csum_verify). gpt-ext3 has no metadata_csum; its s_checksum_type is 0.
func TestQizNoMetadataCsumSuperblock(t *testing.T) {
	b := qizSuperblockBytes(t, qizGPTExt3)
	if b[0x175] != 0 {
		t.Fatalf("fixture check: s_checksum_type of gpt-ext3 is %d, expected 0", b[0x175])
	}
	sb, err := superblockFromBytes(b)
	if err != nil {
		t.Fatalf("superblock of an ext3 filesystem (no metadata_csum, checksum type 0) refused: %v", err)
	}
	if sb.features.metadataChecksums {
		t.Errorf("gpt-ext3 reported with metadata_csum")
	}
}

// With metadata_csum on, a checksum type other than crc32c is still refused.
func TestQizMetadataCsumWrongTypeRefused(t *testing.T) {
	b := qizSuperblockBytes(t, qizMBRExt4)
	if b[0x175] != checkSumTypeCRC32c {
		t.Fatalf("fixture check: s_checksum_type of mbr-ext4 is %d, expected %d", b[0x175], checkSumTypeCRC32c)
	}
	b[0x175] = 0
	if _, err := superblockFromBytes(b); err == nil || !strings.Contains(err.Error(), "invalid checksum type") {
		t.Fatalf("metadata_csum with checksum type 0: got err %v, want invalid checksum type", err)
	}
}

// The filesystem opens, and the root inode (read with the inode checksum check) is read.
func TestQizNoMetadataCsumOpens(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt3)
	if err != nil {
		t.Fatalf("open gpt-ext3: %v", err)
	}
	in, err := fsys.readInode(rootInode)
	if err != nil {
		t.Fatalf("read root inode of gpt-ext3: %v", err)
	}
	if in.fileType != fileTypeDirectory {
		t.Errorf("root inode type %v, want directory", in.fileType)
	}
}

// The inode checksum is still checked with metadata_csum on, and not checked with it off:
// the same one-byte change to the raw root inode (a byte of i_atime) is refused on mbr-ext4
// and accepted on gpt-ext3.
func TestQizInodeChecksumOnlyWithMetadataCsum(t *testing.T) {
	for _, tt := range []struct {
		disk    qizDisk
		wantErr bool
	}{
		{qizMBRExt4, true},
		{qizGPTExt3, false},
	} {
		t.Run(tt.disk.name, func(t *testing.T) {
			fsys, err := qizReadFS(t, tt.disk)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			raw, err := fsys.readInodeRaw(rootInode)
			if err != nil {
				t.Fatalf("read raw root inode: %v", err)
			}
			if _, err := inodeFromBytes(raw, fsys.superblock, rootInode); err != nil {
				t.Fatalf("unchanged root inode refused: %v", err)
			}
			raw[0x08] ^= 0xff
			_, err = inodeFromBytes(raw, fsys.superblock, rootInode)
			if tt.wantErr && (err == nil || !strings.Contains(err.Error(), "checksum mismatch")) {
				t.Errorf("changed inode, metadata_csum on: got err %v, want checksum mismatch", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("changed inode, metadata_csum off: got err %v, want none", err)
			}
		})
	}
}
