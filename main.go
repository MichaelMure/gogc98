// Command gogc98 pulls periodic flight-recorder trace snapshots from
// a target program's HTTP endpoint (see ./demo, instrumented via the
// gogc98/probe package), decodes the traceallocfree experimental
// events into a live heap model, and serves a Win98-defrag-style
// visualization of it over a websocket.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/exp/trace"
	"golang.org/x/net/websocket"
)

func main() {
	target := flag.String("target", "http://127.0.0.1:7999/snapshot", "flight recorder snapshot URL of the target program")
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve the visualizer on")
	pollInterval := flag.Duration("poll", 250*time.Millisecond, "how often to pull a snapshot from the target")
	flag.Parse()

	state := newGCState()
	go pollLoop(state, *target, *pollInterval)

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) {
		serveWS(ws, state)
	}))

	log.Printf("gogc98 bridge listening on http://%s (pulling %s every %s)", *listen, *target, *pollInterval)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func pollLoop(state *gcState, target string, interval time.Duration) {
	for {
		resp, err := http.Get(target)
		if err != nil {
			log.Printf("poll: %v", err)
			time.Sleep(interval)
			continue
		}
		consumeSnapshot(state, resp.Body)
		resp.Body.Close()
		time.Sleep(interval)
	}
}

func consumeSnapshot(state *gcState, r io.Reader) {
	tr, err := trace.NewReader(r)
	if err != nil {
		log.Printf("trace reader: %v", err)
		return
	}

	now := time.Now()
	state.mu.Lock()
	defer state.mu.Unlock()

	// since is fixed for the whole batch: events within one poll can
	// legitimately share a trace timestamp (e.g. a background GC
	// worker on one P freeing something at the same tick as a mutator
	// alloc on another -- ties are only broken within a single P's own
	// buffer, not across Ps). Comparing against a mutating bound would
	// drop the second of any tied pair after processing the first.
	since := state.lastEventTime
	maxTime := since
	n := 0
	for {
		ev, err := tr.ReadEvent()
		if err != nil {
			// Expected, not just at EOF: the demo's flight recorder can
			// still be actively writing the newest trace generation when
			// it hands out a snapshot, so the tail of that generation is
			// routinely incomplete and unparseable here. Whatever events
			// were already read this poll are kept; the rest of this
			// generation arrives fully formed on a later poll.
			break
		}
		if ev.Time() <= since {
			continue
		}
		if ev.Time() > maxTime {
			maxTime = ev.Time()
		}
		n++
		switch ev.Kind() {
		case trace.EventExperimental:
			state.applyEvent(now, ev.Experimental())
		case trace.EventRangeBegin:
			// GC cycles aren't part of the traceallocfree experimental
			// batch (that's alloc/free/span events only) -- they're a
			// generic named range on the standard trace, scoped to the
			// whole program rather than one goroutine/proc. Only Begin
			// is handled: a cycle spanning a poll boundary would also
			// show up as EventRangeActive in the next snapshot, but at
			// its original (already-seen, already-recorded) start time,
			// so it's filtered out by the ev.Time() <= since check above
			// before reaching here.
			if r := ev.Range(); r.Name == "GC concurrent mark phase" {
				state.recordGCStart(now)
			}
		}
	}
	state.lastEventTime = maxTime

	// Sampled here, at the actual poll cadence, rather than on the
	// faster websocket render tick -- counts only change when a poll
	// runs, so sampling on the render tick mostly saw no change and
	// reported 0, then spiked on whichever tick happened to follow a
	// poll.
	state.allocRate.sample(now)
	state.freeRate.sample(now)
}

type controlMsg struct {
	Anchor uint64 `json:"anchor"`
	Freeze bool   `json:"freeze"`
}

func serveWS(ws *websocket.Conn, state *gcState) {
	view := &viewMode{}

	go func() {
		for {
			var msg controlMsg
			if err := websocket.JSON.Receive(ws, &msg); err != nil {
				return
			}
			view.mu.Lock()
			view.anchorSet = true
			view.anchor = msg.Anchor
			view.wantFreeze = msg.Freeze
			view.mu.Unlock()
		}
	}()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		frame := state.buildFrame(time.Now(), view)
		// Bounds how long a send can block on a peer that's gone dark
		// without a clean TCP close (so the reader goroutine above,
		// whose Receive blocking indefinitely is otherwise normal for
		// an idle-but-healthy connection, gets unblocked too once this
		// closes the connection).
		ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := websocket.JSON.Send(ws, frame); err != nil {
			return
		}
	}
}
