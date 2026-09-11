package server

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ChunkedResponseWriter Define a response writer
type ChunkedResponseWriter struct {
	w http.ResponseWriter
}

// Write Writes few bytes
func (rw ChunkedResponseWriter) Write(p []byte) (nn int, err error) {
	nn, err = rw.w.Write(p)
	rw.w.(http.Flusher).Flush()
	return
}

// GetHandler Sends file bytes
func GetHandler(waitingRequests *WaitingRequests, cors *Cors, basePath string, w http.ResponseWriter, r *http.Request) {
	name := r.URL.String()

	FilesLock.RLock()
	f, ok := Files[name]
	FilesLock.RUnlock()

	if !ok {
		isFound := false
		waited := 0 * time.Millisecond
		if waitingRequests != nil {
			// Wait and return
			isFound, waited = waitingRequests.AddWaitingRequest(name, getHeadersFiltered(r.Header))
			w.Header().Set("Waited-For-Data-Ms", strconv.FormatInt(int64(waited/time.Millisecond), 10))
			if isFound {
				// Refresh file
				FilesLock.RLock()
				fnew, ok := Files[name]
				FilesLock.RUnlock()
				if !ok {
					// This should be very rare, file arrived but it is not in Files. It can happen if it expired just between arrived and this line
					isFound = false
				} else {
					f = fnew
				}
			}
		}
		if !isFound {
			addCors(w, cors)
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	addCors(w, cors)
	addHeaders(w, f.headers)

	// Add chunked only if the file is not yet complete
	if !f.eof {
		w.Header().Set("Transfer-Encoding", "chunked")
	}

	rc := f.NewReadCloser(r.Context(), basePath, w)
	if rc == nil {
		// Only reachable on the disk path, when the backing file has gone missing.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	w.WriteHeader(http.StatusOK)
	io.Copy(ChunkedResponseWriter{w}, rc)
}

// HeadHandler Sends if file exists
func HeadHandler(cors *Cors, w http.ResponseWriter, r *http.Request) {
	FilesLock.RLock()
	f, ok := Files[r.URL.String()]
	FilesLock.RUnlock()

	addCors(w, cors)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	addHeaders(w, f.headers)
	w.Header().Set("Transfer-Encoding", "chunked")

	w.WriteHeader(http.StatusOK)
}

// PostHandler Writes a file
func PostHandler(waitingRequests *WaitingRequests, onlyRAM bool, cors *Cors, basePath string, w http.ResponseWriter, r *http.Request) {
	// TODO: Add trigger blocking requests reusing/coping the code in Get
	name := r.URL.String()

	maxAgeS := getMaxAgeOr(r.Header.Get("Cache-Control"), -1)
	headers := getHeadersFiltered(r.Header)

	f := NewFile(name, headers, maxAgeS)

	FilesLock.Lock()
	Files[name] = f
	FilesLock.Unlock()

	// Start writing to file without holding lock so that GET requests can read from it.
	// The body carries an idle deadline so a half-open connection cannot pin this
	// goroutine and its socket forever.
	body := withIdleDeadline(w, r)

	var written int64
	var copyErr error
	if isManifest(name) {
		// A manifest is rewritten to add UTCTiming, which needs the whole body, so
		// it is buffered. Safe only because a manifest is a few kilobytes and is
		// complete the instant it arrives - buffering a media segment here would
		// destroy read-while-write, which is the entire point of this server.
		written, copyErr = copyManifest(f, body)
	} else {
		written, copyErr = io.Copy(f, body)
	}
	body.Close()
	// Close before anything else, so readers parked on this file are released
	// whether the ingest succeeded or not.
	f.Close()

	// An aborted ingest leaves a truncated segment. Serving it as if complete
	// hands players corrupt media, so drop it: the encoder will either retry the
	// PUT or move on, and a 404 is recoverable where bad bytes are not.
	if copyErr != nil {
		FilesLock.Lock()
		if cur, ok := Files[name]; ok && cur == f {
			delete(Files, name)
		}
		FilesLock.Unlock()

		PutAbortedTotal.Add(1)
		logWarnf("ingest of %s aborted after %d bytes, discarded: %v", name, written, copyErr)

		addCors(w, cors)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !onlyRAM {
		if err := f.WriteToDisk(basePath); err != nil {
			// Never log.Fatalf here: one failed write would take the whole origin
			// down, and with it every in-flight stream.
			logErrorf("saving %s to disk: %v", name, err)
			addCors(w, cors)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	addCors(w, cors)
	w.WriteHeader(http.StatusNoContent)

	// Awake GET requests waiting (if there are any)
	if waitingRequests != nil {
		waitingRequests.ReceivedDataFor(name)
	}
}

// PutHandler Writes a file
func PutHandler(waitingRequests *WaitingRequests, onlyRAM bool, cors *Cors, basePath string, w http.ResponseWriter, r *http.Request) {
	PostHandler(waitingRequests, onlyRAM, cors, basePath, w, r)
}

// DeleteHandler Deletes a file
func DeleteHandler(onlyRAM bool, cors *Cors, basePath string, w http.ResponseWriter, r *http.Request) {
	FilesLock.RLock()
	f, ok := Files[r.URL.String()]
	FilesLock.RUnlock()

	addCors(w, cors)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	FilesLock.Lock()
	delete(Files, r.URL.String())
	FilesLock.Unlock()

	if !onlyRAM {
		f.RemoveFromDisk(basePath)
	}

	w.WriteHeader(http.StatusNoContent)
}

// OptionsHandler Returns CORS options
func OptionsHandler(cors *Cors, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Transfer-Encoding", "chunked")

	addCors(w, cors)
	w.WriteHeader(http.StatusNoContent)
}

func addCors(w http.ResponseWriter, cors *Cors) {
	// Add Content-Type & Cache-Control automatically
	// Some features depends on those
	allowedHeaders := cors.GetAllowedHeaders()
	allowedHeaders = append(allowedHeaders, "Content-Type")
	allowedHeaders = append(allowedHeaders, "Cache-Control")

	w.Header().Set("Access-Control-Allow-Origin", strings.Join(cors.GetAllowedOrigins(), ", "))
	w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(cors.GetAllowedMethods(), ", "))
	w.Header().Set("Access-Control-Expose-Headers", strings.Join(allowedHeaders, ", "))
}

func addHeaders(w http.ResponseWriter, headersSrc http.Header) {
	// Copy all headers
	for name, values := range headersSrc {
		// Loop over all values for the name.
		for _, value := range values {
			w.Header().Set(name, value)
		}
	}
}

// copyManifest buffers a manifest, injects UTCTiming, and writes the result.
//
// On any doubt it falls back to storing exactly what arrived: an oversized body
// is streamed through unmodified rather than held, and a read error is returned
// with whatever was written so the caller's existing abort handling applies.
func copyManifest(f *File, body io.Reader) (int64, error) {
	buf, err := io.ReadAll(io.LimitReader(body, manifestSizeCap))
	if err != nil {
		n, _ := f.Write(buf)

		return int64(n), err
	}

	// Larger than any real manifest: store it as-is and stream the remainder,
	// rather than buffering something unbounded.
	if len(buf) == manifestSizeCap {
		n, werr := f.Write(buf)
		if werr != nil {
			return int64(n), werr
		}
		rest, cerr := io.Copy(f, body)

		return int64(n) + rest, cerr
	}

	out := injectUTCTiming(buf)
	if len(out) != len(buf) {
		// The stored headers came from the PUT request, so Content-Length now
		// describes the body we were given, not the one we will serve. Left in
		// place it truncates every response. Dropping it lets net/http set the
		// correct value.
		f.DropHeader("Content-Length")
	}
	n, werr := f.Write(out)

	return int64(n), werr
}

func getMaxAgeOr(s string, def int64) int64 {
	ret := def
	r := regexp.MustCompile(`max-age=(?P<maxage>\d*)`)
	match := r.FindStringSubmatch(s)
	for i, name := range r.SubexpNames() {
		if i > 0 && i <= len(match) {
			if name == "maxage" {
				valInt, err := strconv.ParseInt(match[i], 10, 64)
				if err == nil {
					ret = valInt
					break
				}
			}
		}
	}
	return ret
}

func getHeadersFiltered(headers http.Header) http.Header {
	ret := headers.Clone()

	// Clean up
	ret.Del("User-Agent")

	return ret
}
