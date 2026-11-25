package gpt_test

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// writeBoundDisk writes a 4 MiB disk with a valid GPT of one partition, then lets edit change the 512-byte primary
// header (at LBA 1) and the 512-byte backup header (at the last LBA).
func writeBoundDisk(t *testing.T, edit func(primary, backup []byte)) string {
	t.Helper()
	const size = 4 << 20
	path := filepath.Join(t.TempDir(), "disk.img")
	st, err := file.CreateFromPath(path, size)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := st.Writable()
	if err != nil {
		t.Fatal(err)
	}
	table := &gpt.Table{LogicalSectorSize: 512, PhysicalSectorSize: 512, ProtectiveMBR: true,
		Partitions: []*gpt.Partition{{Index: 1, Start: 2048, End: 6143, Type: gpt.LinuxFilesystem, Name: "data"}}}
	if err := table.Write(wf, size); err != nil {
		t.Fatal(err)
	}
	st.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	last := int64(size/512-1) * 512
	var primary, backup [512]byte
	f.ReadAt(primary[:], 512)
	f.ReadAt(backup[:], last)
	edit(primary[:], backup[:])
	f.WriteAt(primary[:], 512)
	f.WriteAt(backup[:], last)
	return path
}

// setHeaderEntries sets NumberOfPartitionEntries and SizeOfPartitionEntry (UEFI 2.10 section 5.3.2, Table 5.5, offsets 80
// and 84), then recomputes HeaderCRC32 (offset 16, over HeaderSize bytes with that field zero), so the header is a
// well-formed one that states an array of count x size bytes.
func setHeaderEntries(h []byte, count, size uint32) {
	binary.LittleEndian.PutUint32(h[80:84], count)
	binary.LittleEndian.PutUint32(h[84:88], size)
	fixHeaderCRC(h)
}

func fixHeaderCRC(h []byte) {
	binary.LittleEndian.PutUint32(h[16:20], 0)
	hs := binary.LittleEndian.Uint32(h[12:16])
	binary.LittleEndian.PutUint32(h[16:20], crc32.ChecksumIEEE(h[:hs]))
}

func readAllocated(t *testing.T, path string) (uint64, *gpt.Table, error) {
	t.Helper()
	ro, err := file.OpenFromPath(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	table, rerr := gpt.Read(ro, 512, 512)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, table, rerr
}

// A header states an entry array larger than the disk: Read must refuse it before it allocates the array. The primary
// header's CRC is broken so that Read falls back to the backup header, whose array is 0x100000 x 0x1000 = 4 GiB.
func TestReadRefusesBackupEntryArrayLargerThanDisk(t *testing.T) {
	path := writeBoundDisk(t, func(primary, backup []byte) {
		primary[16] ^= 0xff
		setHeaderEntries(backup, 0x00100000, 0x1000)
	})
	alloc, table, err := readAllocated(t, path)
	if err == nil || table != nil {
		t.Fatalf("expected an error and no table, got table %v, err %v", table, err)
	}
	if !strings.Contains(err.Error(), "partition entry array") {
		t.Errorf("error does not say why: %v", err)
	}
	if alloc > 8<<20 {
		t.Errorf("Read allocated %d MiB for a 4 MiB disk", alloc>>20)
	}
}

// The same for the primary header, whose array is the one Read allocates first.
func TestReadRefusesPrimaryEntryArrayLargerThanDisk(t *testing.T) {
	path := writeBoundDisk(t, func(primary, backup []byte) {
		setHeaderEntries(primary, 0x00100000, 0x1000)
		setHeaderEntries(backup, 0x00100000, 0x1000)
	})
	alloc, table, err := readAllocated(t, path)
	if err == nil || table != nil {
		t.Fatalf("expected an error and no table, got table %v, err %v", table, err)
	}
	if !strings.Contains(err.Error(), "partition entry array") {
		t.Errorf("error does not say why: %v", err)
	}
	if alloc > 8<<20 {
		t.Errorf("Read allocated %d MiB for a 4 MiB disk", alloc>>20)
	}
}

// Count x size that wraps a 32-bit product: 0xFFFFFFFF x 0xFFFFFFFF.
func TestReadRefusesEntryArrayThatOverflows(t *testing.T) {
	path := writeBoundDisk(t, func(primary, backup []byte) {
		setHeaderEntries(primary, 0xFFFFFFFF, 0xFFFFFFFF)
		setHeaderEntries(backup, 0xFFFFFFFF, 0xFFFFFFFF)
	})
	_, table, err := readAllocated(t, path)
	if err == nil || table != nil {
		t.Fatalf("expected an error and no table, got table %v, err %v", table, err)
	}
}

// A valid disk still reads, and the bound is not a stricter rule than the disk: the array of a valid disk is read.
func TestReadValidDiskStillReads(t *testing.T) {
	path := writeBoundDisk(t, func(primary, backup []byte) {})
	_, table, err := readAllocated(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Partitions) != 1 || table.Partitions[0].Name != "data" {
		t.Errorf("unexpected partitions %+v", table.Partitions)
	}
}

// The bound refuses the primary header's array, the backup header and its array are valid: Read still returns the table,
// from the backup (the refusal is a content error, so the backup is tried).
func TestReadFallsBackToBackupWhenBoundRefusesPrimary(t *testing.T) {
	path := writeBoundDisk(t, func(primary, backup []byte) {
		setHeaderEntries(primary, 0x00100000, 0x1000)
	})
	_, table, err := readAllocated(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !table.RecoveredFromBackup || len(table.Partitions) != 1 || table.Partitions[0].Name != "data" {
		t.Errorf("RecoveredFromBackup=%v partitions=%+v", table.RecoveredFromBackup, table.Partitions)
	}
}

// moveArray copies the primary entry array (LBA 2, 128 entries x 128 bytes = 32 sectors) of a valid 4 MiB disk to
// startLBA and points the primary header's PartitionEntryLBA there. The last LBA of the disk is 8191, so startLBA 8160
// makes the array end exactly at the disk's last byte, and 8161 one sector past it.
func moveArray(t *testing.T, startLBA uint64) string {
	t.Helper()
	path := writeBoundDisk(t, func(primary, backup []byte) {})
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const arrayBytes = 128 * 128
	array := make([]byte, arrayBytes)
	if _, err := f.ReadAt(array, 2*512); err != nil {
		t.Fatal(err)
	}
	// the array is written whole when it fits; the part past the end of the disk is not written
	end := int64(4 << 20)
	at := int64(startLBA) * 512
	if _, err := f.WriteAt(array[:min(int64(arrayBytes), end-at)], at); err != nil {
		t.Fatal(err)
	}
	var primary [512]byte
	f.ReadAt(primary[:], 512)
	binary.LittleEndian.PutUint64(primary[72:80], startLBA)
	fixHeaderCRC(primary[:])
	f.WriteAt(primary[:], 512)
	// the backup header is overwritten by the array when it ends at the disk's last byte, as the layout says it must be
	return path
}

// An array that ends exactly at the disk's last byte is read from the primary header (the bound is inclusive); one
// that ends a sector past the disk is refused with the reason.
func TestReadEntryArrayEndingAtLastByteOfDisk(t *testing.T) {
	_, table, err := readAllocated(t, moveArray(t, 8160))
	if err != nil {
		t.Fatal(err)
	}
	if table.RecoveredFromBackup || len(table.Partitions) != 1 {
		t.Errorf("RecoveredFromBackup=%v partitions=%d", table.RecoveredFromBackup, len(table.Partitions))
	}
	_, table, err = readAllocated(t, moveArray(t, 8161))
	if table != nil || err == nil || !strings.Contains(err.Error(), "partition entry array") || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("an array one sector past the disk: table %v, err %v", table, err)
	}
}
