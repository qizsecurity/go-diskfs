package ext4

import (
	"errors"
	"testing"
)

// gpt-ext3 has no extent feature: its directories and files are block-mapped (i_block holds
// block pointers; measured: the root inode has block pointer 760 and no extent flag). This
// package reads extent trees only. Reading a block-mapped directory used to dereference the nil
// extent tree (panic in readDirectory); it must return ErrNoExtentTree.
func TestQizBlockMappedNoPanic(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt3)
	if err != nil {
		t.Fatalf("open gpt-ext3: %v", err)
	}
	_, refused, panicked := qizWalk(fsys)
	if panicked != nil {
		t.Fatalf("walk panicked: %v", panicked)
	}
	if err := refused["."]; !errors.Is(err, ErrNoExtentTree) {
		t.Errorf("ReadDir of the root: got %v, want ErrNoExtentTree", err)
	}
}

// Every oracle row of gpt-ext3 fails with ErrNoExtentTree, none for another reason or with a
// wrong value: the rows are all below the block-mapped root directory.
func TestQizBlockMappedOracleRows(t *testing.T) {
	fsys, err := qizReadFS(t, qizGPTExt3)
	if err != nil {
		t.Fatalf("open gpt-ext3: %v", err)
	}
	rows := qizLoadOracle(t, qizGPTExt3)
	for _, r := range rows {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s: panic: %v", r.path, p)
				}
			}()
			diffs := qizCompareRow(fsys, r)
			if len(diffs) == 0 {
				t.Logf("%s: matches the oracle", r.path)
			}
			for _, d := range diffs {
				if d.err == nil || !errors.Is(d.err, ErrNoExtentTree) {
					t.Errorf("%s: %v", r.path, d)
				}
			}
		}()
	}
}
