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
// and wantFreeze are set by client control messages (mutex-guarded,
// written from a separate goroutine) once the client has scrolled at
// least once; windowStart/haveWindow/frozen are owned exclusively by
// the frame-building goroutine. Before the client has ever scrolled,
// the server jumps windowStart once to whatever page id is currently
// first-populated (so a fresh connection doesn't start staring at
// empty low page ids) and then leaves it alone -- there's no periodic
// re-centering here, the user is in full control of scrolling after
// that first jump.
type viewMode struct {
	mu         sync.Mutex
	anchor     uint64
	anchorSet  bool
	wantFreeze bool

	windowStart uint64
	haveWindow  bool

	// frozen holds a point-in-time snapshot of the heap model while
	// the client has paused the view, so scrolling around a frozen
	// view explores a stable heap state instead of only the window
	// position being held still. nil means live.
	frozen *frozenSnapshot
}

// frozenSnapshot is a deep copy of the heap model taken the moment a
// connection freezes, plus the metrics and clock reading that go with
// it -- so a frozen frame's fade animations stop advancing too,
// instead of freezing the objects but not their ages.
type frozenSnapshot struct {
	at       time.Time
	spans    map[uint64]*span
	metrics  metricsJSON
	gcEvents []time.Time
}

func snapshotSpans(live map[uint64]*span) map[uint64]*span {
	out := make(map[uint64]*span, len(live))
	for id, sp := range live {
		cp := *sp
		cp.objects = make(map[uint32]*object, len(sp.objects))
		for idx, obj := range sp.objects {
			objCopy := *obj
			cp.objects[idx] = &objCopy
		}
		out[id] = &cp
	}
	return out
}

func (g *gcState) metricsSnapshot() metricsJSON {
	var trackedPages uint64
	arenaBlocks := make(map[uint64]struct{})
	for _, sp := range g.spans {
		npages := sp.npages
		if npages == 0 {
			npages = 1
		}
		trackedPages += npages
		for p := uint64(0); p < npages; p++ {
			arenaBlocks[(sp.id+p)/arenaPages] = struct{}{}
		}
	}
	return metricsJSON{
		TotalEvents: g.totalEvents, SpanAllocs: g.spanAllocs, SpanFrees: g.spanFrees,
		AllocsPerSec: g.allocRate.perSec, FreesPerSec: g.freeRate.perSec,
		TrackedSpans: len(g.spans), TrackedPages: trackedPages,
		TrackedArenaBlocks: len(arenaBlocks),
	}
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

// rowJSON is one display row. Kind is "page" (a real, tracked page --
// PageOffset/PageCount describe its place within its span, since
// npages can be >1), "void" (a single untracked page id), "gap" (a
// run of more than gapCollapseThreshold consecutive untracked ids,
// collapsed into one row so a long unused stretch of address space
// doesn't force scrolling through hundreds of blank rows -- see
// buildRows), or "end" (nothing tracked anywhere at or beyond this id
// -- an open-ended stretch, not a bounded gap, so it's the last row
// in the frame rather than one of many). ID is the page id for
// page/void/end, or the first id in the run for gap.
type rowJSON struct {
	Kind string `json:"kind"`
	ID   uint64 `json:"id"`

	GapLen uint64 `json:"gapLen,omitempty"` // gap only

	// page only. ElemSize/Nelems are deliberately not omitempty: 0 is
	// their real value when a span's own Span/SpanAlloc event hasn't
	// been seen yet (its size class is genuinely unknown, not just
	// zero) -- omitting the field entirely for that case would leave
	// it undefined in JS instead of a value the frontend can check for.
	SizeClass  uint8        `json:"sizeClass,omitempty"`
	ElemSize   uint32       `json:"elemSize"`
	Nelems     uint32       `json:"nelems"`
	Objects    []objectJSON `json:"objects,omitempty"`
	PageOffset uint64       `json:"pageOffset,omitempty"`
	PageCount  uint64       `json:"pageCount,omitempty"`
}

// gapCollapseThreshold is how many consecutive untracked page ids have
// to appear before they're collapsed into a single gap marker row.
// There's no way to directly tell "this is a different heap arena"
// apart from "this is unused space within the current one" using the
// traceallocfree data this tool has -- that would need real arena
// boundaries, which aren't exposed anywhere reachable without
// patching the runtime. So this is set deliberately very high: well
// above anything a single contiguous arena run would plausibly leave
// unused, and well below the ~134M-page spacing Go uses between its
// arena-placement hints (64MB arenas, hints roughly 1TB apart). A gap
// this large is almost certainly a genuine jump to non-contiguous
// address space, not ordinary unused-but-reserved space. Smaller
// unused stretches -- even thousands of pages -- render as plain,
// individually scrollable blank rows, since that IS real information
// about the heap's layout.
const gapCollapseThreshold = 1_000_000

// spanRange is a span's page-id extent, used to walk the id space in
// order without visiting every untracked id one at a time.
type spanRange struct {
	lo, hi uint64
	sp     *span
}

func sortedSpanRanges(spans map[uint64]*span) []spanRange {
	ranges := make([]spanRange, 0, len(spans))
	for _, sp := range spans {
		npages := sp.npages
		if npages == 0 {
			npages = 1
		}
		ranges = append(ranges, spanRange{lo: sp.id, hi: sp.id + npages, sp: sp})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].lo < ranges[j].lo })
	return ranges
}

// buildRows walks the page-id space forward from windowStart,
// producing exactly windowCount rows: a "page" row per real tracked
// page, and either individual "void" rows or a single collapsed "gap"
// row for runs of untracked ids, depending on how long the run is.
func buildRows(windowStart uint64, spans map[uint64]*span, frameTime time.Time) []rowJSON {
	ranges := sortedSpanRanges(spans)
	rangeIdx := 0
	cur := windowStart
	rows := make([]rowJSON, 0, windowCount)

	for len(rows) < windowCount {
		for rangeIdx < len(ranges) && ranges[rangeIdx].hi <= cur {
			rangeIdx++
		}

		if rangeIdx < len(ranges) && ranges[rangeIdx].lo <= cur {
			sp := ranges[rangeIdx].sp
			npages := sp.npages
			if npages == 0 {
				npages = 1
			}
			pageOffset := cur - sp.id
			frags := fragmentsByPage(sp, frameTime)
			rows = append(rows, rowJSON{
				Kind: "page", ID: cur,
				SizeClass: sp.sizeClass, ElemSize: sp.elemSize, Nelems: sp.nelems,
				PageOffset: pageOffset, PageCount: npages,
				Objects: frags[pageOffset],
			})
			cur++
			continue
		}

		// cur is untracked. If nothing is tracked anywhere at or beyond
		// it, this is an open-ended stretch, not a bounded gap between
		// two real spans -- one terminal row says so and the walk
		// stops, rather than manufacturing a fake boundary out of
		// however many rows happen to be left in the budget.
		if rangeIdx >= len(ranges) {
			rows = append(rows, rowJSON{Kind: "end", ID: cur})
			break
		}

		gapEnd := ranges[rangeIdx].lo
		gapLen := gapEnd - cur
		if gapLen > gapCollapseThreshold {
			rows = append(rows, rowJSON{Kind: "gap", ID: cur, GapLen: gapLen})
			cur = gapEnd
		} else {
			rows = append(rows, rowJSON{Kind: "void", ID: cur})
			cur++
		}
	}
	return rows
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

	// Fullness is the fraction (0..1) of this span's capacity (nelems)
	// currently occupied by an object we've directly observed as
	// alive. It undercounts for objects allocated before this bridge
	// started watching (we only know about an idx once we've seen an
	// event for it), same limitation as the "?B" unknown-geometry
	// case elsewhere -- an approximation, not a precise occupancy.
	Fullness float64 `json:"fullness"`

	// LastEventSeconds is how long ago (in seconds) any event last
	// touched this span -- a plain recency measurement, unlike
	// Activity: that's a cumulative decaying score, so a single burst
	// can inflate it into the hundreds or thousands, which then takes
	// many seconds (at the 1.5s half-life) to decay back down even
	// with nothing further happening. That made a "changed recently"
	// threshold on Activity read as permanently true for almost
	// everything. This is a direct wall-clock measurement instead, so
	// it reflects actual recency regardless of how big a burst came
	// before it.
	LastEventSeconds float64 `json:"lastEventSeconds"`
}

func fullness(sp *span) float64 {
	if sp.nelems == 0 {
		return 0
	}
	alive := 0
	for _, obj := range sp.objects {
		if obj.status == objAlive {
			alive++
		}
	}
	return min(1, float64(alive)/float64(sp.nelems))
}

type metricsJSON struct {
	TotalEvents  int64   `json:"totalEvents"`
	SpanAllocs   int64   `json:"spanAllocs"`
	SpanFrees    int64   `json:"spanFrees"`
	AllocsPerSec float64 `json:"allocsPerSec"`
	FreesPerSec  float64 `json:"freesPerSec"`
	TrackedSpans int     `json:"trackedSpans"`
	TrackedPages uint64  `json:"trackedPages"` // sum of npages across TrackedSpans -- a multi-page span counts once as a span but several as pages

	// TrackedArenaBlocks is how many distinct arenaPages-sized blocks
	// the tracked pages fall into -- an approximation of real Go arena
	// count, see arenaPages.
	TrackedArenaBlocks int `json:"trackedArenaBlocks"`
}

type frameJSON struct {
	Type        string        `json:"type"`
	WindowStart uint64        `json:"windowStart"`
	WindowCount int           `json:"windowCount"`
	Rows        []rowJSON     `json:"rows"`
	Summary     []summaryJSON `json:"summary"`
	Metrics     metricsJSON   `json:"metrics"`

	// SlotCap and FadeWindowSeconds mirror the same-named server-side
	// constants (slotCap, fadeWindow in state.go). Sent once per frame
	// rather than hardcoded a second time in app.js, so the two sides
	// can't drift out of sync the way slotCap/SERVER_SLOT_CAP used to.
	SlotCap           int     `json:"slotCap"`
	FadeWindowSeconds float64 `json:"fadeWindowSeconds"`

	// GCEvents is how many seconds ago each recent GC cycle started
	// (oldest first), bounded to GCHistorySeconds -- the frontend plots
	// these as a timeline strip under the minimap. Unlike the minimap
	// itself, this axis is time, not address space.
	GCEvents         []float64 `json:"gcEvents"`
	GCHistorySeconds float64   `json:"gcHistorySeconds"`
}

// buildFrame renders a stable, contiguous window of page ids. Once the
// client has scrolled, the window always starts at their chosen
// anchor. Until then, it jumps exactly once to center on the first
// currently-tracked page id, and otherwise doesn't move on its own --
// no periodic re-centering, so the user's scroll position is never
// yanked out from under them. While frozen, the window can still move
// (scrolling keeps working), but it moves over a frozen snapshot of
// the heap instead of the live one, taken the moment freeze was
// requested.
func (g *gcState) buildFrame(now time.Time, view *viewMode) frameJSON {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.prune(now)

	view.mu.Lock()
	anchorSet, anchor, wantFreeze := view.anchorSet, view.anchor, view.wantFreeze
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

	switch {
	case wantFreeze && view.frozen == nil:
		view.frozen = &frozenSnapshot{
			at: now, spans: snapshotSpans(g.spans), metrics: g.metricsSnapshot(),
			gcEvents: append([]time.Time(nil), g.gcEvents...),
		}
	case !wantFreeze && view.frozen != nil:
		view.frozen = nil
	}

	spans, metrics, frameTime, gcEvents := g.spans, g.metricsSnapshot(), now, g.gcEvents
	if view.frozen != nil {
		spans, metrics, frameTime, gcEvents = view.frozen.spans, view.frozen.metrics, view.frozen.at, view.frozen.gcEvents
	}

	rows := buildRows(view.windowStart, spans, frameTime)

	// One summary entry per page the span actually covers, not just its
	// base id -- otherwise a multi-page span's continuation pages have
	// no minimap representation at all (not just missing fullness, no
	// tick whatsoever), the same "fills every page it spans" principle
	// the main view already applies to these.
	summary := make([]summaryJSON, 0, len(spans))
	for id, sp := range spans {
		npages := sp.npages
		if npages == 0 {
			npages = 1
		}
		full := fullness(sp)
		lastEventSeconds := frameTime.Sub(sp.lastEvent).Seconds()
		for p := uint64(0); p < npages; p++ {
			summary = append(summary, summaryJSON{
				ID: id + p, Activity: sp.activity, Fullness: full,
				LastEventSeconds: lastEventSeconds,
			})
		}
	}
	sort.Slice(summary, func(i, j int) bool { return summary[i].ID < summary[j].ID })

	gcAges := make([]float64, len(gcEvents))
	for i, t := range gcEvents {
		gcAges[i] = frameTime.Sub(t).Seconds()
	}

	return frameJSON{
		Type:              "frame",
		WindowStart:       view.windowStart,
		WindowCount:       windowCount,
		Rows:              rows,
		Summary:           summary,
		SlotCap:           slotCap,
		FadeWindowSeconds: fadeWindow.Seconds(),
		Metrics:           metrics,
		GCEvents:          gcAges,
		GCHistorySeconds:  gcHistoryWindow.Seconds(),
	}
}
