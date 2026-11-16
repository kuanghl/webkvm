package sshmachines

// Folder-size walks for the file browser.
//
// A directory listing cannot carry a directory's size: the FileInfo a
// listing returns for a directory is its block size (a constant 4096 on
// most servers), not the sum of what is under it. The endpoint in
// api/sshmachines_sftp.go therefore asks for the sum in a second,
// bounded request, and this walk is that bound.
//
// The walk is deliberately shallower and smaller than dirwalk.go's
// transfer walk: it moves nothing, it only reads listings, and it is
// cosmetic — the listing row's size cell shows "≈ 1.2 GB" with an
// approximation mark when the caps fire, and a user reading a folder
// size does not want a five-minute remote crawl behind a file listing.

import (
	"context"
	"os"
	"path"
	"sync"
)

// SizeFS is the read-only slice a folder-size walk needs: a stat and a
// listing, nothing that opens a file. *sftp.Client satisfies it as is,
// which keeps the endpoint working for the host card's in-process
// session and for tests alike.
type SizeFS interface {
	Stat(p string) (os.FileInfo, error)
	ReadDir(p string) ([]os.FileInfo, error)
}

// SizeLimits bounds one walk: MaxDepth levels below the root, MaxEntries
// listings consumed in total (every entry counted, not just files).
type SizeLimits struct {
	MaxDepth   int
	MaxEntries int
}

// DefaultSizeLimits is the endpoint's answer. Depth 12 and 2000 entries
// cover a home directory's ordinary shape (a few levels of subfolders,
// a few thousand files) and stop well inside the request's deadline on
// a slow link.
var DefaultSizeLimits = SizeLimits{MaxDepth: 12, MaxEntries: 2000}

// sizeWorkers is how many directories are listed in parallel per level.
// pkg/sftp multiplexes requests on one connection, so the gain is
// latency hiding on a high-RTT link; eight is enough to saturate it
// without turning a size request into a crawl.
const sizeWorkers = 8

// FolderSize sums the regular file sizes under root.
//
//   - Directories contribute nothing: their own FileInfo is a block
//     size, and counting it would put a constant 4096 per folder into
//     every number the browser shows.
//   - Symlinks are neither followed nor counted, the same rule as the
//     transfer walk: a link farm or a link cycle must not turn a size
//     read into a crawl of somewhere else.
//   - One unreadable directory truncates the walk instead of failing
//     it: a folder with 99 readable files and one permission-denied
//     subfolder is still worth 99 files' worth, and the UI renders the
//     partial sum with an approximation mark either way.
//
// truncated is true when either cap stopped the walk, a directory could
// not be listed, or ctx fired; the returned size is what the walk saw.
func FolderSize(ctx context.Context, fs SizeFS, root string, lim SizeLimits) (size int64, truncated bool, err error) {
	fi, err := fs.Stat(root)
	if err != nil {
		return 0, false, err
	}
	if !fi.IsDir() {
		// A file "folder" is just itself; the endpoint never asks
		// for one, but the walk stays total for a direct caller.
		return fi.Size(), false, nil
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = DefaultSizeLimits.MaxDepth
	}
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = DefaultSizeLimits.MaxEntries
	}

	type dirJob struct {
		dir   string
		depth int
	}
	level := []dirJob{{dir: root, depth: 0}}

	var (
		mu      sync.Mutex
		total   int64
		listed  int
		stopped bool // a cap fired, a listing failed, or ctx went
	)
	for len(level) > 0 {
		if ctx.Err() != nil {
			return total, true, nil // partial: the deadline is the bound
		}
		// List this whole level, at most sizeWorkers at once. The
		// semaphore is taken in the spawning loop so the fan-out is
		// bounded even when a level holds thousands of directories.
		results := make([][]os.FileInfo, len(level))
		var (
			wg  sync.WaitGroup
			sem = make(chan struct{}, sizeWorkers)
		)
		for i, j := range level {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, j dirJob) {
				defer wg.Done()
				defer func() { <-sem }()
				entries, rerr := fs.ReadDir(j.dir)
				if rerr != nil {
					mu.Lock()
					stopped = true
					mu.Unlock()
					return
				}
				results[i] = entries
			}(i, j)
		}
		wg.Wait()

		var next []dirJob
		for i, j := range level {
			// A cap that fired earlier in this level is already
			// reported: the remaining listings would only add lock
			// cycles over an unchanged total.
			if stopped {
				break
			}
			for _, e := range results[i] {
				mu.Lock()
				if listed >= lim.MaxEntries {
					stopped = true
					mu.Unlock()
					break
				}
				listed++
				switch {
				case e.IsDir():
					if !stopped && j.depth+1 < lim.MaxDepth {
						next = append(next, dirJob{dir: path.Join(j.dir, e.Name()), depth: j.depth + 1})
					} else {
						stopped = true
					}
				case e.Mode().IsRegular():
					total += e.Size()
				}
				// symlinks and special files: neither followed nor counted
				mu.Unlock()
			}
		}
		if stopped {
			return total, true, nil
		}
		level = next
	}
	return total, false, nil
}
