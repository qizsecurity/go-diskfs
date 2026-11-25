package ext4

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

// The group descriptor checksum of a file system made with uninit_bg (gdt_csum, no metadata_csum) is the kernel's
// crc16 (fs/ext4/super.c ext4_group_desc_csum, v6.12 lines 3223-3230; lib/crc16.c), not the crc32c of metadata_csum.
//
// The two files are real file systems made by mke2fs 1.47.4 (6-Mar-2025), each after `truncate -s 1M <file>`:
//
//	E2FSPROGS_FAKE_TIME=1700000000 mke2fs -q -F -t ext4 -b 1024 -g 256 -N 32 -O ^has_journal,^metadata_csum,uninit_bg,64bit \
//	  -U 11111111-2222-3333-4444-555555555555 -E hash_seed=66666666-7777-8888-9999-000000000000 -L gdtcsum qiz-gdtcsum.img
//	E2FSPROGS_FAKE_TIME=1700000000 mke2fs -q -F -t ext4 -b 1024 -g 256 -N 32 -O ^has_journal,metadata_csum,64bit \
//	  -U 11111111-2222-3333-4444-555555555555 -E hash_seed=66666666-7777-8888-9999-000000000000 -L mdcsum qiz-mdcsum.img
//
// e2fsck -fn is clean on both. The stored checksums of the 4 groups of qiz-gdtcsum.img (7d04, c07e, 0429, a0d7) equal the kernel
// formula computed independently over the file's bytes.
func TestQizGroupDescriptorChecksumOfMkfsFileSystems(t *testing.T) {
	for _, tt := range []struct {
		name, sha string
		gdt       bool
	}{
		{"qiz-gdtcsum.img", "d79d81cd40c8134b6971a75eb90b5da7d9bad48c14bdecbf1d08092c45dbb744", true},
		{"qiz-mdcsum.img", "2737138cd499255475a3079965d83444efc0353e26d0766b42b93979a4a7b93f", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := "testdata/" + tt.name
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != tt.sha {
				t.Fatalf("%s: sha256 %s, want %s", path, got, tt.sha)
			}
			b, err := file.OpenFromPath(path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			fsys, err := Read(b, int64(len(raw)), 0, 512)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got := fsys.superblock.features.gdtChecksum && !fsys.superblock.features.metadataChecksums; got != tt.gdt {
				t.Fatalf("gdt_csum without metadata_csum is %v, want %v", got, tt.gdt)
			}
			if n := len(fsys.groupDescriptors.descriptors); n != 4 {
				t.Errorf("%d group descriptors, want 4", n)
			}
		})
	}
}
