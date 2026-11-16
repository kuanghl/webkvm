package sshmachines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// memFS already satisfies SizeFS (Stat + ReadDir); this is what keeps the
// walk testable without a live SFTP server.
var _ SizeFS = (*memFS)(nil)

func seedSizeTree(t *testing.T, fs *memFS) {
	t.Helper()
	fs.writeFile(t, "/d/a.txt", "12345", 0o644)            // 5
	fs.writeFile(t, "/d/sub/b.bin", "abcdef", 0o644)       // 6
	fs.writeFile(t, "/d/sub/deep/c.dat", "0123456", 0o644) // 7
	if err := fs.MkdirAll("/d/empty"); err != nil {
		t.Fatal(err)
	}
}

// The sum is over regular files only: directory FileInfos are block
// sizes, symlinks are neither followed nor counted, and special files
// would be a crawl into something that is not a file at all.
func TestFolderSizeSumsRegularFilesOnly(t *testing.T) {
	fs := newMemFS()
	seedSizeTree(t, fs)
	if err := fs.Symlink("/d/a.txt", "/d/link"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("/d/sub", "/d/linkdir"); err != nil {
		t.Fatal(err)
	}
	fs.writeRaw("/d/pipe", &memNode{mode: os.ModeNamedPipe | 0o644})

	size, truncated, err := FolderSize(context.Background(), fs, "/d", DefaultSizeLimits)
	if err != nil {
		t.Fatalf("FolderSize: %v", err)
	}
	if size != 18 {
		t.Fatalf("size = %d, want 18 (5+6+7, nothing else)", size)
	}
	if truncated {
		t.Fatalf("a small complete tree must not read as truncated")
	}
}

// A missing root is an error, not a zero: the endpoint renders per-path
// errors, and a silent 0 would read as "empty folder".
func TestFolderSizeMissingRootIsAnError(t *testing.T) {
	fs := newMemFS()
	if _, _, err := FolderSize(context.Background(), fs, "/nope", DefaultSizeLimits); err == nil {
		t.Fatal("FolderSize(/nope) = no error, want one")
	}
}

// A file "folder" is just itself; the endpoint never asks for one, but
// the walk stays total for a direct caller.
func TestFolderSizeOnAFileReturnsItsOwnSize(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/f", "12345678", 0o644)
	size, truncated, err := FolderSize(context.Background(), fs, "/f", DefaultSizeLimits)
	if err != nil || size != 8 || truncated {
		t.Fatalf("FolderSize(/f) = %d, %v, %v; want 8, false, nil", size, truncated, err)
	}
}

// The entry cap counts every listing entry; past it the walk returns the
// partial sum with truncated set, and nothing more is listed.
func TestFolderSizeEntryCapTruncates(t *testing.T) {
	fs := newMemFS()
	for i := 0; i < 5; i++ {
		fs.writeFile(t, fmt.Sprintf("/e/f%d", i), "xx", 0o644) // 2 bytes each
	}
	lim := SizeLimits{MaxDepth: 12, MaxEntries: 3}
	size, truncated, err := FolderSize(context.Background(), fs, "/e", lim)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("a walk cut by the entry cap must be truncated")
	}
	if size != 6 {
		t.Fatalf("size = %d, want 6 (the first three entries, 2 bytes each)", size)
	}
}

// The depth cap stops the descent and flags the number: a folder that
// still has unreadable-below levels is worth its partial sum.
func TestFolderSizeDepthCapTruncates(t *testing.T) {
	fs := newMemFS()
	seedSizeTree(t, fs)

	// Two levels deep is enough for the tree's files; the descent into
	// sub/deep is refused and the walk says so.
	shallow := SizeLimits{MaxDepth: 2, MaxEntries: 2000}
	size, truncated, err := FolderSize(context.Background(), fs, "/d", shallow)
	if err != nil {
		t.Fatal(err)
	}
	if size != 11 || !truncated {
		t.Fatalf("depth-capped walk = %d, %v; want 11 (5+6) and truncated", size, truncated)
	}

	// The default depth covers the whole tree.
	size, truncated, err = FolderSize(context.Background(), fs, "/d", DefaultSizeLimits)
	if err != nil || size != 18 || truncated {
		t.Fatalf("full walk = %d, %v, %v; want 18, false, nil", size, truncated, err)
	}
}

// One unreadable directory truncates instead of failing: the readable
// siblings still sum, and the number carries the approximation mark.
func TestFolderSizeUnreadableDirTruncates(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/d/a.txt", "12345", 0o644)
	fs.writeFile(t, "/d/locked/b.bin", "abcdef", 0o644)
	fs.dirErr["/d/locked"] = errors.New("permission denied")

	size, truncated, err := FolderSize(context.Background(), fs, "/d", DefaultSizeLimits)
	if err != nil {
		t.Fatalf("an unreadable subfolder must not fail the walk: %v", err)
	}
	if size != 5 || !truncated {
		t.Fatalf("walk = %d, %v; want the readable 5 with truncated set", size, truncated)
	}
}

// An expired context is the request deadline: the walk returns whatever
// it has, flagged, and no error — the per-path entry says "timed out",
// not "empty".
func TestFolderSizeExpiredContextReturnsPartial(t *testing.T) {
	fs := newMemFS()
	seedSizeTree(t, fs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	size, truncated, err := FolderSize(ctx, fs, "/d", DefaultSizeLimits)
	if err != nil {
		t.Fatalf("an expired ctx must not surface as an error: %v", err)
	}
	if size != 0 || !truncated {
		t.Fatalf("walk = %d, %v; want the empty partial sum, truncated", size, truncated)
	}
}
