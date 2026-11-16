package sshmachines

import (
	"context"
	"errors"
	"os"
	"testing"
)

func walkTree(t *testing.T, fs *memFS, root string, lim WalkLimits) *WalkResult {
	t.Helper()
	res, err := expandTree(context.Background(), fs, root, lim)
	if err != nil {
		t.Fatalf("expandTree(%s): %v", root, err)
	}
	return res
}

func relPaths(res *WalkResult) []string {
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.RelPath)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func TestExpandTreeNested(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/e.txt", "eeee", 0o644)
	fs.writeFile(t, "/src/a/b.txt", "bb", 0o600)
	fs.writeFile(t, "/src/a/c/d.txt", "ddddddd", 0o640)
	if err := fs.MkdirAll("/src/empty"); err != nil {
		t.Fatal(err)
	}

	res := walkTree(t, fs, "/src", DefaultWalkLimits)

	want := []string{"a/b.txt", "a/c/d.txt", "e.txt"}
	got := relPaths(res)
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files = %v, want %v (deterministic, relpath order)", got, want)
		}
	}
	// Empty directories survive: a tree download must not silently drop
	// a logs/ or tmp/ the user expects to find on the other side.
	if !contains(res.Dirs, "a/c") || !contains(res.Dirs, "empty") {
		t.Fatalf("dirs = %v, want a/c and empty present", res.Dirs)
	}
	for i := 1; i < len(res.Dirs); i++ {
		if res.Dirs[i-1] > res.Dirs[i] {
			t.Fatalf("dirs not sorted parents-first: %v", res.Dirs)
		}
	}
	if res.TotalBytes != 4+2+7 {
		t.Fatalf("total = %d, want 13", res.TotalBytes)
	}
	if res.Truncated {
		t.Fatalf("truncated on a 4-entry tree")
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", res.Skipped)
	}
}

func TestExpandTreeCarriesModeAndMtime(t *testing.T) {
	fs := newMemFS()
	mod := fs.writeFile(t, "/src/run.sh", "#!/bin/sh\n", 0o750)

	res := walkTree(t, fs, "/src", DefaultWalkLimits)
	if len(res.Files) != 1 {
		t.Fatalf("files = %v", relPaths(res))
	}
	f := res.Files[0]
	if f.Mode.Perm() != 0o750 {
		t.Fatalf("mode = %v, want 0750", f.Mode.Perm())
	}
	if !f.ModTime.Equal(mod) {
		t.Fatalf("mtime = %v, want %v", f.ModTime, mod)
	}
	if f.Kind != specFile {
		t.Fatalf("kind = %d, want specFile", f.Kind)
	}
	if f.AbsPath != "/src/run.sh" {
		t.Fatalf("abspath = %q", f.AbsPath)
	}
}

func TestExpandTreeDepthLimit(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/l1/l2/l3/deep.txt", "x", 0o644)
	fs.writeFile(t, "/src/top.txt", "x", 0o644)

	res := walkTree(t, fs, "/src", WalkLimits{MaxDepth: 3, MaxFiles: 100})
	if !res.Truncated {
		t.Fatalf("want truncated at depth 3, got %+v", res)
	}
	if contains(relPaths(res), "l1/l2/l3/deep.txt") {
		t.Fatalf("depth limit ignored: %v", relPaths(res))
	}
	var noted bool
	for _, s := range res.Skipped {
		if s.Reason == "max depth reached" {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("depth cut is not reported in skipped: %+v", res.Skipped)
	}
}

func TestExpandTreeFileCountLimit(t *testing.T) {
	fs := newMemFS()
	for i := 0; i < 20; i++ {
		fs.writeFile(t, "/src/"+string(rune('a'+i))+".txt", "x", 0o644)
	}
	res := walkTree(t, fs, "/src", WalkLimits{MaxDepth: 8, MaxFiles: 5})
	if len(res.Files) != 5 {
		t.Fatalf("files = %d, want the cap of 5", len(res.Files))
	}
	if !res.Truncated {
		t.Fatalf("a capped walk must report truncation")
	}
}

func TestExpandTreeByteLimit(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/small.txt", "x", 0o644)
	// Named last so the ceiling is hit after one file fits: the walk
	// must stop at the entry that would overflow, not before it.
	fs.writeFile(t, "/src/zbig.bin", string(make([]byte, 4096)), 0o644)

	res := walkTree(t, fs, "/src", WalkLimits{MaxDepth: 8, MaxFiles: 100, MaxTotalBytes: 1024})
	if len(res.Files) != 1 || res.Files[0].RelPath != "small.txt" {
		t.Fatalf("files = %v, want only small.txt under a 1 KiB ceiling", relPaths(res))
	}
	if !res.Truncated {
		t.Fatalf("byte-cap truncation not reported")
	}
	if res.TotalBytes != 1 {
		t.Fatalf("total = %d, want the 1 byte actually planned", res.TotalBytes)
	}
}

func TestExpandTreeSkipsSpecialFiles(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/ok.txt", "x", 0o644)
	// A fifo with no writer blocks its reader forever: opening one would
	// park a transfer worker for the life of the process.
	fs.writeRaw("/src/live.fifo", &memNode{mode: os.ModeNamedPipe | 0o644})
	fs.writeRaw("/src/sock", &memNode{mode: os.ModeSocket | 0o666})
	fs.writeRaw("/dev/null", &memNode{mode: os.ModeDevice | 0o666})
	fs.writeRaw("/src/dev", &memNode{mode: os.ModeCharDevice | 0o666})

	res := walkTree(t, fs, "/src", DefaultWalkLimits)
	if len(res.Files) != 1 || res.Files[0].RelPath != "ok.txt" {
		t.Fatalf("files = %v, want only ok.txt", relPaths(res))
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want the three special entries", res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Fatalf("skip note without a reason: %+v", s)
		}
	}
}

func TestExpandTreeDoesNotFollowNestedSymlinks(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/real/huge.bin", string(make([]byte, 8192)), 0o644)
	fs.writeFile(t, "/src/keep.txt", "x", 0o644)
	if err := fs.Symlink("/real", "/src/outside"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("keep.txt", "/src/rel_link"); err != nil {
		t.Fatal(err)
	}

	res := walkTree(t, fs, "/src", DefaultWalkLimits)
	if len(res.Files) != 3 {
		t.Fatalf("files = %v, want keep.txt plus both links", relPaths(res))
	}
	// The link to a directory stays a link: following it would turn one
	// download into a whole filesystem.
	kinds := map[string]specKind{}
	for _, f := range res.Files {
		kinds[f.RelPath] = f.Kind
	}
	if kinds["outside"] != specSymlink || kinds["rel_link"] != specSymlink {
		t.Fatalf("kinds = %+v, want both entries as specSymlink", kinds)
	}
	if contains(res.Dirs, "outside") {
		t.Fatalf("the link to /real was walked as a directory: %v", res.Dirs)
	}
	if contains(relPaths(res), "outside/huge.bin") {
		t.Fatalf("symlink target contents leaked into the tree")
	}
}

func TestExpandTreeFollowsTheRootLink(t *testing.T) {
	// Pointing a download at /var/log expects the logs, not the link.
	fs := newMemFS()
	fs.writeFile(t, "/real/inner.txt", "abc", 0o644)
	if err := fs.Symlink("/real", "/link"); err != nil {
		t.Fatal(err)
	}
	res := walkTree(t, fs, "/link", DefaultWalkLimits)
	if !contains(relPaths(res), "inner.txt") {
		t.Fatalf("files = %v, want inner.txt through the root link", relPaths(res))
	}
}

func TestExpandTreeSurvivesAnUnreadableSubdir(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/good.txt", "x", 0o644)
	fs.writeFile(t, "/src/rooty/secret.txt", "x", 0o644)
	fs.dirErr["/src/rooty"] = &os.PathError{Op: "readdir", Path: "/src/rooty", Err: errors.New("permission denied")}

	res := walkTree(t, fs, "/src", DefaultWalkLimits)
	if !contains(relPaths(res), "good.txt") {
		t.Fatalf("one unreadable directory aborted the tree: %v", relPaths(res))
	}
	var noted bool
	for _, s := range res.Skipped {
		if s.Path == "rooty" {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("unreadable dir not reported: %+v", res.Skipped)
	}
}

func TestExpandTreeReportsUnstatableEntry(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/a.txt", "x", 0o644)
	fs.writeFile(t, "/src/gone.txt", "x", 0o644)
	// Something deleted between the listing and the lstat.
	fs.lstatErr["/src/gone.txt"] = &os.PathError{Op: "lstat", Path: "/src/gone.txt", Err: os.ErrNotExist}

	res := walkTree(t, fs, "/src", DefaultWalkLimits)
	if len(res.Files) != 1 || res.Files[0].RelPath != "a.txt" {
		t.Fatalf("files = %v, want only a.txt", relPaths(res))
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Path != "gone.txt" {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
}

func TestExpandTreeSingleFileRoot(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/src/one.txt", "hello", 0o644)
	res := walkTree(t, fs, "/src/one.txt", DefaultWalkLimits)
	if len(res.Files) != 1 || res.Files[0].RelPath != "one.txt" {
		t.Fatalf("files = %+v, want one entry relpath one.txt", res.Files)
	}
	if res.TotalBytes != 5 {
		t.Fatalf("total = %d", res.TotalBytes)
	}
	if len(res.Dirs) != 0 {
		t.Fatalf("dirs = %v", res.Dirs)
	}
}

func TestExpandTreeMissingRoot(t *testing.T) {
	fs := newMemFS()
	if _, err := expandTree(context.Background(), fs, "/nope", DefaultWalkLimits); err == nil {
		t.Fatalf("want an error for a missing root")
	}
}

func TestExpandTreeHonorsContext(t *testing.T) {
	fs := newMemFS()
	for i := 0; i < 8; i++ {
		fs.writeFile(t, "/src/d1/d2/d3/x.txt", "x", 0o644)
		fs.writeFile(t, "/src/x.txt", "x", 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := expandTree(ctx, fs, "/src", DefaultWalkLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSortFilesFirstPutsDirsFirst(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/x/zeta.txt", "x", 0o644)
	fs.writeFile(t, "/x/alpha.txt", "x", 0o644)
	fs.writeFile(t, "/x/sub/deep.txt", "x", 0o644)
	entries, err := fs.ReadDir("/x")
	if err != nil {
		t.Fatal(err)
	}
	sortFilesFirst(entries)
	if entries[0].Name() != "sub" {
		t.Fatalf("first entry = %q, want the directory", entries[0].Name())
	}
	if entries[1].Name() != "alpha.txt" || entries[2].Name() != "zeta.txt" {
		t.Fatalf("order = %v, want dirs then name asc", []string{entries[1].Name(), entries[2].Name()})
	}
}

func TestDefaultWalkLimitsAreBounded(t *testing.T) {
	// Guards the ceiling the plan settled on: a runaway tree is the one
	// failure mode a "download my configs" click should never produce.
	if DefaultWalkLimits.MaxDepth != 32 || DefaultWalkLimits.MaxFiles != 20000 {
		t.Fatalf("DefaultWalkLimits = %+v", DefaultWalkLimits)
	}
	if DefaultWalkLimits.MaxTotalBytes != 0 {
		t.Fatalf("byte ceiling must stay off by default, got %d", DefaultWalkLimits.MaxTotalBytes)
	}
}
