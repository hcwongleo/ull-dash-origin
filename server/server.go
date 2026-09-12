package server

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

var (
	cleanUpChannel = make(chan bool)
)

// Options configures the server. A struct rather than a positional argument
// list so that adding an option cannot silently reorder existing ones.
type Options struct {
	BasePath                     string
	IngestIdleTimeout            time.Duration
	AdminAddr                    string
	LogLevel                     string
	GitSHA                       string
	UTCTiming                    string
	InitPattern                  string
	IdleSweep                    time.Duration
	WaitTimeout                  time.Duration
	PersistInit                  bool
	Port                         int
	CertFilePath                 string
	KeyFilePath                  string
	CorsConfigFilePath           string
	OnlyRAM                      bool
	WaitForDataToArrive          bool
	DoCleanupBasedOnCacheHeaders bool
}

// StartHTTPServer Starts the webserver
func StartHTTPServer(o Options) error {
	var err error

	basePath := o.BasePath
	port := o.Port
	certFilePath := o.CertFilePath
	keyFilePath := o.KeyFilePath
	corsConfigFilePath := o.CorsConfigFilePath
	onlyRAM := o.OnlyRAM
	waitForDataToArrive := o.WaitForDataToArrive
	doCleanupBasedOnCacheHeaders := o.DoCleanupBasedOnCacheHeaders

	SetLogLevel(o.LogLevel)
	SetUTCTiming(o.UTCTiming)

	if err := SetInitPattern(o.InitPattern); err != nil {
		return fmt.Errorf("bad -init-pattern %q: %w", o.InitPattern, err)
	}
	if o.InitPattern == "" {
		logWarnf("no -init-pattern: initialisation segments will be swept, and every viewer joining afterwards will fail")
	}
	SetIdleSweep(o.IdleSweep)
	StartSweeper(time.Minute)
	defer StopSweeper()

	if o.UTCTiming == "" {
		logWarnf("UTCTiming injection disabled: players will fall back to the device clock, which mis-computes the live edge")
	} else {
		log.Printf("UTCTiming injected on manifest ingest: %s", o.UTCTiming)
	}
	SetIngestIdleTimeout(o.IngestIdleTimeout)
	log.Printf("ingest idle timeout: %s", o.IngestIdleTimeout)
	StartAdminListener(o.AdminAddr, o.GitSHA)

	cors := NewCors()
	if corsConfigFilePath != "" {
		// Loads CORS config
		err = cors.LoadFromDisc(corsConfigFilePath)
		if err != nil {
			return err
		}
	} else {
		log.Printf("CORS default policy applied")
	}
	log.Printf("CORS: %s", cors.String())

	SetWaitTimeout(o.WaitTimeout)

	// Set up persistence AFTER the init pattern, which decides what qualifies, and
	// reload before the listener opens so a viewer never sees a gap.
	if o.PersistInit {
		SetPersistDir(basePath)
		LoadPersisted()
	} else {
		SetPersistDir("")
		logWarnf("-persist-init is off: after any restart, new viewers cannot play until the Elemental output group is restarted")
	}

	var waitingRequests *WaitingRequests = nil
	if waitForDataToArrive {
		log.Printf("holding GETs for not-yet-arrived segments for up to %s; this MUST exceed the MPD availabilityTimeOffset", requestExpiration())
		waitingRequests = NewWaitingRequests()
	}

	r := mux.NewRouter()

	r.PathPrefix("/").Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer w.(http.Flusher).Flush()
		logDebugf("%s %s", r.Method, r.URL.String())
		switch r.Method {
		case http.MethodGet:
			GetHandler(waitingRequests, cors, basePath, w, r)
		case http.MethodHead:
			HeadHandler(cors, w, r)
		case http.MethodPost:
			PostHandler(waitingRequests, onlyRAM, cors, basePath, w, r)
		case http.MethodPut:
			PutHandler(waitingRequests, onlyRAM, cors, basePath, w, r)
		case http.MethodDelete:
			DeleteHandler(onlyRAM, cors, basePath, w, r)
		case http.MethodOptions:
			OptionsHandler(cors, w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})).Methods(http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions)

	if doCleanupBasedOnCacheHeaders {
		startCleanUp(basePath, 1000)
	}

	// An explicit server, not ListenAndServe. ReadTimeout stays zero on purpose:
	// it would bound the whole request and so kill a legitimate long chunked PUT.
	// Silence is bounded per-read instead, in withIdleDeadline.
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           r,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if (certFilePath != "") && (keyFilePath != "") {
		// Try HTTPS
		log.Printf("HTTPS server running on port %d", port)
		err = srv.ListenAndServeTLS(certFilePath, keyFilePath)
	} else {
		// Try HTTP
		log.Printf("HTTP server running on port %d", port)
		err = srv.ListenAndServe()
	}

	if doCleanupBasedOnCacheHeaders {
		stopCleanUp()
	}
	if waitingRequests != nil {
		waitingRequests.Close()
	}

	return err
}

func startCleanUp(basePath string, periodMs int64) {
	go runCleanupEvery(basePath, periodMs, cleanUpChannel)

	log.Printf("HTTP Started clean up thread")
}

func stopCleanUp() {
	// Send finish signal
	cleanUpChannel <- true

	// Wait to finish
	<-cleanUpChannel

	log.Printf("HTTP Stopped clean up thread")
}

func runCleanupEvery(basePath string, periodMs int64, cleanUpChannelBidi chan bool) {
	timeCh := time.NewTicker(time.Millisecond * time.Duration(periodMs))
	exit := false

	for !exit {
		select {
		// Wait for the next tick
		case tm := <-timeCh.C:
			cacheCleanUp(basePath, tm)

		case <-cleanUpChannelBidi:
			exit = true
		}
	}
	// Indicates finished
	cleanUpChannelBidi <- true

	log.Printf("HTTP Exited clean up thread")
}

func cacheCleanUp(basePath string, now time.Time) {
	filesToDel := map[string]*File{}

	// TODO: This is a brute force approach, optimization recommended

	FilesLock.Lock()
	defer FilesLock.Unlock()

	// Check for expired files
	for key, file := range Files {
		if file.maxAgeS >= 0 && file.eof {
			if file.receivedAt.Add(time.Second * time.Duration(file.maxAgeS)).Before(now) {
				filesToDel[key] = file
			}
		}
	}
	// Delete expired files
	for keyToDel, fileToDel := range filesToDel {
		// Delete from array
		delete(Files, keyToDel)
		if fileToDel.onDisk {
			fileToDel.RemoveFromDisk(basePath)
		}
		log.Printf("CLEANUP expired, deleted: %s", keyToDel)
	}
}
