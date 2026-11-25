package fat16_test

// A FAT16 volume whose BPB states a FAT and a volume far larger than the file (Microsoft FAT32 File System
// Specification: BPB_FATSz16, BPB_TotSec32, with the cluster count in the FAT16 range): Read refuses it before it
// allocates the FAT. The BPB is self-consistent (the cluster count stays under 65525), so only the file's own
// size shows that the FAT does not fit.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/fat16"
)

func TestHostileFAT16LargerThanVolumeIsRefused(t *testing.T) {
	const volume = 40 << 20
	p := filepath.Join(t.TempDir(), "v.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(volume); err != nil {
		t.Fatal(err)
	}
	if _, err := fat16.Create(file.New(f, false), volume, 0, 512, "HOSTILE", false); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// FAT of 65535 sectors (32 MiB), 64 sectors per cluster, 4,194,304 sectors stated (2 GiB): about 65 000 clusters.
	binary.LittleEndian.PutUint16(b[0x16:], 0xFFFF)
	b[0x0D] = 64
	binary.LittleEndian.PutUint16(b[0x13:], 0)
	binary.LittleEndian.PutUint32(b[0x20:], 4194304)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_, rerr := fat16.Read(file.New(g, true), volume, 0, 512)
	runtime.ReadMemStats(&m1)
	alloc := m1.TotalAlloc - m0.TotalAlloc
	t.Logf("error: %v; allocated %d KiB", rerr, alloc>>10)
	if rerr == nil {
		t.Fatal("Read accepted a 32 MiB FAT in a 40 MiB volume with 2 copies")
	}
	if !strings.Contains(rerr.Error(), "FAT does not fit") {
		t.Errorf("want the FAT-does-not-fit refusal, got %v", rerr)
	}
	if alloc > 8<<20 {
		t.Errorf("allocated %d bytes, want at most %d", alloc, 8<<20)
	}
}
