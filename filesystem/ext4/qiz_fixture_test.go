package ext4

// Helpers for the tests that read the QIZ fixture disks (QIZ-4909, QIZ-4910 and the
// inline-data panic). These tests need the fixture folder: set DISKFS_QIZ_FIXTURES to
// the folder that holds <disk>.raw.zst (mbr-ext4, gpt-ext3, gpt-ext4, none-ext4). They are
// skipped when it is unset.
//
// The expected values are the rows the Linux kernel gave when it mounted the same disks
// (testdata/qiz-oracle/<disk>.oracle.tsv; columns: type, path, size, perm, mtime, sha256,
// link target). They are not produced by this package.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/diskfs/go-diskfs/backend/file"
)

const qizFixturesEnv = "DISKFS_QIZ_FIXTURES"

// qizDisk describes where the ext filesystem is on a fixture disk. Offsets and sizes are
// the sfdisk start x 512 and size x 512 of the partition (none-ext4 has no partition table).
type qizDisk struct {
	name   string
	offset int64
	size   int64
}

var (
	qizMBRExt4  = qizDisk{"mbr-ext4", 2048 * 512, 129024 * 512}
	qizGPTExt3  = qizDisk{"gpt-ext3", 2048 * 512, 128991 * 512}
	qizGPTExt4  = qizDisk{"gpt-ext4", 2048 * 512, 128991 * 512}
	qizNoneExt4 = qizDisk{"none-ext4", 0, 64 * 1024 * 1024}
)

// qizOpenDisk decompresses the disk into a temporary folder and returns the image path.
func qizOpenDisk(t *testing.T, d qizDisk) string {
	t.Helper()
	dir := os.Getenv(qizFixturesEnv)
	if dir == "" {
		t.Skipf("%s is not set; the QIZ fixture disks are needed", qizFixturesEnv)
	}
	in, err := os.Open(filepath.Join(dir, d.name+".raw.zst"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer in.Close()
	zr, err := zstd.NewReader(in, zstd.WithDecoderMaxWindow(1<<30))
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	img := filepath.Join(t.TempDir(), d.name+".raw")
	out, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		t.Fatalf("decompress %s: %v", d.name, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
	return img
}

// qizReadFS opens the filesystem of a fixture disk. It returns the error of Read to the
// caller, so that a test can check it.
func qizReadFS(t *testing.T, d qizDisk) (*FileSystem, error) {
	t.Helper()
	img := qizOpenDisk(t, d)
	b, err := file.OpenFromPath(img, true)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return Read(b, d.size, d.offset, 512)
}

// qizOracleRow is one line of the kernel oracle.
type qizOracleRow struct {
	typ, path, size, perm, mtime, sha, link string
}

func qizLoadOracle(t *testing.T, d qizDisk) []qizOracleRow {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "qiz-oracle", d.name+".oracle.tsv"))
	if err != nil {
		t.Fatalf("open oracle: %v", err)
	}
	defer f.Close()
	var rows []qizOracleRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		c := strings.Split(sc.Text(), "\t")
		if len(c) != 7 {
			t.Fatalf("oracle line has %d columns, want 7: %q", len(c), sc.Text())
		}
		// the oracle prints the time with 10 fractional digits; the tenth is a 0 after the
		// nanoseconds. Keep the nanoseconds only when that is so.
		mtime := c[4]
		if i := strings.IndexByte(mtime, '.'); i >= 0 && len(mtime)-i-1 == 10 && strings.HasSuffix(mtime, "0") {
			mtime = mtime[:len(mtime)-1]
		}
		rows = append(rows, qizOracleRow{c[0], c[1], c[2], c[3], mtime, c[5], c[6]})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	return rows
}

// qizRead reads a whole file of size bytes. A Read that returns no bytes, no error and no
// EOF before the size is reached is a stall (callers such as io.ReadAll would loop for ever):
// it is returned as an error.
func qizRead(fsys *FileSystem, p string, size int64) ([]byte, error) {
	return qizReadChunks(fsys, p, size, 32*1024)
}

// qizReadChunks is qizRead with a chosen buffer size. The buffer is filled with 0xAA before
// every Read, so a Read that leaves bytes of the buffer untouched is seen.
func qizReadChunks(fsys *FileSystem, p string, size int64, chunk int) ([]byte, error) {
	f, err := fsys.OpenFile(p, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]byte, 0, size)
	buf := make([]byte, chunk)
	for {
		for i := range buf {
			buf[i] = 0xAA
		}
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if n == 0 {
			return out, fmt.Errorf("read stalled at offset %d of %d: 0 bytes, no error", len(out), size)
		}
		if int64(len(out)) > size {
			return out, fmt.Errorf("read more than the size %d", size)
		}
	}
}

// qizCompareRow compares one oracle row with what the reader reports. It returns the names
// of the fields that differ, each with both values. Nothing is returned when all equal.
func qizCompareRow(fsys *FileSystem, r qizOracleRow) []string {
	var diffs []string
	st, err := fsys.Stat(r.path)
	if err != nil {
		return []string{"stat: " + err.Error()}
	}
	mode := st.Mode()
	typ := "f"
	switch {
	case mode.IsDir():
		typ = "d"
	case mode&iofs.ModeSymlink != 0:
		typ = "l"
	case mode&iofs.ModeNamedPipe != 0:
		typ = "p"
	case mode&iofs.ModeCharDevice != 0:
		typ = "c"
	case mode&iofs.ModeDevice != 0:
		typ = "b"
	case mode&iofs.ModeSocket != 0:
		typ = "s"
	}
	perm := uint32(mode.Perm())
	if mode&iofs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if mode&iofs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if mode&iofs.ModeSticky != 0 {
		perm |= 0o1000
	}
	mt := st.ModTime()
	got := qizOracleRow{
		typ:   typ,
		size:  fmt.Sprint(st.Size()),
		perm:  fmt.Sprintf("%o", perm),
		mtime: fmt.Sprintf("%d.%09d", mt.Unix(), mt.Nanosecond()),
		sha:   "-",
		link:  "-",
	}
	skip := map[string]bool{}
	if typ == "l" {
		l, err := fsys.ReadLink(r.path)
		if err != nil {
			diffs = append(diffs, "readlink: "+err.Error())
			skip["link"] = true
		} else {
			got.link = l
		}
	}
	if typ == "f" {
		b, err := qizRead(fsys, r.path, st.Size())
		if err != nil {
			diffs = append(diffs, "read: "+err.Error())
			skip["sha256"] = true
		} else {
			sum := sha256.Sum256(b)
			got.sha = hex.EncodeToString(sum[:])
		}
	}
	for _, c := range []struct{ name, want, got string }{
		{"type", r.typ, got.typ},
		{"size", r.size, got.size},
		{"perm", r.perm, got.perm},
		{"mtime", r.mtime, got.mtime},
		{"sha256", r.sha, got.sha},
		{"link", r.link, got.link},
	} {
		if c.want != c.got && !skip[c.name] {
			diffs = append(diffs, fmt.Sprintf("%s: want %s got %s", c.name, c.want, c.got))
		}
	}
	return diffs
}

// qizCompareAll compares every oracle row and returns a map path -> differences.
func qizCompareAll(t *testing.T, fsys *FileSystem, rows []qizOracleRow) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range rows {
		if d := qizCompareRow(fsys, r); len(d) > 0 {
			out[r.path] = d
		}
	}
	return out
}

func qizSortedKeys(m map[string][]string) []string {
	k := make([]string, 0, len(m))
	for p := range m {
		k = append(k, p)
	}
	sort.Strings(k)
	return k
}
