const state = { auto: true, anchor: 0 };

// See the minimap brightness smoothing in render() for why this
// exists instead of just taking each frame's max directly.
let minimapMaxActivity = 1;
const MINIMAP_MAX_DECAY = Math.pow(0.5, 0.15 / 15); // half-life ~15s at the 150ms frame rate

const minimapEl = document.getElementById('minimap');
const statusEl = document.getElementById('status');
const autoBtn = document.getElementById('autoBtn');

let ws;
function connect() {
  ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws');
  ws.onopen = () => {
    console.log('gogc98: connected');
    // Every new connection starts a fresh server-side view defaulting
    // to auto-follow -- without this, any reconnect (a transient
    // network blip, the tab being backgrounded, anything) would
    // silently revert a manually-scrolled view back to auto-follow,
    // which repositions the window based on activity instead of
    // holding it where the user left it.
    send(state.auto ? { mode: 'auto' } : { mode: 'manual', anchor: state.anchor });
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

autoBtn.onclick = () => {
  state.auto = !state.auto;
  autoBtn.textContent = 'Auto-follow: ' + (state.auto ? 'ON' : 'OFF');
  send(state.auto ? { mode: 'auto' } : { mode: 'manual', anchor: state.anchor });
};

// Mouse-wheel panning over the view. preventDefault + non-passive so
// this replaces the browser's native scroll instead of fighting it.
document.getElementById('pagesContainer').addEventListener(
  'wheel',
  (e) => {
    e.preventDefault();
    state.auto = false;
    autoBtn.textContent = 'Auto-follow: OFF';
    const step = Math.sign(e.deltaY) * Math.max(1, Math.round(Math.abs(e.deltaY) / 20));
    state.anchor = Math.max(0, state.anchor + step);
    send({ mode: 'manual', anchor: state.anchor });
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

// Fixed pixel size per object cell -- the "always same scale" part.
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
// enough for "id +1024" or "id 32768B", the longest label text used.
const LABEL_WIDTH = 84;

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
// text next to them), and an optional secondary annotation (size,
// truncation count, continuation marker) right-aligned against the
// box grid boundary, in a de-emphasized color, so it reads as detail
// attached to the boxes rather than part of the id itself.
function drawLabel(id, annotation, idColor, annotationColor, y) {
  heapCtx.textAlign = 'left';
  heapCtx.fillStyle = idColor;
  heapCtx.fillText(String(id), 4, y + CELL / 2);

  if (annotation) {
    heapCtx.textAlign = 'right';
    heapCtx.fillStyle = annotationColor;
    heapCtx.fillText(annotation, LABEL_WIDTH - 6, y + CELL / 2);
  }
}

// One row per page -- a real single line, hex-dump style -- rather
// than wrapping a page's objects into a square block. A page's row
// position depends only on its id and the container width, never on
// which other pages happen to be tracked, so it stays stable frame to
// frame.
function render(frame) {
  // Keep the local anchor synced to wherever auto-follow currently is,
  // so switching to manual (button or wheel) continues from the
  // current view instead of jumping to a stale anchor.
  if (state.auto) state.anchor = frame.windowStart;
  serverSlotCap = frame.slotCap || serverSlotCap;
  fadeWindowSeconds = frame.fadeWindowSeconds || fadeWindowSeconds;

  const cellsPerRow = Math.max(1, Math.floor((pagesContainer.clientWidth - LABEL_WIDTH) / CELL));

  heapCanvas.width = LABEL_WIDTH + cellsPerRow * CELL;
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
      drawLabel(id, '', LABEL_DIM, LABEL_DIM, y);
      heapCtx.fillStyle = '#0a0a0a';
      heapCtx.fillRect(LABEL_WIDTH, y, cellsPerRow * CELL, CELL);
      continue;
    }

    if (page.pageOffset > 0) {
      // Continuation of a multi-page span (typically a large object
      // with its own dedicated span) -- physically part of the same
      // allocation as the base row above, not empty space. There's no
      // per-object grid to draw here (the object's own index grid
      // only makes sense on the base row), so just fill the row to
      // show it's occupied, at reduced opacity to read as "continues"
      // rather than a fresh object.
      drawLabel(id, `${page.pageOffset + 1}/${page.pageCount}`, LABEL_ACTIVE, LABEL_CONTINUATION, y);
      heapCtx.globalAlpha = 0.45;
      heapCtx.fillStyle = typeColor(page.repType);
      heapCtx.fillRect(LABEL_WIDTH, y + 1, cellsPerRow * CELL, CELL - 2);
      heapCtx.globalAlpha = 1;
      continue;
    }

    const nelems = page.nelems || 0;
    const shown = Math.min(nelems, cellsPerRow, serverSlotCap);
    const truncated = nelems > shown;

    // Flag truncation as a warning annotation rather than silently
    // clipping -- a row that looks "not full" could otherwise be
    // hiding hundreds of objects (small size classes go up to 1024
    // per page, easily more than fit in one row). Otherwise, show the
    // object size, de-emphasized -- a compact stand-in for "what kind
    // of page this is", since a page only ever holds one Go size
    // class, without competing visually with the id itself.
    if (truncated) {
      drawLabel(id, `+${nelems - shown}`, LABEL_ACTIVE, LABEL_WARN, y);
    } else {
      drawLabel(id, `${page.elemSize}B`, LABEL_ACTIVE, LABEL_DIM, y);
    }

    const byIdx = new Map((page.objects || []).map((o) => [o.idx, o]));
    for (let i = 0; i < shown; i++) {
      const cx = LABEL_WIDTH + i * CELL;
      const obj = byIdx.get(i);
      if (!obj || obj.status === 'freed') {
        // Freed slots read as holes immediately (empty fill), not as
        // still-occupied cells -- only the fading red outline marks
        // "recently freed" vs. "never allocated".
        heapCtx.fillStyle = '#181818';
        heapCtx.fillRect(cx + 0.5, y + 0.5, CELL - 1, CELL - 1);
        if (obj) {
          heapCtx.strokeStyle = borderColor(obj);
          heapCtx.strokeRect(cx + 0.5, y + 0.5, CELL - 1, CELL - 1);
        }
        continue;
      }
      heapCtx.fillStyle = typeColor(obj.typeId);
      heapCtx.fillRect(cx + 1, y + 1, CELL - 2, CELL - 2);
      heapCtx.strokeStyle = borderColor(obj);
      heapCtx.strokeRect(cx + 0.5, y + 0.5, CELL - 1, CELL - 1);
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
    `mode: ${frame.auto ? 'auto-follow' : 'manual'}`;
}
