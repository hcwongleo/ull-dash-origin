package server

import (
	"context"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// Files Array of on the fly files
	Files = map[string]*File{}

	// FilesLock Lock used to write / read files
	FilesLock = new(sync.RWMutex)
)

// FileReader Defines a reader
type FileReadCloser struct {
	offset int
	w      http.ResponseWriter
	ctx    context.Context
	// stopWake deregisters the context callback that wakes this reader. Calling
	// it is optional: the callback is released anyway once ctx is done, and a
	// request context always completes.
	stopWake func() bool
	// closed makes Close idempotent, so the readersActive gauge cannot go negative.
	closed atomic.Bool
	*File
}

// Read Reads bytes from filereader
//
// When the reader has drained everything written so far but the writer has not
// finished, it waits on the file's condition variable. Returning (0, nil) here
// instead would make io.Copy retry immediately, spinning a whole CPU core for
// every reader parked on an in-progress file.
//
// A parked reader is invisible to net/http, so the request context is checked on
// every wakeup. NewReadCloser arranges for cancellation to broadcast, which is
// what makes a disconnected viewer's goroutine terminate instead of waiting for
// a writer that may never come back.
func (r *FileReadCloser) Read(p []byte) (int, error) {
	r.File.lock.RLock()
	defer r.File.lock.RUnlock()
	for r.offset >= len(r.File.buffer) && !r.File.eof {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		r.File.cond.Wait()
	}
	if r.offset >= len(r.File.buffer) {
		return 0, io.EOF
	}
	n := copy(p, r.File.buffer[r.offset:])
	r.offset += n
	// r.w.(http.Flusher).Flush()
	return n, nil
}

// Close Releases the reader.
//
// This deliberately shadows the promoted File.Close. FileReadCloser embeds
// *File, so without this method closing a reader would set eof on the file and
// silently truncate a segment still being ingested for every other reader.
func (r *FileReadCloser) Close() error {
	// Guard against a double Close: net/http may close a body more than once, and
	// decrementing twice would drive the gauge negative.
	if r.closed.Swap(true) {
		return nil
	}

	readersActive.Add(-1)

	if r.stopWake != nil {
		r.stopWake()
	}

	return nil
}

// File Definition of file
type File struct {
	Name    string
	headers http.Header
	lock    *sync.RWMutex
	// cond signals readers parked on an in-progress file. Its Locker is
	// lock.RLocker(), which is safe because every waiter is a reader holding
	// RLock. Wait enqueues on the notify list before unlocking, and a writer
	// cannot append while a reader holds RLock, so a Broadcast issued after the
	// write lock is released can never be missed.
	cond       *sync.Cond
	buffer     []byte
	eof        bool
	onDisk     bool
	receivedAt time.Time
	maxAgeS    int64
}

// NewFile Creates a new file
func NewFile(name string, headers http.Header, maxAgeS int64) *File {
	f := File{
		Name:       name,
		headers:    headers,
		lock:       new(sync.RWMutex),
		buffer:     []byte{},
		eof:        false,
		onDisk:     false,
		receivedAt: time.Now(),
		maxAgeS:    maxAgeS,
	}
	f.cond = sync.NewCond(f.lock.RLocker())

	contentType := f.GetContentType()

	logDebugf("new file, Content-Type %q", contentType)

	return &f
}

func (f *File) GetContentType() string {
	return f.headers.Get("Content-Type")
}

// NewReadCloser Crates a new filereader from a file
//
// ctx is the request context. When it is cancelled the file's condition variable
// is broadcast, so a reader parked in Read wakes up and observes the
// cancellation rather than waiting on a writer indefinitely.
func (f *File) NewReadCloser(ctx context.Context, baseDir string, w http.ResponseWriter) io.ReadCloser {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if ctx == nil {
		ctx = context.Background()
	}

	if f.onDisk {
		name := path.Join(baseDir, f.Name)
		file, err := os.Open(name)
		if err != nil {
			logErrorf("opening %s from disk: %v", name, err)
			return nil
		}
		logDebugf("serving %s from disk", f.Name)
		return file
	}

	logDebugf("serving %s from memory", f.Name)
	readersActive.Add(1)
	r := &FileReadCloser{
		offset: 0,
		w:      w,
		ctx:    ctx,
		File:   f,
	}
	r.stopWake = context.AfterFunc(ctx, f.cond.Broadcast)

	return r
}

// Close Closes a file
func (f *File) Close() error {
	f.lock.Lock()
	f.eof = true
	f.lock.Unlock()
	f.cond.Broadcast()

	return nil
}

// Write Write bytes to a file
func (f *File) Write(p []byte) (int, error) {
	f.lock.Lock()
	f.buffer = append(f.buffer, p...)
	f.lock.Unlock()

	noteIngest(len(p))

	f.cond.Broadcast()
	return len(p), nil
}

// WriteToDisk Writes a file to disc
func (f *File) WriteToDisk(baseDir string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	name := path.Join(baseDir, f.Name)

	if _, err := os.Stat(filepath.Dir(name)); os.IsNotExist(err) {
		err := os.MkdirAll(filepath.Dir(name), 0755)
		if err != nil {
			return err
		}
	}

	err := ioutil.WriteFile(name, f.buffer, 0644)
	if err != nil {
		return err
	}
	f.onDisk = true
	f.buffer = nil
	return nil
}

// RemoveFromDisk Removes file from disc
func (f *File) RemoveFromDisk(baseDir string) error {
	f.lock.Lock()

	name := path.Join(baseDir, f.Name)
	err := os.Remove(name)

	// even if we get an error, lets act as if the file is completely removed
	f.onDisk = false
	f.buffer = nil
	// Discarding the buffer is terminal for anyone still reading it, so treat it
	// as end of file and wake any reader parked in Read. Without this a reader
	// waiting on a file removed before its writer closed would wait forever.
	f.eof = true

	f.lock.Unlock()
	f.cond.Broadcast()

	return err
}
