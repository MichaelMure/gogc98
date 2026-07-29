// state.anchor tracks wherever the view currently is (server-chosen
// until the user scrolls, then whatever they scrolled to).
// state.userScrolled becomes true on the first scroll -- from then on
// the server never moves the view on its own, and a reconnect resends
// the anchor instead of letting the server redo its one-time initial
// jump over the user's scroll position. state.frozen just mirrors
// what was last sent to the server -- freezing itself (holding the
// heap data still while still letting the window scroll) happens
// server-side, see viewMode in frame.go.
const state = { anchor: 0, userScrolled: false, frozen: false };

// A span counts as "recently active" if something touched it within
// this many seconds. Deliberately a direct wall-clock check on
// lastEventSeconds, not a threshold on the decayed `activity` score:
// that score is cumulative, so one burst can push it into the
// hundreds or thousands and it then takes many seconds to decay back
// down even with nothing further happening -- a threshold on it read
// as "active" for almost everything, almost permanently.
const MINIMAP_RECENT_SECONDS = 2;

// Minimum pixel width for the viewport marker (see render()) -- not
// used for minimap ticks, those are single canvas pixel columns now.
const MINIMAP_VIEWPORT_MIN_WIDTH = 2;

const minimapEl = document.getElementById('minimap');
const minimapCanvasEl = document.getElementById('minimapCanvas');
const minimapCtx = minimapCanvasEl.getContext('2d');
const minimapViewportEl = document.getElementById('minimapViewport');
const gcTimelineEl = document.getElementById('gcTimeline');
const gcTimelineCanvasEl = document.getElementById('gcTimelineCanvas');
const gcTimelineCtx = gcTimelineCanvasEl.getContext('2d');
const statusEl = document.getElementById('status');
const freezeBtn = document.getElementById('freezeBtn');
const learnBtn = document.getElementById('learnBtn');
const learnOverlay = document.getElementById('learnOverlay');
const closeLearnBtn = document.getElementById('closeLearnBtn');

learnBtn.onclick = () => { learnOverlay.hidden = false; };
closeLearnBtn.onclick = () => { learnOverlay.hidden = true; };
learnOverlay.onclick = (e) => {
  if (e.target === learnOverlay) learnOverlay.hidden = true;
};
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') learnOverlay.hidden = true;
  if (e.key === '?') learnOverlay.hidden = !learnOverlay.hidden;
  if (e.key === 'f' || e.key === 'F') toggleFreeze();
});

let ws;
function connect() {
  ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws');
  ws.onopen = () => {
    console.log('gogc98: connected');
    // Only resend state if the user actually took control already --
    // otherwise let the fresh server-side view do its own one-time
    // jump to the first populated page, same as any new connection.
    if (state.userScrolled || state.frozen) send({ anchor: state.anchor, freeze: state.frozen });
  };
  ws.onmessage = (ev) => render(JSON.parse(ev.data));
  ws.onclose = () => {
    console.log('gogc98: disconnected, reconnecting in 1s');
    statusEl.textContent = 'disconnected — retrying...';
    setTimeout(connect, 1000);
  };
  ws.onerror = () => ws.close();
}
connect();

function send(msg) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
}

function toggleFreeze() {
  state.frozen = !state.frozen;
  freezeBtn.innerHTML = state.frozen ? 'Un<u>f</u>reeze' : '<u>F</u>reeze';
  send({ anchor: state.anchor, freeze: state.frozen });
}
freezeBtn.onclick = toggleFreeze;

// Mouse-wheel panning over the view. preventDefault + non-passive so
// this replaces the browser's native scroll instead of fighting it.
// Still works while frozen -- the server keeps navigating a frozen
// snapshot of the heap rather than blocking movement entirely.
document.getElementById('pagesContainer').addEventListener(
  'wheel',
  (e) => {
    e.preventDefault();
    state.userScrolled = true;
    const step = Math.sign(e.deltaY) * Math.max(1, Math.round(Math.abs(e.deltaY) / 20));
    state.anchor = Math.max(0, state.anchor + step);
    send({ anchor: state.anchor, freeze: state.frozen });
  },
  { passive: false }
);

// Hash a type id to a stable-within-a-snapshot hue. Type ids are only
// scoped to one trace generation, so long-lived objects' colors can
// drift between polls -- acceptable for now, see project notes.
function typeColor(typeId) {
  // Gray is already "nothing here" elsewhere (empty slots, void rows)
  // -- untyped objects are still real occupied data, so give them a
  // distinct muted amber instead of a color from that same family.
  if (typeId === 0) return 'hsl(35, 55%, 45%)';
  let h = 2166136261 ^ typeId;
  h = Math.imul(h ^ (typeId >>> 16), 2246822519);
  h = Math.imul(h ^ (h >>> 13), 3266489917);
  h ^= h >>> 16;
  const hue = Math.abs(h) % 360;
  return `hsl(${hue}, 65%, 55%)`;
}

// Small size classes read blue, large ones read red (log2 scale, 8B
// to 32KB -- Go's actual size-class range), so the size annotation's
// color alone hints at scale even before reading the number.
function sizeColor(elemSize) {
  if (!elemSize) return LABEL_DIM;
  const lo = 3; // log2(8)
  const hi = 15; // log2(32768), Go's largest size class
  const t = Math.min(1, Math.max(0, (Math.log2(elemSize) - lo) / (hi - lo)));
  return `hsl(${200 - t * 200}, 70%, 55%)`;
}

function borderColor(obj) {
  if (obj.status === 'freed') {
    const alpha = Math.max(0, 1 - obj.age / fadeWindowSeconds);
    return `rgba(255, 60, 60, ${alpha})`;
  }
  if (obj.age < 0.3) {
    return `rgba(80, 255, 120, ${1 - obj.age / 0.3})`;
  }
  return 'rgba(255,255,255,0.15)';
}

// Fixed pixel height per row. Box *width* is proportional to byte
// size instead (see render()) -- fixed-width cells made every object
// look the same size regardless of whether it was 8 bytes or 16KB.
const CELL = 8;

// serverSlotCap and fadeWindowSeconds are read from each frame
// (bridge/frame.go's slotCap/fadeWindow) rather than hardcoded here,
// so the two sides can't silently drift out of sync. These are just
// fallback values until the first frame arrives.
//
// serverSlotCap matters because the server never sends objects past
// that index -- treating indices beyond it as "empty" would be wrong,
// not just imprecise, since they might be occupied and we just don't
// have the data. Clamping to it stops a wide window from silently
// misreporting unsent slots as free.
let serverSlotCap = 256;
let fadeWindowSeconds = 1.5;

// Width reserved on the left of each row for its page-id label, a
// stand-in "address" (we only ever have the relative page index, see
// project notes on why we don't reconstruct real addresses). Wide
// enough for an id plus two annotation segments, e.g. "id 32768B +1024".
const LABEL_WIDTH = 108;

const pagesContainer = document.getElementById('pagesContainer');
const heapCanvas = document.getElementById('heapCanvas');
const heapCtx = heapCanvas.getContext('2d');

// Label id color: green means "there's a real page here", dim gray
// means "reclaimed or never allocated" -- so at a glance, presence of
// any data at all is a single, consistent color regardless of what
// kind of row it is.
const LABEL_ACTIVE = '#4a4';
const LABEL_DIM = '#556';
const LABEL_WARN = '#e80';
const LABEL_CONTINUATION = '#88a';

// Draws "id" left-aligned so ids form a stable, scannable column
// (their position no longer depends on the length of any annotation
// text next to them), and zero or more annotation segments (size,
// truncation count, continuation marker) packed right-to-left against
// the box grid boundary, each in its own de-emphasized color, so they
// read as detail attached to the boxes rather than part of the id
// itself. Segments closest to the boxes are listed last.
function drawLabel(id, idColor, y, ...segments) {
  heapCtx.textAlign = 'left';
  heapCtx.fillStyle = idColor;
  heapCtx.fillText(String(id), 4, y + CELL / 2);

  heapCtx.textAlign = 'right';
  let x = LABEL_WIDTH - 6;
  for (let i = segments.length - 1; i >= 0; i--) {
    const seg = segments[i];
    if (!seg || !seg.text) continue;
    heapCtx.fillStyle = seg.color;
    heapCtx.fillText(seg.text, x, y + CELL / 2);
    x -= heapCtx.measureText(seg.text).width + 6;
  }
}

// One row per page -- a real single line, hex-dump style -- rather
// than wrapping a page's objects into a square block. A page's row
// position depends only on its id and the container width, never on
// which other pages happen to be tracked, so it stays stable frame to
// frame.
function render(frame) {
  // Until the user scrolls, keep the local anchor synced to wherever
  // the server has placed the view (its one-time initial jump), so
  // the first actual scroll continues from the current view instead
  // of jumping from a stale anchor.
  if (!state.userScrolled) state.anchor = frame.windowStart;
  serverSlotCap = frame.slotCap || serverSlotCap;
  fadeWindowSeconds = frame.fadeWindowSeconds || fadeWindowSeconds;

  const cellsPerRow = Math.max(1, Math.floor((pagesContainer.clientWidth - LABEL_WIDTH) / CELL));
  const rowWidth = cellsPerRow * CELL;

  heapCanvas.width = LABEL_WIDTH + rowWidth;
  heapCanvas.height = frame.windowCount * CELL;

  heapCtx.fillStyle = '#000';
  heapCtx.fillRect(0, 0, heapCanvas.width, heapCanvas.height);
  heapCtx.font = '8px monospace';
  heapCtx.textBaseline = 'middle';

  // Rows are self-describing now (kind: "page" | "void" | "gap"),
  // sent in top-to-bottom order -- unlike the old windowStart+row
  // scheme, one row doesn't always correspond to one page id, since a
  // long run of untracked ids collapses into a single "gap" row (see
  // buildRows in frame.go). So this just draws frame.rows in order,
  // it doesn't compute ids itself.
  for (let i = 0; i < frame.rows.length; i++) {
    const row = frame.rows[i];
    const y = i * CELL;

    if (row.kind === 'void') {
      // Untracked, but not long enough a run to collapse: dim, not
      // green -- a void row must not look like it holds a real page,
      // only its id is known.
      drawLabel(row.id, LABEL_DIM, y);
      heapCtx.fillStyle = '#0a0a0a';
      heapCtx.fillRect(LABEL_WIDTH, y, rowWidth, CELL);
      continue;
    }

    if (row.kind === 'gap') {
      // Collapsed into one row because it's huge -- see
      // gapCollapseThreshold in frame.go for why only gaps at this
      // scale get collapsed. Ordinary unused-but-reserved space (even
      // thousands of pages) renders as plain blank rows instead, since
      // that's real, scrollable information about the heap's layout,
      // not noise.
      drawLabel(row.id, LABEL_DIM, y);
      heapCtx.fillStyle = '#0a0a0a';
      heapCtx.fillRect(LABEL_WIDTH, y, rowWidth, CELL);
      heapCtx.textAlign = 'center';
      heapCtx.fillStyle = LABEL_DIM;
      heapCtx.fillText(`⋯ ${row.gapLen.toLocaleString()} pages unused ⋯`, LABEL_WIDTH + rowWidth / 2, y + CELL / 2);
      continue;
    }

    if (row.kind === 'end') {
      // Nothing tracked anywhere at or beyond this id -- the server
      // stops the frame here rather than padding out fake rows, so
      // this is always the last row actually drawn.
      drawLabel(row.id, LABEL_DIM, y);
      heapCtx.fillStyle = '#0a0a0a';
      heapCtx.fillRect(LABEL_WIDTH, y, rowWidth, CELL);
      heapCtx.textAlign = 'center';
      heapCtx.fillStyle = LABEL_DIM;
      heapCtx.fillText('⋯ nothing tracked beyond here ⋯', LABEL_WIDTH + rowWidth / 2, y + CELL / 2);
      break;
    }

    const nelems = row.nelems || 0;
    const shown = Math.min(nelems, serverSlotCap);
    const truncated = nelems > shown;
    // elemSize is 0 when this span's own Span/SpanAlloc event hasn't
    // arrived yet (its size class genuinely isn't known yet, not just
    // zero) -- most often because we first heard about it from one of
    // its objects instead. Say so plainly rather than showing a
    // meaningless "0B".
    const sizeSeg = row.elemSize
      ? { text: `${row.elemSize}B`, color: sizeColor(row.elemSize) }
      : { text: '?B', color: LABEL_DIM };

    // The size is shown by default, so a row's kind of page is legible
    // regardless of what else is going on. Truncation is flagged
    // alongside it rather than replacing it -- a page could otherwise
    // be hiding hundreds of untracked objects (small size classes go
    // up to 1024 per page), and losing the size while flagging that
    // would be a step backwards. A continuation row shows its place in
    // the span instead -- the size class is already established by
    // that same span's base row, just above.
    if (row.pageOffset > 0) {
      drawLabel(row.id, LABEL_ACTIVE, y, { text: `${row.pageOffset + 1}/${row.pageCount}`, color: LABEL_CONTINUATION });
    } else if (truncated) {
      drawLabel(row.id, LABEL_ACTIVE, y, sizeSeg, { text: `+${nelems - shown}`, color: LABEL_WARN });
    } else {
      drawLabel(row.id, LABEL_ACTIVE, y, sizeSeg);
    }

    // Every row -- base or continuation -- draws its own fragments,
    // each already sized (xFrac/widthFrac) to its true share of this
    // page's bytes by the server. An object no bigger than one page
    // gets one fragment sized to elemSize/pageSize; an object bigger
    // than one page gets a fragment per page it touches, so it reads
    // as one solid box spanning every row it occupies, at the same
    // full opacity and color as any other box -- no separate
    // "continuation" fill style needed.
    for (const frag of row.objects || []) {
      const bx = LABEL_WIDTH + frag.xFrac * rowWidth;
      const bw = Math.max(1, frag.widthFrac * rowWidth);
      if (!frag.status || frag.status === 'freed') {
        // Freed/never-observed slots read as holes immediately (empty
        // fill), not as still-occupied boxes -- only the fading red
        // outline marks "recently freed" vs. "never observed".
        heapCtx.fillStyle = '#181818';
        heapCtx.fillRect(bx + 0.5, y + 0.5, bw - 1, CELL - 1);
        if (frag.status === 'freed') {
          heapCtx.strokeStyle = borderColor(frag);
          heapCtx.strokeRect(bx + 0.5, y + 0.5, bw - 1, CELL - 1);
        }
        continue;
      }
      heapCtx.fillStyle = typeColor(frag.typeId);
      heapCtx.fillRect(bx + 1, y + 1, bw - 2, CELL - 2);
      heapCtx.strokeStyle = borderColor(frag);
      heapCtx.strokeRect(bx + 0.5, y + 0.5, bw - 1, CELL - 1);
    }
  }

  // Position, not count, is what a map should encode: each tick sits
  // at a fixed spot derived from its own real page id (min..max of
  // currently tracked ids, same idea as everywhere else in this tool
  // -- position always reflects real address, never "index among
  // however many things happen to exist right now"). A span appearing
  // or disappearing elsewhere then just adds or removes one mark at
  // its own spot; it can't resize or re-space anything else, which is
  // what tiling N equal-width ticks edge to edge was doing before --
  // that's why the strip kept visibly "growing" and "shrinking" no
  // matter how the count feeding it was smoothed.
  // minId is always 0, the true start of the id space -- not the
  // first *tracked* id. The main page view can scroll all the way
  // down to real id 0 (as void/gap rows before anything tracked
  // starts), so the minimap needs the same reference point or "the
  // beginning" in one view isn't the same position as in the other,
  // which is what made every previous version of this wrong: they all
  // anchored the strip's left edge to the first tracked span instead,
  // so id 0 wasn't representable in the minimap's own coordinate
  // system at all. maxId comes only from the tracked range (stable,
  // barely changes -- see the earlier trackedSpans-vs-idSpan
  // measurements), falling back to the current window's end only when
  // nothing is tracked yet at all, just to avoid a zero-width domain
  // -- not extended for scroll position the way an earlier version
  // tried, since that reintroduces the same shifting-domain problem
  // on the far end. Since windowStart can never go below 0 either
  // (the wheel handler already clamps that), this whole domain --
  // and therefore every tick's position -- never needs to shift for
  // scrolling to stay representable. Scrolling past the last tracked
  // page just pins the viewport marker at the right edge instead (see
  // below).
  const lastRow = frame.rows[frame.rows.length - 1];
  let windowEndId = frame.windowStart;
  if (lastRow) {
    windowEndId = lastRow.kind === 'gap' ? lastRow.id + lastRow.gapLen : lastRow.id + 1;
  }
  const ids = frame.summary.map((s) => s.id);
  const minId = 0;
  const maxId = ids.length ? Math.max(...ids) : windowEndId;
  const idSpan = Math.max(1, maxId - minId);
  const stripWidth = minimapEl.clientWidth;
  const stripHeight = minimapEl.clientHeight;
  const idToX = (id) => ((id - minId) / idSpan) * (stripWidth - 1);

  // Drawn on canvas, one pixel column at a time, rather than one DOM
  // element per tracked page positioned by left px: with a tracked id
  // range easily in the thousands spread across a strip a fraction of
  // that many pixels wide, most ids map to the very same pixel, and
  // separate absolutely-positioned elements would just silently stack
  // -- only the last one drawn (highest id) stays visible, hiding
  // everything else at that spot, most often a span's own
  // continuation pages since they sit at adjacent ids right next to
  // whatever's drawn after them. Bucketing by pixel column instead
  // means every tracked page actually contributes to what's shown,
  // none of them silently lost to draw order.
  // Each summary entry is exactly one page wide in id-space, so its
  // real extent on the strip is [idToX(id), idToX(id+1)) -- filling
  // that whole range, not just a single rounded point, matters
  // whenever the tracked range is narrow enough that one id covers
  // more than a pixel: otherwise consecutive real ids can round to
  // non-adjacent pixels, leaving a 1px black gap between pages that
  // are genuinely contiguous (no gap at all in the main view).
  const pxWidth = Math.max(1, Math.round(stripWidth));
  const bucketFull = new Float64Array(pxWidth);
  const bucketActive = new Uint8Array(pxWidth);
  for (const s of frame.summary) {
    const x0 = Math.min(pxWidth - 1, Math.max(0, Math.floor(idToX(s.id))));
    const x1 = Math.max(x0, Math.min(pxWidth - 1, Math.ceil(idToX(s.id + 1)) - 1));
    const full = Math.min(1, s.fullness);
    const active = s.lastEventSeconds < MINIMAP_RECENT_SECONDS;
    for (let x = x0; x <= x1; x++) {
      bucketFull[x] = Math.max(bucketFull[x], full);
      if (active) bucketActive[x] = 1;
    }
  }
  minimapCanvasEl.width = pxWidth;
  minimapCanvasEl.height = Math.max(1, Math.round(stripHeight));
  minimapCtx.fillStyle = '#000';
  minimapCtx.fillRect(0, 0, minimapCanvasEl.width, minimapCanvasEl.height);
  // One scalar (fullness) drives brightness; whether anything in that
  // column changed recently just picks the hue -- green if so, red if
  // not -- rather than trying to blend two independent magnitudes
  // into one color, which made "how full" and "how active" hard to
  // read apart from each other (and, normalized against a global max,
  // made ordinary real activity nearly invisible next to rare bursts).
  // Nothing there at all (0 fullness, never active) fades to black,
  // which is also the background.
  for (let x = 0; x < pxWidth; x++) {
    if (bucketFull[x] === 0) continue;
    const v = Math.round(bucketFull[x] * 255);
    minimapCtx.fillStyle = bucketActive[x] ? `rgb(0,${v},0)` : `rgb(${v},0,0)`;
    minimapCtx.fillRect(x, 0, 1, minimapCanvasEl.height);
  }

  // The current viewport is its own thin indicator underneath the
  // strip, rather than recoloring whichever ticks happen to be in
  // range -- so it reads as "here's where you're looking" without
  // competing with, or being confused for, a tick's own fullness/
  // activity color. Width comes from the real id span of the current
  // view relative to the tracked range, computed before any clamping
  // so it stays at its true size; only position gets clamped into the
  // strip afterward, same as a normal scrollbar thumb -- scrolling
  // past either end of the tracked range slides it flush against that
  // edge and leaves it there, it doesn't keep moving (there's nowhere
  // meaningful left for it to go without redefining what the strip
  // represents, which is exactly what caused the zoom-while-scrolling
  // bug above).
  const vpWidth = Math.min(stripWidth, Math.max(MINIMAP_VIEWPORT_MIN_WIDTH, ((windowEndId - frame.windowStart) / idSpan) * stripWidth));
  const vpLeft = Math.min(stripWidth - vpWidth, Math.max(0, idToX(frame.windowStart)));
  minimapViewportEl.style.left = `${vpLeft}px`;
  minimapViewportEl.style.width = `${vpWidth}px`;

  // GC cycle timeline: a separate strip under the minimap, but on a
  // different axis -- time, not address space, right edge is "now".
  // Ages arrive pre-computed from the server (frame.gcEvents, seconds
  // since each recent cycle started) rather than timestamps, the same
  // reasoning as every other age/recency field here: only relative
  // recency against the render cadence matters, and it avoids needing
  // clock sync between server and browser.
  const gcWidth = Math.max(1, Math.round(gcTimelineEl.clientWidth));
  const gcHeight = Math.max(1, Math.round(gcTimelineEl.clientHeight));
  gcTimelineCanvasEl.width = gcWidth;
  gcTimelineCanvasEl.height = gcHeight;
  gcTimelineCtx.fillStyle = '#000';
  gcTimelineCtx.fillRect(0, 0, gcWidth, gcHeight);
  const gcHistorySeconds = frame.gcHistorySeconds || 20;
  gcTimelineCtx.fillStyle = '#ff0';
  for (const age of frame.gcEvents || []) {
    if (age < 0 || age > gcHistorySeconds) continue;
    const x = Math.min(gcWidth - 1, Math.round((1 - age / gcHistorySeconds) * (gcWidth - 1)));
    gcTimelineCtx.fillRect(x, 0, 1, gcHeight);
  }

  const m = frame.metrics;
  statusEl.textContent =
    `events: ${m.totalEvents.toLocaleString()}  |  ` +
    `allocs/s: ${m.allocsPerSec.toFixed(0)}  |  ` +
    `frees/s: ${m.freesPerSec.toFixed(0)}  |  ` +
    `arenas: ${m.trackedArenaBlocks}  |  ` +
    `spans: ${m.trackedSpans}  |  ` +
    `pages: ${m.trackedPages.toLocaleString()}  |  ` +
    `${state.frozen ? 'frozen' : 'live'}`;
}
