// Command demo is a small allocation-heavy program that shows how to
// instrument a Go program with gogc98/probe for the gogc98 visualizer
// to poll.
//
// The program runs its workload independently of any visualizer. The
// probe package buffers recent trace data in memory; an HTTP endpoint
// lets the gogc98 visualizer pull a snapshot whenever it wants, the
// same pattern net/http/pprof uses.
package main

import (
	"time"

	"gogc98/probe"
)

// Go's allocator buckets objects into a span purely by size class,
// not by type — a page only ever mixes types that happen to round to
// the same byte size. So for each size class we care about, define a
// pair of distinctly-named types with the same layout, to actually
// see multiple colors land on the same page.
type smallStruct struct { // 32 bytes -> size class 32
	A, B int64
	C    string
}

type smallStructAlt struct { // 32 bytes -> size class 32
	X, Y int64
	Tag  string
}

type mediumStruct struct { // 56 bytes -> size class 64
	Data [4]int64
	Name string
	Next *mediumStruct
}

type mediumStructAlt struct { // 56 bytes -> size class 64
	Values [4]int64
	Label  string
	Prev   *mediumStructAlt
}

// bigTyped is deliberately 128 bytes, exactly the size class of
// byteBlob's backing array below, to contrast a real type against
// opaque bytes on the same page.
type bigTyped struct {
	Data [16]int64
}

type byteBlob struct {
	Data []byte // the []byte backing array itself carries no useful type info
}

func main() {
	go probe.Start()

	// The real workload runs regardless of whether a bridge is pulling
	// snapshots. Keep some objects alive across GC cycles (survivors)
	// and let others churn (garbage), so both HeapObjectAlloc and
	// HeapObjectFree show up, and alternate between sibling types so
	// pages actually end up multi-colored instead of one dominant
	// churn source drowning everything else out.
	var keepSmall []any
	var keepMedium []any

	i := 0
	for {
		i++

		if i%2 == 0 {
			s := &smallStruct{A: int64(i), B: int64(i * 2), C: "hello"}
			if i%3 == 0 {
				keepSmall = append(keepSmall, s)
			}
		} else {
			s := &smallStructAlt{X: int64(i), Y: int64(i * 2), Tag: "world"}
			if i%3 == 0 {
				keepSmall = append(keepSmall, s)
			}
		}
		if len(keepSmall) > 300 {
			keepSmall = keepSmall[1:]
		}

		if i%2 == 0 {
			m := &mediumStruct{Name: "node"}
			if i%7 == 0 {
				keepMedium = append(keepMedium, m)
			}
		} else {
			m := &mediumStructAlt{Label: "vertex"}
			if i%7 == 0 {
				keepMedium = append(keepMedium, m)
			}
		}
		if len(keepMedium) > 150 {
			keepMedium = keepMedium[1:]
		}

		// Pure churn, alternating a typed struct against opaque bytes
		// on the same size class, throttled so it doesn't dominate
		// every other allocation's chance at being "hot".
		if i%2 == 0 {
			if i%4 == 0 {
				_ = &bigTyped{}
			} else {
				_ = &byteBlob{Data: make([]byte, 128)}
			}
		}

		if i%10 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
}
