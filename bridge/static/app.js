// state.anchor tracks wherever the view currently is (server-chosen
// until the user scrolls, then whatever they scrolled to).
// state.userScrolled becomes true on the first scroll -- from then on
// the server never moves the view on its own, and a reconnect resends
// the anchor instead of letting the server redo its one-time initial
// jump over the user's scroll position.
const state = { anchor: 0, userScrolled: false, frozen: false };

// See the minimap brightness smoothing in render() for why this
// exists instead of just taking each frame's max directly.
let minimapMaxActivity = 1;
const MINIMAP_MAX_DECAY = Math.pow(0.5, 0.15 / 15); // half-life ~15s at the 150ms frame rate

const minimapEl = document.getElementById('minimap');
const statusEl = document.getElementById('status');
const freezeBtn = document.getElementById('freezeBtn');

let ws;
function connect() {
  ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws');
  ws.onopen = () => {
    console.log('gogc98: connected');
    // Only resend an anchor if the user actually took control already
    // -- otherwise let the fresh server-side view do its own one-time
    // jump to the first populated page, same as any new connection.
    if (state.userScrolled) send({ anchor: state.anchor });
  };
  ws.onmessage = (ev) => {
    if (!state.frozen) render(JSON.parse(ev.data));
  };
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

freezeBtn.onclick = () => {
  state.frozen = !state.frozen;
  freezeBtn.textContent = state.frozen ? 'Unfreeze' : 'Freeze';
};

// Mouse-wheel panning over the view. preventDefault + non-passive so
// this replaces the browser's native scroll instead of fighting it.
// Ignored while frozen, so unfreezing always resumes exactly where it
// left off instead of jumping to wherever a blind scroll landed.
document.getElementById('pagesContainer').addEventListener(
  'wheel',
  (e) => {
    e.preventDefault();
    if (state.frozen) return;
    state.userScrolled = true;
    const step = Math.sign(e.deltaY) * Math.max(1, Math.round(Math.abs(e.deltaY) / 20));
    state.anchor = Math.max(0, state.anchor + step);
    send({ anchor: state.anchor });
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

  const byId = new Map(frame.pages.map((p) => [p.id, p]));

  for (let row = 0; row < frame.windowCount; row++) {
    const id = frame.windowStart + row;
    const y = row * CELL;

    const page = byId.get(id);
    if (!page) {
      // Reclaimed/never-allocated: dim, not green -- a void row must
      // not look like it holds a real page, only its id is known.
      drawLabel(id, LABEL_DIM, y);
      heapCtx.fillStyle = '#0a0a0a';
      heapCtx.fillRect(LABEL_WIDTH, y, rowWidth, CELL);
      continue;
    }

    const nelems = page.nelems || 0;
    const shown = Math.min(nelems, serverSlotCap);
    const truncated = nelems > shown;
    const sizeSeg = { text: `${page.elemSize}B`, color: sizeColor(page.elemSize) };

    // The size is always shown, so a row's kind of page is legible
    // regardless of whatever else is going on. Truncation is flagged
    // alongside it rather than replacing it -- a page could otherwise
    // be hiding hundreds of untracked objects (small size classes go
    // up to 1024 per page), and losing the size while flagging that
    // would be a step backwards. A continuation row additionally shows
    // its place in the span.
    if (truncated) {
      drawLabel(id, LABEL_ACTIVE, y, sizeSeg, { text: `+${nelems - shown}`, color: LABEL_WARN });
    } else if (page.pageOffset > 0) {
      drawLabel(id, LABEL_ACTIVE, y, sizeSeg, { text: `${page.pageOffset + 1}/${page.pageCount}`, color: LABEL_CONTINUATION });
    } else {
      drawLabel(id, LABEL_ACTIVE, y, sizeSeg);
    }

    // Every row -- base or continuation -- draws its own fragments,
    // each already sized (xFrac/widthFrac) to its true share of this
    // page's bytes by the server. An object no bigger than one page
    // gets one fragment sized to elemSize/pageSize; an object bigger
    // than one page gets a fragment per page it touches, so it reads
    // as one solid box spanning every row it occupies, at the same
    // full opacity and color as any other box -- no separate
    // "continuation" fill style needed.
    for (const frag of page.objects || []) {
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

  minimapEl.innerHTML = '';
  // Snap up immediately to a new high (so a genuinely more active
  // moment shows right away), but only decay back down slowly (~15s
  // half-life). Normalizing against the raw instantaneous max instead
  // made brightness swing on every frame, since it's rescaled against
  // whatever the single hottest span anywhere happens to be doing that
  // instant -- not because any given span's own activity changed.
  const currentMax = Math.max(1, ...frame.summary.map((s) => s.activity));
  minimapMaxActivity = Math.max(currentMax, minimapMaxActivity * MINIMAP_MAX_DECAY);

  const visibleIds = new Set(frame.pages.map((p) => p.id));
  for (const s of frame.summary) {
    const tick = document.createElement('div');
    tick.className = 'tick';
    const intensity = Math.min(1, s.activity / minimapMaxActivity);
    tick.style.background = visibleIds.has(s.id)
      ? '#ff0'
      : `rgba(0,120,0,${0.15 + intensity * 0.85})`;
    minimapEl.appendChild(tick);
  }

  const m = frame.metrics;
  statusEl.textContent =
    `events: ${m.totalEvents.toLocaleString()}  |  ` +
    `allocs/s: ${m.allocsPerSec.toFixed(0)}  |  ` +
    `frees/s: ${m.freesPerSec.toFixed(0)}  |  ` +
    `spans: ${m.trackedSpans}  |  ` +
    `${state.frozen ? 'frozen' : 'live'}`;
}
