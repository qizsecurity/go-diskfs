package ext4

import (
	"encoding/binary"
	"testing"
)

// QIZ-4909: the oracle rows of var/data/data.bin, prealloc.bin (unwritten extents) and
// sparse.bin (holes) must read back with the bytes the kernel gave.
func TestQizReadHolesAndUnwrittenExtents(t *testing.T) {
	for _, d := range []qizDisk{qizMBRExt4, qizNoneExt4} {
		t.Run(d.name, func(t *testing.T) {
			fsys, err := qizReadFS(t, d)
			if err != nil {
				t.Fatalf("open %s: %v", d.name, err)
			}
			want := map[string]qizOracleRow{}
			for _, r := range qizLoadOracle(t, d) {
				want[r.path] = r
			}
			for _, p := range []string{"var/data/data.bin", "var/data/prealloc.bin", "var/data/sparse.bin"} {
				r, ok := want[p]
				if !ok {
					t.Fatalf("no oracle row for %s", p)
				}
				if diffs := qizCompareRow(fsys, r); len(diffs) > 0 {
					t.Errorf("%s: %v", p, diffs)
				}
			}
		})
	}
}

// Every oracle row of the two plain ext4 disks (type, size, perm, mtime, sha256, link target) matches.
func TestQizExt4AllOracleRows(t *testing.T) {
	for _, d := range []qizDisk{qizMBRExt4, qizNoneExt4} {
		t.Run(d.name, func(t *testing.T) {
			fsys, err := qizReadFS(t, d)
			if err != nil {
				t.Fatalf("open %s: %v", d.name, err)
			}
			diffs := qizCompareAll(t, fsys, qizLoadOracle(t, d))
			for _, p := range qizSortedKeys(diffs) {
				t.Errorf("%s: %v", p, diffs[p])
			}
		})
	}
}

// The same files, read with buffers that do not line up with the 1024-byte blocks or the
// extent borders, must give the same bytes.
func TestQizReadHolesOddBuffers(t *testing.T) {
	fsys, err := qizReadFS(t, qizMBRExt4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, p := range []string{"var/data/prealloc.bin", "var/data/sparse.bin"} {
		st, err := fsys.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		whole, err := qizReadChunks(fsys, p, st.Size(), 1<<20)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if int64(len(whole)) != st.Size() {
			t.Fatalf("%s: read %d bytes, want %d", p, len(whole), st.Size())
		}
		for _, chunk := range []int{1, 7, 1023, 1025, 4097} {
			if chunk == 1 && st.Size() > 1<<20 {
				continue // one byte at a time over 4 MB is only slow, not different
			}
			got, err := qizReadChunks(fsys, p, st.Size(), chunk)
			if err != nil {
				t.Fatalf("%s chunk %d: %v", p, chunk, err)
			}
			if string(got) != string(whole) {
				t.Errorf("%s chunk %d: bytes differ from the whole-file read", p, chunk)
			}
		}
	}
}

// kernel fs/ext4/ext4_extents.h: EXT_INIT_MAX_LEN = 1<<15. An ee_len above it is an
// unwritten extent of ee_len-32768 blocks; ee_len == 32768 is a written extent of 32768 blocks.
func TestQizExtentUnwrittenLength(t *testing.T) {
	leaf := func(eeLen uint16) []byte {
		b := make([]byte, 12+12*4)
		binary.LittleEndian.PutUint16(b[0:2], extentHeaderSignature)
		binary.LittleEndian.PutUint16(b[2:4], 1) // entries
		binary.LittleEndian.PutUint16(b[4:6], 4) // max
		binary.LittleEndian.PutUint16(b[6:8], 0) // depth
		binary.LittleEndian.PutUint32(b[12:16], 7)
		binary.LittleEndian.PutUint16(b[16:18], eeLen)
		binary.LittleEndian.PutUint32(b[20:24], 1000) // ee_start_lo
		return b
	}
	for _, tt := range []struct {
		eeLen     uint16
		count     uint16
		unwritten bool
	}{
		{eeLen: 16, count: 16},
		{eeLen: 32768, count: 32768},
		{eeLen: 32768 + 16, count: 16, unwritten: true},
		{eeLen: 65535, count: 32767, unwritten: true},
	} {
		in := leaf(tt.eeLen)
		f, err := parseExtents(in, 1024, 0, 0)
		if err != nil {
			t.Fatalf("ee_len %d: %v", tt.eeLen, err)
		}
		exts, err := f.blocks(nil)
		if err != nil || len(exts) != 1 {
			t.Fatalf("ee_len %d: blocks %v err %v", tt.eeLen, exts, err)
		}
		if exts[0].count != tt.count || exts[0].unwritten != tt.unwritten {
			t.Errorf("ee_len %d: got count %d unwritten %v, want count %d unwritten %v",
				tt.eeLen, exts[0].count, exts[0].unwritten, tt.count, tt.unwritten)
		}
		// writing the node back must give the same ee_len
		if got := f.toBytes(); string(got[12:24]) != string(in[12:24]) {
			t.Errorf("ee_len %d: toBytes gave % x, want % x", tt.eeLen, got[12:24], in[12:24])
		}
	}
}
