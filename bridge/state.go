package main

import (
	"math"
	"sync"
	"time"

	"golang.org/x/exp/trace"
)

// Constants mirrored from internal/runtime/gc — these are stable,
// long-unchanged Go implementation details, not something we can
// import from outside the standard library.
const (
	pageSize     = 8192
	minHeapAlign = 8
)

var sizeClassToSize = [...]uint32{
	0, 8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224,
	240, 256, 288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896,
	1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456,
	4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880,
	12288, 13568, 14336, 16384, 18432, 19072, 20480, 21760, 24576, 27264,
	28672, 32768,
}

type objStatus string

const (
	objAlive objStatus = "alive"
	objFreed objStatus = "freed"
)

// fadeWindow is how long a freed object stays in the model (drawn
// fading out) before being pruned entirely.
const fadeWindow = 1500 * time.Millisecond

// activityHalfLife controls how fast a span's activity score decays,
// which drives the minimap's per-span brightness (see app.js).
const activityHalfLife = 1.5 // seconds

type object struct {
	idx     uint32
	typeID  uint64
	status  objStatus
	changed time.Time
}

type span struct {
	id        uint64
	npages    uint64
	sizeClass uint8
	elemSize  uint32
	nelems    uint32
	objects   map[uint32]*object
	lastEvent time.Time
	activity  float64
}

func (s *span) touch(now time.Time) {
	if !s.lastEvent.IsZero() {
		dt := now.Sub(s.lastEvent).Seconds()
		if dt > 0 {
			s.activity *= math.Pow(0.5, dt/activityHalfLife)
		}
	}
	s.activity++
	s.lastEvent = now
}

// classFromKindclass decodes the size class out of a Span/SpanAlloc
// event's packed kindclass arg (see traceSpanTypeAndClass). The low
// bit of the intermediate spanclass value is a noscan flag, which
// nothing here currently uses, so it's simply not extracted.
func classFromKindclass(kc uint64) (sizeClass uint8) {
	if kc&1 == 1 {
		// Sentinel for "not in use" spans.
		return 0
	}
	return uint8(kc >> 2)
}

func elemSizeFor(sizeClass uint8, npages uint64) uint32 {
	if sizeClass == 0 || int(sizeClass) >= len(sizeClassToSize) {
		// Large object span: exactly one object filling the span.
		return uint32(npages * pageSize)
	}
	return sizeClassToSize[sizeClass]
}

type rateCounter struct {
	count     int64
	lastCount int64
	lastTime  time.Time
	perSec    float64
}

func (r *rateCounter) sample(now time.Time) {
	if !r.lastTime.IsZero() {
		dt := now.Sub(r.lastTime).Seconds()
		if dt > 0 {
			r.perSec = float64(r.count-r.lastCount) / dt
		}
	}
	r.lastCount = r.count
	r.lastTime = now
}

type gcState struct {
	mu sync.Mutex

	spans map[uint64]*span

	// pageOwner maps every page id covered by a tracked span (its base
	// id plus any continuation pages for multi-page spans) back to
	// that span's base id. It exists to detect when a span we still
	// think is live has actually had its address space reused --
	// e.g. after a SpanFree we never saw, perhaps because the flight
	// recorder's buffer overflowed under heavy event volume before we
	// polled it. Without this, a stale multi-page span can linger
	// forever and start overlapping whatever new span later reuses
	// that range, producing duplicate/conflicting rows.
	pageOwner map[uint64]uint64

	totalEvents int64
	spanAllocs  int64
	spanFrees   int64

	allocRate rateCounter
	freeRate  rateCounter

	lastEventTime trace.Time
}

func newGCState() *gcState {
	return &gcState{
		spans:     make(map[uint64]*span),
		pageOwner: make(map[uint64]uint64),
	}
}

// firstSpanID returns the lowest page id currently tracked, if any --
// used for the one-time initial view placement in buildFrame.
func (g *gcState) firstSpanID() (uint64, bool) {
	first := uint64(0)
	found := false
	for id := range g.spans {
		if !found || id < first {
			first, found = id, true
		}
	}
	return first, found
}

func (g *gcState) spanFor(id uint64) *span {
	sp, ok := g.spans[id]
	if !ok {
		sp = &span{id: id, objects: make(map[uint32]*object)}
		g.spans[id] = sp
		// Claim at least this span's own page immediately, even before
		// we know its real npages (e.g. if it's discovered via an
		// object event with no prior Span/SpanAlloc seen). Without
		// this, such a span is invisible to pageOwner and can never be
		// evicted later even after proof its address space was reused,
		// leaving a permanent stale duplicate.
		g.claimPages(sp)
	}
	return sp
}

// claimPages registers sp as owning its full page range, evicting any
// other span that previously claimed part of that range -- real proof
// that span is gone, regardless of whether we saw its SpanFree.
func (g *gcState) claimPages(sp *span) {
	npages := sp.npages
	if npages == 0 {
		npages = 1
	}
	for id := sp.id; id < sp.id+npages; id++ {
		if owner, ok := g.pageOwner[id]; ok && owner != sp.id {
			g.evictSpan(owner)
		}
		g.pageOwner[id] = sp.id
	}
}

// evictSpan removes a span and its page-ownership claims from the
// model, whether triggered by an explicit SpanFree or by claimPages
// discovering its address space was reused.
func (g *gcState) evictSpan(id uint64) {
	sp, ok := g.spans[id]
	if !ok {
		return
	}
	npages := sp.npages
	if npages == 0 {
		npages = 1
	}
	for pid := sp.id; pid < sp.id+npages; pid++ {
		if g.pageOwner[pid] == id {
			delete(g.pageOwner, pid)
		}
	}
	delete(g.spans, id)
}

// objectLocation resolves an object id to its span and index within
// that span. known is false when the span's geometry (elemSize) isn't
// established yet -- e.g. an object event arrived before we ever saw
// that span's own Span/SpanAlloc event. Callers must not record
// per-object data in that case: without a real elemSize, idx can't be
// computed and would default to 0, silently aliasing every object on
// that span onto the same slot.
func (g *gcState) objectLocation(objID uint64) (sp *span, idx uint32, known bool) {
	addrRel := objID * minHeapAlign
	spanID := addrRel / pageSize
	sp = g.spanFor(spanID)
	if sp.elemSize == 0 {
		return sp, 0, false
	}
	return sp, uint32((addrRel - spanID*pageSize) / uint64(sp.elemSize)), true
}

// applyEvent updates state from a single decoded experimental trace
// event. now is used for activity decay and fade animation, not the
// event's own trace-clock timestamp, since we only care about
// relative recency for rendering.
func (g *gcState) applyEvent(now time.Time, exp trace.ExperimentalEvent) {
	arg := func(i int) uint64 { return exp.ArgValue(i).Uint64() }
	g.totalEvents++

	switch exp.Name {
	case "Span", "SpanAlloc":
		id, npages, kindclass := arg(0), arg(1), arg(2)
		sc := classFromKindclass(kindclass)
		sp := g.spanFor(id)
		sp.npages, sp.sizeClass = npages, sc
		sp.elemSize = elemSizeFor(sc, npages)
		if sp.elemSize > 0 {
			sp.nelems = uint32(npages*pageSize) / sp.elemSize
		}
		sp.touch(now)
		g.claimPages(sp)
		if exp.Name == "SpanAlloc" {
			g.spanAllocs++
		}

	case "SpanFree":
		g.evictSpan(arg(0))
		g.spanFrees++

	case "HeapObject", "HeapObjectAlloc":
		sp, idx, known := g.objectLocation(arg(0))
		if known {
			obj, ok := sp.objects[idx]
			if !ok {
				obj = &object{idx: idx}
				sp.objects[idx] = obj
			}
			obj.typeID = arg(1)
			obj.status = objAlive
			obj.changed = now
		}
		sp.touch(now)
		if exp.Name == "HeapObjectAlloc" {
			g.allocRate.count++
		}

	case "HeapObjectFree":
		sp, idx, known := g.objectLocation(arg(0))
		if known {
			if obj, ok := sp.objects[idx]; ok {
				obj.status = objFreed
				obj.changed = now
			}
		}
		sp.touch(now)
		g.freeRate.count++
	}
}

// prune drops objects that finished fading. Spans are deliberately
// *not* evicted on inactivity here -- a span holding long-lived
// survivors can go quiet for a long time without being freed, and
// treating "no recent events" as "gone" made real, still-allocated
// memory render as an empty page. Spans only leave the model on an
// actual SpanFree event (see applyEvent).
func (g *gcState) prune(now time.Time) {
	for _, sp := range g.spans {
		for idx, obj := range sp.objects {
			if obj.status == objFreed && now.Sub(obj.changed) > fadeWindow {
				delete(sp.objects, idx)
			}
		}
	}
}
