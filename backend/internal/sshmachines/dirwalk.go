package sshmachines

// Directory expansion for recursive transfers.
//
// This deliberately does NOT use sftp.Client.Walk: it returns a
// kr/fs.Walker built on a goroutine with no quit channel, so breaking
// out of the loop to honour a file-count or depth limit leaks that
// goroutine for the life of the process, and there is no way to feed it
// a context or a fake filesystem for tests. The stack-based walk below
// takes an explicit context, bounds depth/count/bytes, and is pure
// enough to unit test against memfs.

import (
	"context"
	"errors"
	"os"
	"path"
	"sort"
	"time"
)

// specKind is what one walked entry turned out to be.
type specKind int

const (
	specFile specKind = iota
	specDir
	specSymlink
)

// FileSpec is one entry of a walked tree. RelPath is relative to the
// walk root with '/' separators, which is what both the remote mkdir
// side and the browser's tar side need.
type FileSpec struct {
	RelPath string
	AbsPath string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	Kind    specKind
}

// SkipNote records an entry the walk refused to include. Skipping is
// never fatal: a tree with a live fifo in it still transfers.
type SkipNote struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// WalkLimits bounds a recursive walk. MaxTotalBytes <= 0 means no byte
// limit; the default is deliberately unlimited because "download this
// log directory" has no meaningful ceiling, while file count and depth
// do.
type WalkLimits struct {
	MaxDepth      int
	MaxFiles      int
	MaxTotalBytes int64
}

// DefaultWalkLimits is the production ceiling: 20k entries is far past
// any legitimate home-directory download and well inside what a
// sequential SFTP stream can finish before a user gives up.
var DefaultWalkLimits = WalkLimits{MaxDepth: 32, MaxFiles: 20000}

type WalkResult struct {
	Files      []FileSpec // files and symlinks, deterministic order
	Dirs       []string   // directory relpaths, parents first
	Skipped    []SkipNote
	TotalBytes int64
	Truncated  bool // a limit cut the tree short
}

// expandTree walks root on fs. Symlinks are never followed: they are
// copied as links, so a tree containing /usr -> a giant mount cannot
// turn one download into a whole filesystem. The root itself IS
// followed, because a user pointing at /var/log expects the logs, not
// the link.
func expandTree(ctx context.Context, fs sftpFS, root string, lim WalkLimits) (*WalkResult, error) {
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = DefaultWalkLimits.MaxDepth
	}
	if lim.MaxFiles <= 0 {
		lim.MaxFiles = DefaultWalkLimits.MaxFiles
	}
	rootFI, err := fs.Stat(root)
	if err != nil {
		return nil, err
	}
	res := &WalkResult{}
	if !rootFI.IsDir() {
		// A single file keeps the caller code path uniform. Its RelPath
		// is the bare name, which is also what planFor produces for the
		// same shape, so the destination ends up under dstDir/ either way.
		res.Files = append(res.Files, specFor(path.Base(root), root, rootFI))
		res.TotalBytes = rootFI.Size()
		return res, nil
	}

	type frame struct {
		abs   string
		rel   string
		depth int
	}
	frameRel := func(f frame) string {
		if f.rel != "" {
			return f.rel
		}
		return path.Base(f.abs) // the root: report it by name, not as ""
	}
	stack := []frame{{abs: root, rel: ""}}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := fs.ReadDir(cur.abs)
		if err != nil {
			// One unreadable subdirectory must not abort the tree.
			res.Skipped = append(res.Skipped, SkipNote{Path: frameRel(cur), Reason: err.Error()})
			continue
		}
		sortFilesFirst(entries)

		for _, fi := range entries {
			name := fi.Name()
			if name == "." || name == ".." {
				continue
			}
			rel := joinRel(cur.rel, name)
			abs := path.Join(cur.abs, name)

			// Lstat, not Stat: a symlink to a directory must stay a link.
			lfi, lerr := fs.Lstat(abs)
			if lerr != nil {
				res.Skipped = append(res.Skipped, SkipNote{Path: rel, Reason: lerr.Error()})
				continue
			}
			mode := lfi.Mode()
			switch {
			case mode&os.ModeSymlink != 0:
				if err := addFile(res, specFor(rel, abs, lfi), lim); err != nil {
					res.Truncated = true
					return res, nil
				}
			case lfi.IsDir():
				if cur.depth+1 >= lim.MaxDepth {
					res.Skipped = append(res.Skipped, SkipNote{Path: rel, Reason: "max depth reached"})
					res.Truncated = true
					continue
				}
				if len(res.Dirs)+len(res.Files) >= lim.MaxFiles {
					res.Truncated = true
					return res, nil
				}
				res.Dirs = append(res.Dirs, rel)
				stack = append(stack, frame{abs: abs, rel: rel, depth: cur.depth + 1})
			case !mode.IsRegular():
				// fifo / socket / device. Opening one would block the
				// worker forever (a fifo with no writer never returns),
				// so they are skipped, hard.
				res.Skipped = append(res.Skipped, SkipNote{Path: rel, Reason: "not a regular file (" + mode.Type().String() + ")"})
			default:
				if err := addFile(res, specFor(rel, abs, lfi), lim); err != nil {
					res.Truncated = true
					return res, nil
				}
			}
		}
	}
	// Dirs came off the stack in DFS order; the transfer side creates
	// them parents-first, so put them in lexical order for determinism.
	sort.Strings(res.Dirs)
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].RelPath < res.Files[j].RelPath })
	return res, nil
}

func specFor(rel, abs string, fi os.FileInfo) FileSpec {
	k := specFile
	if fi.Mode()&os.ModeSymlink != 0 {
		k = specSymlink
	}
	return FileSpec{RelPath: rel, AbsPath: abs, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), Kind: k}
}

// addFile appends one entry, returning an error when a limit is hit so
// the caller can mark the walk truncated.
func addFile(res *WalkResult, s FileSpec, lim WalkLimits) error {
	if len(res.Files) >= lim.MaxFiles {
		return errors.New("max file count reached")
	}
	if lim.MaxTotalBytes > 0 && res.TotalBytes+s.Size > lim.MaxTotalBytes {
		return errors.New("max total size reached")
	}
	res.Files = append(res.Files, s)
	res.TotalBytes += s.Size
	return nil
}

// sortFilesFirst orders a directory listing name-ascending with
// directories before files, which is both what people expect to read
// and what makes the transfer reach directory entries early.
func sortFilesFirst(entries []os.FileInfo) {
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})
}

func joinRel(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}
