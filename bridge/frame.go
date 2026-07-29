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

// viewMode is one connection's view into the heap's id space. anchor
// is set by client control messages (mutex-guarded, written from a
// separate goroutine) once the client has scrolled at least once;
// windowStart/haveWindow are owned exclusively by the frame-building
// goroutine. Before the client has ever scrolled, the server jumps
// windowStart once to whatever page id is currently first-populated
// (so a fresh connection doesn't start staring at empty low page ids)
// and then leaves it alone -- there's no periodic re-centering here,
// the user is in full control of scrolling after that first jump.
type viewMode struct {
	mu        sync.Mutex
	anchor    uint64
	anchorSet bool

	windowStart uint64
	haveWindow  bool
}

type objectJSON struct {
	Idx    uint32  `json:"idx"`
	TypeID uint64  `json:"typeId"`
	Status string  `json:"status"` // "alive", "freed", or "" for a slot never individually observed
	Age    float64 `json:"age"`

	// XFrac/WidthFrac locate this fragment within its page, as a
	// fraction of one page's byte size (0..1). An object no bigger
	// than one page contributes a single fragment sized to its own
	// elemSize/pageSize share of the row; an object spanning several
	// pages contributes one fragment per page it touches, each
	// covering only the portion of the object that actually falls in
	// that page -- so a box's width reflects real byte size instead
	// of a fixed grid cell.
	XFrac     float64 `json:"xFrac"`
	WidthFrac float64 `json:"widthFrac"`
}

type pageJSON struct {
	ID        uint64       `json:"id"`
	SizeClass uint8        `json:"sizeClass"`
	ElemSize  uint32       `json:"elemSize"`
	Nelems    uint32       `json:"nelems"`
	Objects   []objectJSON `json:"objects"`

	// PageOffset/PageCount describe this row's place within its span
	// (npages can be >1, most commonly for objects bigger than one
	// page). Every row gets its own Objects list, not just the base
	// one -- see fragmentsByPage.
	PageOffset uint64 `json:"pageOffset"`
	PageCount  uint64 `json:"pageCount"`
}

// fragmentsByPage splits every tracked (or never-individually
// -observed) object in a span into per-physical-page fragments, keyed
// by page offset within the span. A page-boundary-straddling object
// (common once a span has more than one page, since Go doesn't align
// size-class slots to physical pages) contributes a fragment to each
// side of the boundary, sized to only the portion that actually falls
// on that page.
func fragmentsByPage(sp *span, now time.Time) map[uint64][]objectJSON {
	if sp.elemSize == 0 {
		return nil
	}
	shown := sp.nelems
	if shown > slotCap {
		shown = slotCap
	}
	frags := make(map[uint64][]objectJSON)
	add := func(idx uint32, typeID uint64, status string, age float64) {
		start := uint64(idx) * uint64(sp.elemSize)
		end := start + uint64(sp.elemSize)
		for p := start / pageSize; p <= (end-1)/pageSize; p++ {
			pageStart, pageEnd := p*pageSize, (p+1)*pageSize
			ovStart, ovEnd := max(start, pageStart), min(end, pageEnd)
			frags[p] = append(frags[p], objectJSON{
				Idx: idx, TypeID: typeID, Status: status, Age: age,
				XFrac:     float64(ovStart-pageStart) / pageSize,
				WidthFrac: float64(ovEnd-ovStart) / pageSize,
			})
		}
	}
	for idx := uint32(0); idx < shown; idx++ {
		if obj, ok := sp.objects[idx]; ok {
			add(idx, obj.typeID, string(obj.status), now.Sub(obj.changed).Seconds())
		} else {
			add(idx, 0, "", 0)
		}
	}
	return frags
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

	// SlotCap and FadeWindowSeconds mirror the same-named server-side
	// constants (slotCap, fadeWindow in state.go). Sent once per frame
	// rather than hardcoded a second time in app.js, so the two sides
	// can't drift out of sync the way slotCap/SERVER_SLOT_CAP used to.
	SlotCap           int     `json:"slotCap"`
	FadeWindowSeconds float64 `json:"fadeWindowSeconds"`
}

// buildFrame renders a stable, contiguous window of page ids. Once the
// client has scrolled, the window always starts at their chosen
// anchor. Until then, it jumps exactly once to center on the first
// currently-tracked page id, and otherwise doesn't move on its own --
// no periodic re-centering, so the user's scroll position is never
// yanked out from under them.
func (g *gcState) buildFrame(now time.Time, view *viewMode) frameJSON {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.prune(now)

	view.mu.Lock()
	anchorSet, anchor := view.anchorSet, view.anchor
	view.mu.Unlock()

	if anchorSet {
		view.windowStart = anchor
	} else if !view.haveWindow {
		if first, ok := g.firstSpanID(); ok {
			if first > windowCount/2 {
				view.windowStart = first - windowCount/2
			} else {
				view.windowStart = 0
			}
			view.haveWindow = true
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
		if lo >= hi {
			continue
		}

		fragsByPage := fragmentsByPage(sp, now)
		for id := lo; id < hi; id++ {
			pageOffset := id - sp.id
			pages = append(pages, pageJSON{
				ID: id, SizeClass: sp.sizeClass, ElemSize: sp.elemSize,
				Nelems:     sp.nelems,
				PageOffset: pageOffset, PageCount: npages,
				Objects: fragsByPage[pageOffset],
			})
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
		SlotCap:           slotCap,
		FadeWindowSeconds: fadeWindow.Seconds(),
		Metrics: metricsJSON{
			TotalEvents: g.totalEvents, SpanAllocs: g.spanAllocs, SpanFrees: g.spanFrees,
			AllocsPerSec: g.allocRate.perSec, FreesPerSec: g.freeRate.perSec,
			TrackedSpans: len(g.spans),
		},
	}
}
