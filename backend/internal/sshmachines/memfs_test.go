package sshmachines

// In-memory sftpFS used by the transfer tests. Standing this up is the
// whole reason sftpFS exists: before it, runUpload/runDownload could only
// be exercised against a live sshd, which is why they had zero coverage.

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type memNode struct {
	data []byte
	mode os.FileMode
	mod  time.Time
	link string
}

type memFS struct {
	mu       sync.Mutex
	nodes    map[string]*memNode
	noPosix  bool                     // pretend the server lacks posix-rename
	readErr  map[string]error         // forced Open failures
	dirErr   map[string]error         // forced ReadDir failures
	lstatErr map[string]error         // forced Lstat failures (an entry the walk must skip)
	cut      map[string]int64         // fail an Open read past this many bytes
	gates    map[string]chan struct{} // Open blocks until the test closes it
	readHold map[string]chan struct{} // the first Read of a file blocks until closed
}

func newMemFS() *memFS {
	return &memFS{
		nodes: map[string]*memNode{
			"/": {mode: os.ModeDir | 0o755},
		},
		readErr:  map[string]error{},
		dirErr:   map[string]error{},
		lstatErr: map[string]error{},
		cut:      map[string]int64{},
		gates:    map[string]chan struct{}{},
		readHold: map[string]chan struct{}{},
	}
}

// blockOpen makes Open(p) hang until the returned channel is closed, so
// a cancellation mid-file can be tested without timing sleeps.
func (f *memFS) blockOpen(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.gates[p] = ch
	return ch
}

func (f *memFS) gateFor(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gates[p]
}

// holdFirstRead makes the first Read of p block until the returned
// channel is closed: a deterministic "the link went silent mid-file"
// for pause/cancel tests, with no timing involved. Later reads on the
// same file are unaffected (the hold is one-shot).
func (f *memFS) holdFirstRead(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.readHold[p] = ch
	return ch
}

func (f *memFS) takeHold(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.readHold[p]
	if ok {
		delete(f.readHold, p)
	}
	return ch
}

// dropGates releases every gate blockOpen installed. It is the fake's
// version of "the session was force-closed": a read that was wedged now
// returns instead of hanging for the life of the process.
func (f *memFS) dropGates() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, ch := range f.gates {
		close(ch)
		delete(f.gates, p)
	}
}

func (f *memFS) info(p string) (os.FileInfo, error) {
	n, ok := f.nodes[p]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrNotExist}
	}
	name := path.Base(p)
	size := int64(len(n.data))
	if n.mode&os.ModeSymlink != 0 {
		size = int64(len(n.link))
	}
	return memInfo{name: name, size: size, mode: n.mode, mod: n.mod}, nil
}

type memInfo struct {
	name string
	size int64
	mode os.FileMode
	mod  time.Time
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() os.FileMode  { return i.mode }
func (i memInfo) ModTime() time.Time { return i.mod }
func (i memInfo) IsDir() bool        { return i.mode.IsDir() }
func (i memInfo) Sys() any           { return nil }

// resolve follows every symlink component of p, the way a real SFTP
// server resolves a path before opening it. Lstat is the only call that
// must not use it.
func (f *memFS) resolve(p string) (string, error) {
	if p == "/" || p == "" {
		return "/", nil
	}
	var out string
	for i, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if i == 0 {
			out = "/" + seg
		} else {
			out = out + "/" + seg
		}
		for hops := 0; hops < 8; hops++ {
			n, ok := f.nodes[out]
			if !ok || n.mode&os.ModeSymlink == 0 {
				break
			}
			out = path.Clean(path.Join(path.Dir(out), n.link))
		}
	}
	if _, ok := f.nodes[out]; !ok {
		return "", &os.PathError{Op: "stat", Path: p, Err: os.ErrNotExist}
	}
	return out, nil
}

func (f *memFS) Stat(p string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	return f.info(rp)
}

// resolveParent follows every symlink except the final component, which
// is how lstat() and open() behave on a real server: a path under a
// linked directory is reachable, the link itself stays a link.
func (f *memFS) resolveParent(p string) string {
	dir, base := path.Split(p)
	if base == "" {
		return p
	}
	rp, err := f.resolve(dir)
	if err != nil {
		return p
	}
	return path.Join(rp, base)
}

func (f *memFS) Lstat(p string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.lstatErr[p]; ok {
		return nil, err
	}
	return f.info(f.resolveParent(p))
}

func (f *memFS) ReadDir(p string) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.dirErr[p]; ok {
		return nil, err
	}
	rp, err := f.resolve(p)
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: p, Err: os.ErrNotExist}
	}
	n := f.nodes[rp]
	if !n.mode.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: p, Err: fmt.Errorf("not a directory")}
	}
	prefix := rp
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var names []string
	for k := range f.nodes {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" || strings.Contains(rest, "/") { // grandchild, not an entry
			continue
		}
		names = append(names, rest)
	}
	sort.Strings(names)
	out := make([]os.FileInfo, 0, len(names))
	for _, name := range names {
		fi, err := f.info(prefix + name)
		if err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

func (f *memFS) Mkdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[p]; ok {
		return &os.PathError{Op: "mkdir", Path: p, Err: os.ErrExist}
	}
	parent := path.Dir(p)
	if pn, ok := f.nodes[parent]; !ok || !pn.mode.IsDir() {
		return &os.PathError{Op: "mkdir", Path: p, Err: os.ErrNotExist}
	}
	f.nodes[p] = &memNode{mode: os.ModeDir | 0o755, mod: time.Now()}
	return nil
}

func (f *memFS) MkdirAll(p string) error {
	p = path.Clean(p)
	if p == "." || p == "/" {
		return nil
	}
	f.mu.Lock()
	n, ok := f.nodes[p]
	f.mu.Unlock()
	if ok {
		if n.mode.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdirall", Path: p, Err: fmt.Errorf("not a directory")}
	}
	if err := f.MkdirAll(path.Dir(p)); err != nil {
		return err
	}
	return f.Mkdir(p)
}

func (f *memFS) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return &os.PathError{Op: "remove", Path: p, Err: os.ErrNotExist}
	}
	if n.mode.IsDir() {
		return &os.PathError{Op: "remove", Path: p, Err: fmt.Errorf("is a directory")}
	}
	delete(f.nodes, p)
	return nil
}

func (f *memFS) RemoveDirectory(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return &os.PathError{Op: "rmdir", Path: p, Err: os.ErrNotExist}
	}
	if !n.mode.IsDir() {
		return &os.PathError{Op: "rmdir", Path: p, Err: fmt.Errorf("not a directory")}
	}
	for k := range f.nodes {
		if strings.HasPrefix(k, strings.TrimSuffix(p, "/")+"/") {
			return &os.PathError{Op: "rmdir", Path: p, Err: fmt.Errorf("directory not empty")}
		}
	}
	delete(f.nodes, p)
	return nil
}

func (f *memFS) RemoveAll(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := strings.TrimSuffix(p, "/") + "/"
	for k := range f.nodes {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(f.nodes, k)
		}
	}
	return nil
}

func (f *memFS) Rename(oldp, newp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[oldp]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldp, Err: os.ErrNotExist}
	}
	if _, exists := f.nodes[newp]; exists {
		// Plain SFTP has no replace-rename; the destination must be free.
		return &os.PathError{Op: "rename", Path: newp, Err: os.ErrExist}
	}
	delete(f.nodes, oldp)
	f.nodes[newp] = n
	return nil
}

func (f *memFS) PosixRename(oldp, newp string) error {
	if f.noPosix {
		return &os.PathError{Op: "posix-rename", Path: newp, Err: fmt.Errorf("unsupported")}
	}
	return f.renameOver(oldp, newp)
}

func (f *memFS) renameOver(oldp, newp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[oldp]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldp, Err: os.ErrNotExist}
	}
	delete(f.nodes, oldp)
	f.nodes[newp] = n
	return nil
}

func (f *memFS) Chmod(p string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return &os.PathError{Op: "chmod", Path: p, Err: os.ErrNotExist}
	}
	n.mode = (n.mode & ^os.ModePerm) | (mode & os.ModePerm)
	return nil
}

func (f *memFS) Chtimes(p string, _, mtime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return &os.PathError{Op: "chtimes", Path: p, Err: os.ErrNotExist}
	}
	n.mod = mtime
	return nil
}

func (f *memFS) Symlink(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.nodes[newname]; exists {
		return &os.PathError{Op: "symlink", Path: newname, Err: os.ErrExist}
	}
	f.nodes[newname] = &memNode{mode: os.ModeSymlink | 0o777, link: oldname, mod: time.Now()}
	return nil
}

func (f *memFS) ReadLink(p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return "", &os.PathError{Op: "readlink", Path: p, Err: os.ErrNotExist}
	}
	if n.mode&os.ModeSymlink == 0 {
		return "", &os.PathError{Op: "readlink", Path: p, Err: fmt.Errorf("not a symlink")}
	}
	return n.link, nil
}

func (f *memFS) HasExtension(ext string) (string, bool) {
	if ext == "posix-rename@openssh.com" && f.noPosix {
		return "", false
	}
	return ext, true
}

// Openers ------------------------------------------------------------------

func (f *memFS) Open(p string) (remoteFile, error) {
	if gate := f.gateFor(p); gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.readErr[p]; ok {
		return nil, err
	}
	n, ok := f.nodes[f.resolveParent(p)]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
	if n.mode.IsDir() {
		return nil, &os.PathError{Op: "open", Path: p, Err: fmt.Errorf("is a directory")}
	}
	// Snapshot the bytes: matches a real handle well enough and keeps
	// the lock from being held across reads.
	return &memFile{
		fs:   f,
		name: p,
		r:    bytes.NewReader(append([]byte(nil), n.data...)),
		cut:  f.cut[p],
	}, nil
}

func (f *memFS) OpenFile(p string, flags int) (remoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = f.resolveParent(p)
	n, ok := f.nodes[p]
	if !ok {
		if flags&os.O_CREATE == 0 {
			return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
		}
		parent := path.Dir(p)
		if pn, ok := f.nodes[parent]; !ok || !pn.mode.IsDir() {
			return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
		}
		n = &memNode{mode: 0o644, mod: time.Now()}
		f.nodes[p] = n
	}
	if flags&os.O_TRUNC != 0 {
		n.data = nil
	}
	return &memFile{fs: f, name: p, write: true}, nil
}

func (f *memFS) Create(p string) (remoteFile, error) {
	return f.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
}

type memFile struct {
	fs     *memFS
	name   string
	r      *bytes.Reader
	write  bool
	closed bool
	served int64
	cut    int64 // >0: fail the read once this many bytes were served
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.r == nil {
		return 0, fmt.Errorf("read %s: handle is write-only", m.name)
	}
	if ch := m.fs.takeHold(m.name); ch != nil {
		<-ch
	}
	n, err := m.r.Read(p)
	m.served += int64(n)
	if m.cut > 0 && m.served >= m.cut && err == nil {
		return n, fmt.Errorf("read %s: connection reset by server", m.name)
	}
	return n, err
}

func (m *memFile) Write(p []byte) (int, error) {
	m.fs.mu.Lock()
	defer m.fs.mu.Unlock()
	n, ok := m.fs.nodes[m.name]
	if !ok {
		return 0, &os.PathError{Op: "write", Path: m.name, Err: os.ErrNotExist}
	}
	n.data = append(n.data, p...)
	n.mod = time.Now()
	return len(p), nil
}

func (m *memFile) Close() error { m.closed = true; return nil }

func (m *memFile) Stat() (os.FileInfo, error) { return m.fs.Stat(m.name) }

// Test-side helpers --------------------------------------------------------

func (f *memFS) writeFile(t *testing.T, p, data string, mode os.FileMode) time.Time {
	t.Helper()
	if err := f.MkdirAll(path.Dir(p)); err != nil {
		t.Fatalf("mkdirall %s: %v", path.Dir(p), err)
	}
	mod := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[p] = &memNode{data: []byte(data), mode: mode, mod: mod}
	return mod
}

// writeRaw inserts any node, including kinds the helpers cannot express
// (fifo, socket, dangling symlink).
func (f *memFS) writeRaw(p string, n *memNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[p] = n
}

func (f *memFS) get(p string) *memNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[p]
	if n == nil {
		return nil
	}
	cp := *n
	return &cp
}

func (f *memFS) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.nodes))
	for k := range f.nodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parts lists the scratch files left behind, which is what the
// "cancel leaves no debris" assertions are about.
func (f *memFS) parts() []string {
	var out []string
	for _, p := range f.paths() {
		if strings.Contains(p, partSuffix("")) {
			out = append(out, p)
		}
	}
	return out
}

var _ sftpFS = (*memFS)(nil)

// fakeSource is the sessionSource stand-in: one memFS per machine id,
// plus the lease accounting the registry does in production.
type fakeSource struct {
	mu      sync.Mutex
	fs      map[string]*memFS
	leases  map[string]int
	drops   map[string]int
	acqErr  map[string]error
	acqHold map[string]chan struct{} // the first Acquire blocks until closed
}

func newFakeSource(ids ...string) *fakeSource {
	s := &fakeSource{
		fs:      map[string]*memFS{},
		leases:  map[string]int{},
		drops:   map[string]int{},
		acqErr:  map[string]error{},
		acqHold: map[string]chan struct{}{},
	}
	for _, id := range ids {
		s.fs[id] = newMemFS()
	}
	return s
}

func (s *fakeSource) fsFor(id string) *memFS {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.fs[id]
	if !ok {
		f = newMemFS()
		s.fs[id] = f
	}
	return f
}

func (s *fakeSource) Acquire(machineID string) (sftpFS, func(), error) {
	s.mu.Lock()
	if err, ok := s.acqErr[machineID]; ok {
		s.mu.Unlock()
		return nil, nil, err
	}
	f, ok := s.fs[machineID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("machine %q not registered", machineID)
	}
	if ch, ok := s.acqHold[machineID]; ok {
		// One-shot, like holdFirstRead: the block must not leak into a
		// second Acquire after the first lease is released.
		delete(s.acqHold, machineID)
		s.mu.Unlock()
		<-ch
		s.mu.Lock()
	}
	s.leases[machineID]++
	s.mu.Unlock()

	var once sync.Once
	return f, func() {
		once.Do(func() {
			s.mu.Lock()
			s.leases[machineID]--
			s.mu.Unlock()
		})
	}, nil
}

// holdAcquire makes the FIRST Acquire for id block until the returned
// channel is closed: a deterministic "the worker has not taken its
// lease yet" for the pre-lease pause window, with no timing involved.
// One-shot, like holdFirstRead.
func (s *fakeSource) holdAcquire(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.acqHold[id] = ch
	return ch
}

func (s *fakeSource) live(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leases[id]
}

// Drop is the stall escape hatch. Here that means releasing the machine's
// blocked reads — what a force-closed session does to a wedged socket —
// and counting the call, so a test can assert the watchdog fired rather
// than sleeping through the grace period.
func (s *fakeSource) Drop(machineID string) {
	s.mu.Lock()
	s.drops[machineID]++
	f := s.fs[machineID]
	s.mu.Unlock()
	if f != nil {
		f.dropGates()
	}
}

func (s *fakeSource) dropped(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drops[id]
}

var _ sessionSource = (*fakeSource)(nil)
