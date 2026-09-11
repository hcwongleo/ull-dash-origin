package main

import (
	"flag"
	"log"
	"time"

	"github.com/mjneil/go-chunked-streaming-server/server"
)

// gitSHA is stamped at build time with -ldflags "-X main.gitSHA=...". It is
// reported by the admin endpoint so a running process can be identified without
// guessing which artefact was deployed.
var gitSHA = "dev"

var (
	certFilePath                 = flag.String("c", "", "Certificate file path (only for https)")
	keyFilePath                  = flag.String("k", "", "Key file path (only for https)")
	baseOutPath                  = flag.String("p", "./content", "Path used to store")
	port                         = flag.Int("i", 9094, "Port used for HTTP ingress/ egress")
	corsConfigFilePath           = flag.String("o", "", "JSON file path with the CORS headers definition")
	onlyRAM                      = flag.Bool("r", false, "Indicates DO NOT use disc as persistent/fallback storage (only RAM)")
	waitForDataToArrive          = flag.Bool("w", false, "Indicates to GET request to wait for some specific if data is NOT present yet")
	doCleanupBasedOnCacheHeaders = flag.Bool("d", false, "Indicates to remove files from the server based on original Cache-Control (max-age) header")

	// Added for unattended operation. Neither depends on the encoder's DASH
	// configuration, which is deliberate: a flag that has to track
	// availabilityTimeOffset or timeShiftBufferDepth becomes wrong silently the
	// first time the encoder is reconfigured.
	ingestIdleTimeoutS = flag.Int64("ingest-idle-timeout", 10, "Abort an ingest whose body has been silent this many seconds. Bounds silence, not total duration, so long chunked PUTs are unaffected. 0 disables, which leaks a goroutine and socket per half-open encoder connection")
	adminAddr          = flag.String("admin-addr", "127.0.0.1:9095", "Address for the local-only health and metrics listener. Empty disables it")
	logLevelName       = flag.String("log-level", "warn", "Log level: error, warn, info, debug. Per-request logging is debug-only")

	// A DASH player with no UTCTiming falls back to the device clock, mis-computes
	// the live edge and jumps gaps. Elemental Live does not emit it, so the origin
	// adds it as the manifest is ingested. Empty disables injection.
	//
	// A constant, so unlike a latency target it cannot fall out of step when the
	// encoder is reconfigured - which is why this, and not ServiceDescription, is
	// the one thing the origin rewrites.
	utcTimingURL = flag.String("utc-timing", "https://time.akamai.com/?iso", "URL injected as the DASH UTCTiming value on manifest ingest, using the http-iso scheme. Empty disables injection")
)

func checkError(err error) {
	if err != nil {
		log.Fatalln(err)
	}
}

func main() {
	flag.Parse()

	checkError(server.StartHTTPServer(server.Options{
		BasePath:                     *baseOutPath,
		Port:                         *port,
		CertFilePath:                 *certFilePath,
		KeyFilePath:                  *keyFilePath,
		CorsConfigFilePath:           *corsConfigFilePath,
		OnlyRAM:                      *onlyRAM,
		DoCleanupBasedOnCacheHeaders: *doCleanupBasedOnCacheHeaders,
		WaitForDataToArrive:          *waitForDataToArrive,
		IngestIdleTimeout:            time.Duration(*ingestIdleTimeoutS) * time.Second,
		AdminAddr:                    *adminAddr,
		LogLevel:                     *logLevelName,
		UTCTiming:                    *utcTimingURL,
		GitSHA:                       gitSHA,
	}))
}
