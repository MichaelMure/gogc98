// Package probe adds gogc98 instrumentation to a Go program: an
// in-process flight recorder plus an HTTP endpoint that serves its
// latest trace snapshot, in the shape the gogc98 visualizer polls for.
//
// It requires the traceallocfree runtime experiment, which is not in
// the //go:debug allowlist and so cannot be enabled from within the
// program itself -- the runtime parses GODEBUG before any Go code
// runs, including init(). The process must be launched with
// GODEBUG=traceallocfree=1 already set in its environment; Start
// logs why and exits the process if it isn't.
//
// Usage is a single line, typically run in its own goroutine since
// Start blocks:
//
//	go probe.Start()
package probe

import (
	"log"
	"net/http"
	"os"
	"runtime/trace"
	"strings"
	"time"
)

// Addr is where Start serves the snapshot endpoint -- the gogc98
// visualizer's default poll target.
const Addr = "127.0.0.1:7999"

// Start starts an in-process flight recorder and serves its latest
// trace snapshot at http://127.0.0.1:7999/snapshot on each request.
// It blocks, like http.ListenAndServe, so callers typically run it in
// its own goroutine with go probe.Start().
func Start() {
	if !strings.Contains(os.Getenv("GODEBUG"), "traceallocfree=1") {
		log.Fatal("probe: GODEBUG=traceallocfree=1 is not set; it must be set in the process environment at launch, since the Go runtime reads GODEBUG before main() runs")
	}

	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{
		MinAge:   200 * time.Millisecond,
		MaxBytes: 8 << 20,
	})
	if err := fr.Start(); err != nil {
		log.Fatal("probe: starting flight recorder: ", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fr.WriteTo(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	log.Print("probe: snapshot endpoint on http://", Addr, "/snapshot")
	log.Fatal(http.ListenAndServe(Addr, mux))
}
