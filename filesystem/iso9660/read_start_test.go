package iso9660_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// A volume that begins at a non-zero start of the backend is read at that start in every structure: the root directory record and its SUSP area
// (ECMA-119 8.4.18, 9.1.3: locations are block numbers from the start of the volume, 6.2.1), not only the volume descriptors.
func TestISO9660ReadAtStart(t *testing.T) {
	for _, name := range []string{iso9660.ISO9660File, iso9660.RockRidgeFile} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		const start = 4096
		p := filepath.Join(t.TempDir(), "at.bin")
		if err := os.WriteFile(p, append(make([]byte, start), raw...), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := iso9660.Read(file.New(f, true), int64(len(raw)), start, 2048)
		if err != nil {
			f.Close()
			t.Fatalf("%s at %d: %v", name, start, err)
		}
		g, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := iso9660.Read(file.New(g, true), int64(len(raw)), 0, 2048)
		if err != nil {
			t.Fatal(err)
		}
		a, err1 := fs.ReadDir(".")
		b, err2 := want.ReadDir(".")
		if err1 != nil || err2 != nil || len(a) != len(b) || len(a) == 0 {
			t.Errorf("%s: root at %d has %d entries (%v), at 0 %d (%v)", name, start, len(a), err1, len(b), err2)
		}
		f.Close()
		g.Close()
	}
}
