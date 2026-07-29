package main

import (
	"sort"
	"sync"
	"time"
)

// windowCount is how many contiguous page ids the display shows at
// once, one per row.
const windowCount = 120

// slotCap is how many objects render per page row, regardless of that
// page's real object count (1 to 1024 depending on size class). Size
// classes with more objects than this get silently clipped to the
// first slotCap by index; the frontend may cap further to fit the
// available row width.
const slotCap = 256

// viewState is one connection's view into the heap's id space. auto
// and anchor are set by client control messages (mutex-guarded,
// written from a separate goroutine); windowStart/haveWindow are
// hysteresis state owned exclusively by the frame-building goroutine,
// so a page's screen position stays stable across frames instead of
// reshuffling every tick -- only actually panning (auto drifting, or
// a manual scroll) should move anything.
type viewMode struct {
	mu     sync.Mutex
	auto   bool
	anchor uint64

	windowStart uint64
	haveWindow  bool

	// outsideStreak counts consecutive frames where the hottest page
	// has been outside the window, so a single-tick activity spike on
	// some far-off page can't yank the view for one frame and then
	// bounce back -- only sustained drift actually pans it.
	outsideStreak int
}

// recenterDebounce is how many consecutive frames the hottest page
// must stay outside the window before auto-follow actually pans,
// filtering out single-frame activity noise.
const recenterDebounce = 4

type objectJSON struct {
	Idx    uint32  `json:"idx"`
	TypeID uint64  `json:"typeId"`
	Status string  `json:"status"`
	Age    float64 `json:"age"`
}

type pageJSON struct {
	ID        uint64       `json:"id"`
	SizeClass uint8        `json:"sizeClass"`
	ElemSize  uint32       `json:"elemSize"`
	Nelems    uint32       `json:"nelems"`
	Objects   []objectJSON `json:"objects"`

	// PageOffset/PageCount describe this row's place within its span
	// (npages can be >1, most commonly for large objects that get a
	// dedicated multi-page span). PageOffset 0 is the span's base row,
	// which carries the real per-object grid; PageOffset > 0 rows are
	// physically part of the same allocation and must not be rendered
	// as empty, but don't get their own Objects list -- RepType is a
	// representative type id from the span for coloring them.
	PageOffset uint64 `json:"pageOffset"`
	PageCount  uint64 `json:"pageCount"`
	RepType    uint64 `json:"repType"`
}

// representativeType returns a type id for coloring a span's
// continuation rows, since those rows don't carry their own object
// list. Deterministically picks the object at the lowest index rather
// than ranging over the map (whose iteration order Go randomizes per
// call), so a continuation row's color doesn't flicker every frame.
func representativeType(sp *span) uint64 {
	found := false
	var minIdx uint32
	var typeID uint64
	for idx, obj := range sp.objects {
		if !found || idx < minIdx {
			minIdx, typeID, found = idx, obj.typeID, true
		}
	}
	return typeID
}

type summaryJSON struct {
	ID       uint64  `json:"id"`
	Activity float64 `json:"activity"`
}

type metricsJSON struct {
	TotalEvents  int64   `json:"totalEvents"`
	SpanAllocs   int64   `json:"spanAllocs"`
	SpanFrees    int64   `json:"spanFrees"`
	AllocsPerSec float64 `json:"allocsPerSec"`
	FreesPerSec  float64 `json:"freesPerSec"`
	TrackedSpans int     `json:"trackedSpans"`
}

type frameJSON struct {
	Type        string        `json:"type"`
	WindowStart uint64        `json:"windowStart"`
	WindowCount int           `json:"windowCount"`
	Pages       []pageJSON    `json:"pages"`
	Summary     []summaryJSON `json:"summary"`
	Metrics     metricsJSON   `json:"metrics"`
	Auto        bool          `json:"auto"`

	// SlotCap and FadeWindowSeconds mirror the same-named server-side
	// constants (slotCap, fadeWindow in state.go). Sent once per frame
	// rather than hardcoded a second time in app.js, so the two sides
	// can't drift out of sync the way slotCap/SERVER_SLOT_CAP used to.
	SlotCap           int     `json:"slotCap"`
	FadeWindowSeconds float64 `json:"fadeWindowSeconds"`
}

// buildFrame renders a stable, contiguous window of page ids: in
// manual mode the window starts at the client's chosen anchor; in
// auto mode it only pans when the currently hottest page has drifted
// outside the window, rather than re-picking "top N by activity"
// fresh every tick, which would reshuffle the whole ribbon on every
// frame.
func (g *gcState) buildFrame(now time.Time, view *viewMode) frameJSON {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.prune(now)

	view.mu.Lock()
	auto, anchor := view.auto, view.anchor
	view.mu.Unlock()

	if !auto {
		view.windowStart = anchor
		view.haveWindow = true
	} else {
		var hottest uint64
		hottestScore := -1.0
		for id, sp := range g.spans {
			if sp.activity > hottestScore {
				hottestScore = sp.activity
				hottest = id
			}
		}
		outside := !view.haveWindow || hottest < view.windowStart || hottest >= view.windowStart+windowCount
		if outside {
			view.outsideStreak++
		} else {
			view.outsideStreak = 0
		}
		if !view.haveWindow || view.outsideStreak >= recenterDebounce {
			if hottest > windowCount/2 {
				view.windowStart = hottest - windowCount/2
			} else {
				view.windowStart = 0
			}
			view.haveWindow = true
			view.outsideStreak = 0
		}
	}

	windowEnd := view.windowStart + windowCount
	pages := make([]pageJSON, 0, windowCount)
	for _, sp := range g.spans {
		npages := sp.npages
		if npages == 0 {
			npages = 1 // not yet known from a Span/SpanAlloc event; assume 1 page
		}
		spanEnd := sp.id + npages
		lo, hi := max(sp.id, view.windowStart), min(spanEnd, windowEnd)
		for id := lo; id < hi; id++ {
			pj := pageJSON{
				ID: id, SizeClass: sp.sizeClass, ElemSize: sp.elemSize,
				Nelems:     sp.nelems,
				PageOffset: id - sp.id, PageCount: npages,
				RepType: representativeType(sp),
			}
			if id == sp.id {
				for _, obj := range sp.objects {
					if obj.idx >= slotCap {
						continue
					}
					pj.Objects = append(pj.Objects, objectJSON{
						Idx: obj.idx, TypeID: obj.typeID, Status: string(obj.status),
						Age: now.Sub(obj.changed).Seconds(),
					})
				}
			}
			pages = append(pages, pj)
		}
	}

	summary := make([]summaryJSON, 0, len(g.spans))
	for id, sp := range g.spans {
		summary = append(summary, summaryJSON{ID: id, Activity: sp.activity})
	}
	sort.Slice(summary, func(i, j int) bool { return summary[i].ID < summary[j].ID })

	return frameJSON{
		Type:              "frame",
		WindowStart:       view.windowStart,
		WindowCount:       windowCount,
		Pages:             pages,
		Summary:           summary,
		Auto:              auto,
		SlotCap:           slotCap,
		FadeWindowSeconds: fadeWindow.Seconds(),
		Metrics: metricsJSON{
			TotalEvents: g.totalEvents, SpanAllocs: g.spanAllocs, SpanFrees: g.spanFrees,
			AllocsPerSec: g.allocRate.perSec, FreesPerSec: g.freeRate.perSec,
			TrackedSpans: len(g.spans),
		},
	}
}
