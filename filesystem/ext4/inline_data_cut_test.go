package ext4

// Tests of inline data (inline_data.go) on a real file system: testdata/qiz-inline/gpt-ext4-metadata.ext4.zst is
// the ext4 file system of disk gpt-ext4.raw of corpus image qiz-fixtures/disk-images:v1 (disk sha256
// d8a88cc4fa48f174905af7ce6004703f57e733561807d9d0701dc995a6c0ac44, made by mke2fs -O inline_data, filled on a
// mounted Linux kernel), cut to its METADATA: the superblock and group descriptors, the bitmaps, every inode table
// and the blocks of the extent-mapped directories. No file content block is in it (they read as zeros), so the
// content of a file is only checked for the files that are inline (their bytes are in the inode).
// Cut on Cloud Batch, run ext4c-3 (2026-10-28); the file system is 66039808 bytes at offset 0.
// The expected rows are the kernel's own, testdata/qiz-oracle/gpt-ext4.oracle.tsv (columns: type, path, size,
// perm, mtime, sha256, link target).

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/diskfs/go-diskfs/backend/file"
)

const qizInlineFSSize = 66039808

func qizOpenInlineCut(t *testing.T) *FileSystem {
	t.Helper()
	in, err := os.Open(filepath.Join("testdata", "qiz-inline", "gpt-ext4-metadata.ext4.zst"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	zr, err := zstd.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	img := filepath.Join(t.TempDir(), "gpt-ext4-metadata.ext4")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := file.OpenFromPath(img, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	fsys, err := Read(b, qizInlineFSSize, 0, 512)
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// The walk of the whole tree from the root, with ReadDir only, must list exactly the paths of the kernel oracle
// with the same type and size. /etc, /usr, /var and every directory under them are inline directories (inodes
// 13-18, 2017, 2018); /var/data (size 128) has entries in both parts, i_block and system.data.
func TestInlineDataTreeMatchesKernelOracle(t *testing.T) {
	fsys := qizOpenInlineCut(t)
	type row struct {
		typ  string
		size int64
	}
	want := map[string]row{}
	for _, r := range qizLoadOracle(t, qizDisk{name: "gpt-ext4"}) {
		n, err := strconv.ParseInt(r.size, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		want[r.path] = row{r.typ, n}
	}
	got := map[string]row{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			t.Errorf("ReadDir(%q): %v", dir, err)
			return
		}
		for _, e := range entries {
			p := e.Name()
			if dir != "." {
				p = dir + "/" + e.Name()
			}
			info, err := e.Info()
			if err != nil {
				t.Errorf("Info(%q): %v", p, err)
				continue
			}
			typ := "f"
			switch {
			case e.IsDir():
				typ = "d"
			case info.Mode()&os.ModeSymlink != 0:
				typ = "l"
			case info.Mode()&os.ModeNamedPipe != 0:
				typ = "p"
			case info.Mode()&os.ModeCharDevice != 0:
				typ = "c"
			}
			got[p] = row{typ, info.Size()}
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk(".")
	var paths []string
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if g, ok := got[p]; !ok {
			t.Errorf("%s: in the kernel oracle (%v), not listed", p, want[p])
		} else if g != want[p] {
			t.Errorf("%s: listed %v, kernel oracle %v", p, g, want[p])
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: listed, not in the kernel oracle", p)
		}
	}
	if len(got) == 0 {
		t.Fatal("nothing listed")
	}
}

// The two regular files whose data is inline (inode flag 0x10000000, 5 bytes each, all in i_block) read as the
// kernel read them: sha256 and size of the oracle rows.
func TestInlineDataFilesMatchKernelOracle(t *testing.T) {
	fsys := qizOpenInlineCut(t)
	rows := map[string]qizOracleRow{}
	for _, r := range qizLoadOracle(t, qizDisk{name: "gpt-ext4"}) {
		rows[r.path] = r
	}
	for _, p := range []string{"tiny.txt", "usr/bin/suid-tool"} {
		r, ok := rows[p]
		if !ok {
			t.Fatalf("%s is not in the oracle", p)
		}
		f, err := fsys.OpenFile(p, os.O_RDONLY)
		if err != nil {
			t.Errorf("OpenFile(%s): %v", p, err)
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			continue
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != r.sha || strconv.Itoa(len(data)) != r.size {
			t.Errorf("%s: %d bytes sha256 %s, kernel oracle %s bytes sha256 %s", p, len(data), got, r.size, r.sha)
		}
	}
}
