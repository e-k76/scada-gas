"use strict";
/*
 * Engineering Studio: SVG single-line-diagram / P&ID editor.
 *
 * Design Mode : place symbols, draw orthogonal pipelines, bind SCADA tags.
 * Live Mode   : the same drawing driven by the /ws/telemetry feed.
 * Persistence : /api/v1/topology/{station_id} (save) and .../deploy (publish to the runtime).
 *
 * No build step and no third-party code: plain ES2020 + SVG.
 */
(() => {
  const SVGNS = "http://www.w3.org/2000/svg";
  const $ = (id) => document.getElementById(id);
  const params = new URLSearchParams(location.search);
  const URL_TOKEN = params.get("token") || "";
  const USER_KEY = "scada.user";
  // The sign-in of the operator dashboard (same tab) is reused: its session token is sent as bearer token.
  let SESSION = null;
  try { SESSION = JSON.parse(sessionStorage.getItem("scada.session") || "null"); } catch (e) { /* storage blocked */ }
  const TOKEN = (SESSION && SESSION.token) || URL_TOKEN;

  // ------------------------------------------------------------------ DOM helpers
  function h(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function sv(tag, attrs, parent) {
    const e = document.createElementNS(SVGNS, tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
  const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();

  const { SYM, DEFAULT_BANDS, rotv, rotBox, pipeWidth, labelFor } = Schematic;
  const PALETTE_ORDER = ["BALL_VALVE", "SLAM_SHUT_VALVE", "REGULATOR", "FILTER", "ODORIZER", "PRESSURE_TRANSMITTER",
    "TEMPERATURE_TRANSMITTER", "PRESSURE_GAUGE", "JUNCTION", "FLANGE"];
  const DIAMETERS = ["2-inch", "3-inch", "4-inch", "6-inch", "8-inch", "10-inch", "12-inch", "16-inch", "20-inch", "24-inch"];
  const TAG_RE = /^[A-Za-z0-9_.:\-]{1,128}$/;
  const ID_RE = /^[A-Za-z0-9_.\-]{1,64}$/;

  // ------------------------------------------------------------------ state
  let doc = null;          // current StationTopology
  let status = null;       // server-side status of the stored version
  let dirty = false;
  let mode = "design";
  let tool = { kind: "select" };   // select | place(type) | pipe
  let sel = null;          // { type: "node"|"pipe", id }
  let serverIssues = [];
  let view = { s: 1, x: 40, y: 40 };
  let draft = null;        // pipe being drawn
  let ghostPos = null;
  let lastDiameter = "8-inch";
  const undoStack = [], redoStack = [];

  const tagsById = new Map();      // tag_id -> tag json (from /api/v1/telemetry/current)
  const liveTags = new Map();      // live mode: tag_id -> tag json
  const liveAlarms = new Map();    // live mode: alarm_id -> alarm json
  let alarmByTag = new Map();      // tag_id -> { sev, unacked }
  let lastMsgAt = 0;
  let ws = null, wsRetry = 1000, wsTimer = null;

  // scene references
  const svg = $("canvas");
  let viewport, gridLayer, pipesLayer, nodesLayer, overlay, bgRect, borderRect, minorPat, majorPat, minorPath, majorPath;
  let nodeEls = new Map(), pipeEls = new Map();

  // ------------------------------------------------------------------ utilities
  function toast(msg, kind) {
    const t = $("toast");
    t.textContent = msg; t.className = kind || ""; t.style.display = "block";
    clearTimeout(toast.t); toast.t = setTimeout(() => { t.style.display = "none"; }, kind === "err" ? 6000 : 3500);
  }
  function operator() { return $("user").value.trim(); }
  function grid() { return doc ? doc.grid_bounds.grid_size : 20; }
  function snap(v) { const g = grid(); return Math.round(v / g) * g; }
  const nodeById = (id) => (doc ? doc.components.find((n) => n.id === id) : null);
  const pipeById = (id) => (doc ? doc.pipelines.find((p) => p.id === id) : null);

  function newId(prefix) {
    const used = new Set(doc.components.map((n) => n.id).concat(doc.pipelines.map((p) => p.id)));
    for (let i = 1; ; i++) {
      const id = `${prefix}_${String(i).padStart(2, "0")}`;
      if (!used.has(id)) return id;
    }
  }
  const termInfo = (node, name) => Schematic.termInfo(node, name);

  // ------------------------------------------------------------------ orthogonal routing
  const same = (a, b) => a.x === b.x && a.y === b.y;
  function simplify(pts) {
    const out = [];
    for (const p of pts) if (!out.length || !same(out[out.length - 1], p)) out.push({ x: p.x, y: p.y });
    for (let i = out.length - 2; i > 0; i--) {   // drop collinear middle points
      const a = out[i - 1], b = out[i], c = out[i + 1];
      if ((a.x === b.x && b.x === c.x) || (a.y === b.y && b.y === c.y)) out.splice(i, 1);
    }
    return out;
  }
  // Intermediate bend points between s and e given the outward directions at both ends.
  function route(s, sd, e, ed) {
    if (s.x === e.x || s.y === e.y) return [];
    const sH = sd.dx !== 0, eH = ed ? ed.dx !== 0 : null;
    if (eH === null) return sH ? [{ x: e.x, y: s.y }] : [{ x: s.x, y: e.y }];
    if (sH && eH) { const mx = snap((s.x + e.x) / 2); return [{ x: mx, y: s.y }, { x: mx, y: e.y }]; }
    if (!sH && !eH) { const my = snap((s.y + e.y) / 2); return [{ x: s.x, y: my }, { x: e.x, y: my }]; }
    return sH ? [{ x: e.x, y: s.y }] : [{ x: s.x, y: e.y }];
  }
  function autoRoutePipe(p) {
    const a = nodeById(p.from_node_id), b = nodeById(p.to_node_id);
    if (!a || !b) return;
    const s = termInfo(a, p.from_terminal), e = termInfo(b, p.to_terminal);
    if (!s || !e) return;
    const full = simplify([s, ...route(s, s, e, e), e]);
    p.path_points = full.slice(1, -1);
  }
  function rerouteAttached(nodeId) {
    for (const p of doc.pipelines) if (p.from_node_id === nodeId || p.to_node_id === nodeId) autoRoutePipe(p);
  }
  const pipePoints = (p) => Schematic.pipePoints(doc, p);
  const usedTerminals = () => {
    const set = new Set();
    for (const p of doc.pipelines) { set.add(p.from_node_id + "/" + p.from_terminal); set.add(p.to_node_id + "/" + p.to_terminal); }
    return set;
  };

  // ------------------------------------------------------------------ scene construction
  function initScene() {
    svg.textContent = "";
    const defs = sv("defs", {}, svg);
    minorPat = sv("pattern", { id: "gpMinor", patternUnits: "userSpaceOnUse", width: 20, height: 20 }, defs);
    minorPath = sv("path", { d: "M 20 0 L 0 0 0 20", fill: "none", class: "gridline" }, minorPat);
    majorPat = sv("pattern", { id: "gpMajor", patternUnits: "userSpaceOnUse", width: 100, height: 100 }, defs);
    majorPath = sv("path", { d: "M 100 0 L 0 0 0 100", fill: "none", class: "gridmajor" }, majorPat);
    viewport = sv("g", { id: "viewport" }, svg);
    gridLayer = sv("g", {}, viewport);
    bgRect = sv("rect", { x: 0, y: 0, width: 1200, height: 600, fill: "url(#gpMinor)" }, gridLayer);
    sv("rect", { x: 0, y: 0, width: 1200, height: 600, fill: "url(#gpMajor)", class: "major" }, gridLayer);
    borderRect = sv("rect", { x: 0, y: 0, width: 1200, height: 600, class: "canvas-border" }, gridLayer);
    pipesLayer = sv("g", { id: "pipes" }, viewport);
    nodesLayer = sv("g", { id: "nodes" }, viewport);
    overlay = sv("g", { id: "overlay" }, viewport);
    applyView();
  }
  function applyView() {
    viewport.setAttribute("transform", `translate(${view.x} ${view.y}) scale(${view.s})`);
    $("zoomLabel").textContent = Math.round(view.s * 100) + "%";
  }
  function updateGrid() {
    const g = grid(), w = doc ? doc.grid_bounds.width : 1200, hh = doc ? doc.grid_bounds.height : 600;
    minorPat.setAttribute("width", g); minorPat.setAttribute("height", g);
    minorPath.setAttribute("d", `M ${g} 0 L 0 0 0 ${g}`);
    majorPat.setAttribute("width", g * 5); majorPat.setAttribute("height", g * 5);
    majorPath.setAttribute("d", `M ${g * 5} 0 L 0 0 0 ${g * 5}`);
    for (const r of gridLayer.querySelectorAll("rect")) { r.setAttribute("width", w); r.setAttribute("height", hh); }
    gridLayer.style.display = doc ? "" : "none";
  }

  let renderQueued = false;
  function requestRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => { renderQueued = false; renderScene(); });
  }

  function renderScene() {
    nodeEls = new Map(); pipeEls = new Map();
    pipesLayer.textContent = ""; nodesLayer.textContent = "";
    updateGrid();
    $("emptyHint").classList.toggle("show", !doc);
    if (doc) {
      for (const p of doc.pipelines) drawPipe(p);
      for (const n of doc.components) drawNode(n);
    }
    drawOverlay();
    applyLive();
    updateChrome();
  }

  function drawPipe(p) {
    const r = Schematic.drawPipe(pipesLayer, doc, p, !!(sel && sel.type === "pipe" && sel.id === p.id));
    if (r) pipeEls.set(p.id, r);
  }
  function drawNode(n) {
    nodeEls.set(n.id, Schematic.drawNode(nodesLayer, n, !!(sel && sel.type === "node" && sel.id === n.id)));
  }

  function drawOverlay() {
    overlay.textContent = "";
    if (!doc || mode !== "design") return;
    if (tool.kind === "pipe") {
      const used = usedTerminals();
      for (const n of doc.components) {
        const def = SYM[n.type]; if (!def) continue;
        for (const name of Object.keys(def.term)) {
          const t = termInfo(n, name);
          const isUsed = used.has(n.id + "/" + name);
          const isStart = draft && draft.node === n.id && draft.term === name;
          sv("circle", { cx: t.x, cy: t.y, r: 6, class: "term" + (isUsed ? " used" : "") + (isStart ? " start" : "") }, overlay);
        }
      }
      if (draft) {
        const pts = draftPreview();
        sv("polyline", { class: "draft", points: pts.map((q) => `${q.x},${q.y}`).join(" ") }, overlay);
      }
    } else if (tool.kind === "place" && ghostPos) {
      const def = SYM[tool.type];
      const g = sv("g", { class: "node ghost", transform: `translate(${ghostPos.x},${ghostPos.y})` }, overlay);
      def.draw(sv("g", { class: "sym" }, g));
    }
  }

  // ------------------------------------------------------------------ live rendering
  function recomputeAlarmIndex() { alarmByTag = Schematic.alarmIndex(liveAlarms.values()); }
  function applyLive() { Schematic.applyLive(doc, nodeEls, pipeEls, liveTags, alarmByTag, mode === "live"); }

  // ------------------------------------------------------------------ WebSocket (live mode)
  function setConn(state, txt) { $("conn").className = "badge " + state; $("conntxt").textContent = txt; }
  function wsConnect() {
    clearTimeout(wsTimer);
    const url = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws/telemetry" + (TOKEN ? "?token=" + encodeURIComponent(TOKEN) : "");
    setConn("offline", "CONNECTING");
    const sock = new WebSocket(url);
    ws = sock;
    sock.onopen = () => { wsRetry = 1000; setConn("live", "LIVE"); };
    sock.onmessage = (ev) => {
      let m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      lastMsgAt = Date.now();
      if (m.type === "snapshot") {
        liveTags.clear(); liveAlarms.clear();
        m.tags.forEach((t) => liveTags.set(t.tag_id, t));
        m.alarms.forEach((a) => { if (a.active) liveAlarms.set(a.alarm_id, a); });
        if (m.error) toast("Core data incomplete: " + m.error, "err");
        recomputeAlarmIndex();
      } else if (m.type === "telemetry") {
        m.tags.forEach((t) => liveTags.set(t.tag_id, Object.assign({}, liveTags.get(t.tag_id), t)));
      } else if (m.type === "alarm") {
        if (m.alarm.active) liveAlarms.set(m.alarm.alarm_id, m.alarm); else liveAlarms.delete(m.alarm.alarm_id);
        recomputeAlarmIndex();
      }
      applyLive();
    };
    sock.onclose = () => {
      if (ws !== sock) return;            // replaced or intentionally closed
      setConn("offline", "OFFLINE · retry in " + Math.round(wsRetry / 1000) + "s");
      wsTimer = setTimeout(() => { if (mode === "live") wsConnect(); }, wsRetry);
      wsRetry = Math.min(wsRetry * 2, 10000);
    };
    sock.onerror = () => { try { sock.close(); } catch (e) { /* ignore */ } };
  }
  function wsClose() {
    clearTimeout(wsTimer);
    const s = ws; ws = null;
    if (s) { try { s.close(); } catch (e) { /* ignore */ } }
    liveTags.clear(); liveAlarms.clear(); recomputeAlarmIndex();
  }
  setInterval(() => {
    if (mode !== "live" || !ws || ws.readyState !== 1) return;
    const stale = Date.now() - lastMsgAt > 5000;
    setConn(stale ? "stale" : "live", stale ? "NO DATA" : "LIVE");
    if (sel) renderInspector();   // keeps the live values in the inspector current
  }, 1000);

  // ------------------------------------------------------------------ mode / tool
  function setMode(m) {
    if (m === mode) return;
    mode = m;
    document.body.dataset.mode = m;
    $("modeDesign").setAttribute("aria-pressed", String(m === "design"));
    $("modeLive").setAttribute("aria-pressed", String(m === "live"));
    $("conn").style.display = m === "live" ? "" : "none";
    svg.classList.toggle("live-scene", m === "live");
    draft = null; ghostPos = null; tool = { kind: "select" };
    if (m === "live") { lastMsgAt = Date.now(); wsConnect(); } else wsClose();
    syncTools(); renderInspector(); requestRender();
  }
  function setTool(t) {
    if (mode !== "design") return;
    tool = t; draft = null; ghostPos = null;
    syncTools(); requestRender();
  }
  function syncTools() {
    $("toolSelect").setAttribute("aria-pressed", String(tool.kind === "select"));
    $("toolPipe").setAttribute("aria-pressed", String(tool.kind === "pipe"));
    for (const b of document.querySelectorAll(".sym-btn")) b.setAttribute("aria-pressed", String(tool.kind === "place" && tool.type === b.dataset.type));
    svg.classList.toggle("tool-place", tool.kind === "place");
    svg.classList.toggle("tool-pipe", tool.kind === "pipe");
  }

  // ------------------------------------------------------------------ document mutation / undo
  function snapshot() { return JSON.stringify(doc); }
  function pushUndo() {
    undoStack.push(snapshot()); if (undoStack.length > 100) undoStack.shift();
    redoStack.length = 0;
  }
  function markDirty() { dirty = true; updateChrome(); }
  // Applies fn to the document. Changes that leave the drawing identical (for
  // example committing an unchanged field) create no undo entry and no dirty flag.
  function mutate(fn, opts) {
    const before = snapshot();
    fn();
    if (snapshot() === before) return;
    undoStack.push(before); if (undoStack.length > 100) undoStack.shift();
    redoStack.length = 0;
    markDirty();
    if (!opts || opts.inspector !== false) renderInspector();
    requestRender();
  }
  function undo() {
    if (!undoStack.length || mode !== "design") return;
    redoStack.push(snapshot()); doc = JSON.parse(undoStack.pop()); dirty = true; fixSelection(); renderInspector(); requestRender();
  }
  function redo() {
    if (!redoStack.length || mode !== "design") return;
    undoStack.push(snapshot()); doc = JSON.parse(redoStack.pop()); dirty = true; fixSelection(); renderInspector(); requestRender();
  }
  function fixSelection() {
    if (sel && !(sel.type === "node" ? nodeById(sel.id) : pipeById(sel.id))) sel = null;
  }
  function select(s) { sel = s; renderInspector(); requestRender(); }

  function addNode(type, x, y) {
    const def = SYM[type];
    const n = {
      id: newId(def.prefix), type, x: clamp(snap(x), 0, doc.grid_bounds.width), y: clamp(snap(y), 0, doc.grid_bounds.height), rotation: 0,
      bindings: {}, display: {},
    };
    if (def.kind === "instrument") { n.display.decimals = 1; if (def.unit) n.display.unit = def.unit; }
    mutate(() => { doc.components.push(n); }, { inspector: false });
    select({ type: "node", id: n.id });
    return n;
  }
  function deleteSelection() {
    if (!sel || !doc || mode !== "design") return;
    const s = sel;
    mutate(() => {
      if (s.type === "node") {
        doc.components = doc.components.filter((n) => n.id !== s.id);
        doc.pipelines = doc.pipelines.filter((p) => p.from_node_id !== s.id && p.to_node_id !== s.id);
      } else doc.pipelines = doc.pipelines.filter((p) => p.id !== s.id);
      sel = null;
    });
  }
  function rotateSelection() {
    if (!sel || sel.type !== "node" || mode !== "design") return;
    mutate(() => { const n = nodeById(sel.id); n.rotation = (n.rotation + 90) % 360; rerouteAttached(n.id); });
  }
  function renameNode(n, newName) {
    newName = newName.trim();
    if (newName === n.id) return true;
    const clash = doc.components.some((x) => x.id === newName) || doc.pipelines.some((x) => x.id === newName);
    if (!ID_RE.test(newName) || clash) { toast(clash ? `The id "${newName}" is already used` : "Ids may contain letters, digits, _ . - (max 64)", "err"); return false; }
    const old = n.id;
    mutate(() => {
      n.id = newName;
      for (const p of doc.pipelines) { if (p.from_node_id === old) p.from_node_id = newName; if (p.to_node_id === old) p.to_node_id = newName; }
      if (sel && sel.type === "node" && sel.id === old) sel = { type: "node", id: newName };
    });
    return true;
  }
  function renamePipe(p, newName) {
    newName = newName.trim();
    if (newName === p.id) return true;
    const clash = doc.components.some((x) => x.id === newName) || doc.pipelines.some((x) => x.id === newName);
    if (!ID_RE.test(newName) || clash) { toast(clash ? `The id "${newName}" is already used` : "Ids may contain letters, digits, _ . - (max 64)", "err"); return false; }
    mutate(() => { p.id = newName; sel = { type: "pipe", id: newName }; });
    return true;
  }
  function inferPipeDefaults(a, b) {
    for (const p of doc.pipelines) {
      if ([a, b].includes(p.from_node_id) || [a, b].includes(p.to_node_id)) {
        if (p.bound_pressure_tag) return { tag: p.bound_pressure_tag, dia: p.nominal_diameter };
      }
    }
    return { tag: "", dia: lastDiameter };
  }

  // ------------------------------------------------------------------ pointer interaction
  function worldPoint(e) {
    const r = svg.getBoundingClientRect();
    return { x: (e.clientX - r.left - view.x) / view.s, y: (e.clientY - r.top - view.y) / view.s };
  }
  function nearestTerminal(w, excludeNode) {
    if (!doc) return null;
    const used = usedTerminals();
    const radius = 14 / view.s;
    let best = null, bd = radius;
    for (const n of doc.components) {
      const def = SYM[n.type]; if (!def || n.id === excludeNode) continue;
      for (const name of Object.keys(def.term)) {
        if (used.has(n.id + "/" + name)) continue;
        const t = termInfo(n, name);
        const d = Math.hypot(t.x - w.x, t.y - w.y);
        if (d <= bd) { bd = d; best = { node: n.id, term: name, x: t.x, y: t.y, dx: t.dx, dy: t.dy }; }
      }
    }
    return best;
  }
  function draftLast() { return draft.pts.length ? draft.pts[draft.pts.length - 1] : draft.start; }
  function draftAxisH() { return draft.pts.length ? draft.axisH : draft.dir.dx !== 0; }
  function draftPreview() {
    const pts = [draft.start, ...draft.pts];
    const last = draftLast(), horiz = draftAxisH();
    const sd = horiz ? { dx: 1, dy: 0 } : { dx: 0, dy: 1 };
    const target = draft.hover || (draft.cursor ? { x: draft.cursor.x, y: draft.cursor.y } : null);
    if (target) {
      const ed = draft.hover ? { dx: draft.hover.dx, dy: draft.hover.dy } : null;
      pts.push(...route(last, sd, target, ed), target);
    }
    return simplify(pts);
  }
  function draftAddWaypoint(p) {
    const last = draftLast(), horiz = draftAxisH();
    if (same(p, last)) return;
    let corner = null;
    if (p.x !== last.x && p.y !== last.y) corner = horiz ? { x: p.x, y: last.y } : { x: last.x, y: p.y };
    if (corner) draft.pts.push(corner);
    draft.pts.push({ x: p.x, y: p.y });
    draft.axisH = p.y === (corner || last).y;
  }
  function pipeStart(t) {
    draft = { node: t.node, term: t.term, start: { x: t.x, y: t.y }, dir: { dx: t.dx, dy: t.dy }, pts: [], axisH: t.dx !== 0, cursor: null, hover: null };
    requestRender();
  }
  function pipeFinish(t) {
    const a = nodeById(draft.node), b = nodeById(t.node);
    const last = draftLast(), horiz = draftAxisH();
    const sd = horiz ? { dx: 1, dy: 0 } : { dx: 0, dy: 1 };
    const end = { x: t.x, y: t.y };
    const full = simplify([draft.start, ...draft.pts, ...route(last, sd, end, { dx: t.dx, dy: t.dy }), end]);
    const def = inferPipeDefaults(a.id, b.id);
    const pipe = {
      id: newId("PIPE"), from_node_id: draft.node, from_terminal: draft.term, to_node_id: t.node, to_terminal: t.term,
      path_points: full.slice(1, -1), nominal_diameter: def.dia, bound_pressure_tag: def.tag,
    };
    draft = null;
    mutate(() => { doc.pipelines.push(pipe); }, { inspector: false });
    select({ type: "pipe", id: pipe.id });
    setTool({ kind: "pipe" });
    toast(`Pipeline ${pipe.id} created`, "ok");
  }

  let drag = null, pan = null, spaceDown = false;
  svg.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    if (draft && draft.pts.length) { draft.pts.pop(); draft.axisH = draft.pts.length ? draft.axisH : draft.dir.dx !== 0; requestRender(); }
  });
  svg.addEventListener("pointerdown", (e) => {
    if (!doc) return;
    const w = worldPoint(e);
    const panStart = e.button === 1 || (e.button === 0 && spaceDown);
    if (panStart) { pan = { sx: e.clientX, sy: e.clientY, vx: view.x, vy: view.y }; svg.classList.add("panning"); svg.setPointerCapture(e.pointerId); return; }
    if (e.button !== 0) return;

    if (mode === "design" && tool.kind === "pipe") {
      const t = nearestTerminal(w, draft ? draft.node : null);
      if (!draft) { if (t) pipeStart(t); else toast("Click a free connection point (circle) to start a pipeline", ""); }
      else if (t) pipeFinish(t);
      else { draftAddWaypoint({ x: snap(w.x), y: snap(w.y) }); requestRender(); }
      return;
    }
    if (mode === "design" && tool.kind === "place") { addNode(tool.type, w.x, w.y); setTool({ kind: "select" }); return; }

    const nodeEl = e.target.closest(".node");
    const pipeEl = e.target.closest(".pipe-g");
    if (nodeEl && nodeEl.dataset.node) {
      const n = nodeById(nodeEl.dataset.node);
      select({ type: "node", id: n.id });
      if (mode === "design") { drag = { id: n.id, wx: w.x, wy: w.y, ox: n.x, oy: n.y, moved: false }; svg.setPointerCapture(e.pointerId); }
    } else if (pipeEl) {
      select({ type: "pipe", id: pipeEl.dataset.pipe });
    } else {
      if (sel) select(null);
      pan = { sx: e.clientX, sy: e.clientY, vx: view.x, vy: view.y }; svg.classList.add("panning"); svg.setPointerCapture(e.pointerId);
    }
  });
  svg.addEventListener("pointermove", (e) => {
    if (!doc) return;
    const w = worldPoint(e);
    $("coords").textContent = `x ${snap(w.x)} · y ${snap(w.y)}`;
    if (pan) { view.x = pan.vx + (e.clientX - pan.sx); view.y = pan.vy + (e.clientY - pan.sy); applyView(); return; }
    if (drag) {
      const n = nodeById(drag.id);
      const nx = clamp(snap(drag.ox + (w.x - drag.wx)), 0, doc.grid_bounds.width);
      const ny = clamp(snap(drag.oy + (w.y - drag.wy)), 0, doc.grid_bounds.height);
      if (!drag.moved && (nx !== n.x || ny !== n.y)) { pushUndo(); drag.moved = true; }
      if (drag.moved && (nx !== n.x || ny !== n.y)) { n.x = nx; n.y = ny; rerouteAttached(n.id); requestRender(); }
      return;
    }
    if (mode === "design" && tool.kind === "place") { ghostPos = { x: clamp(snap(w.x), 0, doc.grid_bounds.width), y: clamp(snap(w.y), 0, doc.grid_bounds.height) }; requestRender(); }
    if (mode === "design" && tool.kind === "pipe" && draft) {
      draft.cursor = { x: snap(w.x), y: snap(w.y) };
      draft.hover = nearestTerminal(w, draft.node);
      requestRender();
    }
  });
  const endPointer = (e) => {
    if (pan) { pan = null; svg.classList.remove("panning"); }
    if (drag) {
      if (drag.moved) { markDirty(); renderInspector(); }
      drag = null;
    }
    try { svg.releasePointerCapture(e.pointerId); } catch (err) { /* not captured */ }
  };
  svg.addEventListener("pointerup", endPointer);
  svg.addEventListener("pointercancel", endPointer);
  svg.addEventListener("wheel", (e) => {
    e.preventDefault();
    const r = svg.getBoundingClientRect();
    zoomAt(e.clientX - r.left, e.clientY - r.top, Math.pow(1.0015, -e.deltaY));
  }, { passive: false });
  function zoomAt(px, py, factor) {
    const ns = clamp(view.s * factor, 0.2, 4);
    view.x = px - (px - view.x) * (ns / view.s);
    view.y = py - (py - view.y) * (ns / view.s);
    view.s = ns; applyView();
  }
  function zoomCenter(factor) { const r = svg.getBoundingClientRect(); zoomAt(r.width / 2, r.height / 2, factor); }
  function contentBounds() { return Schematic.contentBounds(doc); }
  function fitView() {
    const r = svg.getBoundingClientRect();
    const b = contentBounds(), w = b.x1 - b.x0, hh = b.y1 - b.y0;
    const s = clamp(Math.min((r.width - 40) / w, (r.height - 40) / hh), 0.2, 2);
    view = { s, x: (r.width - w * s) / 2 - b.x0 * s, y: (r.height - hh * s) / 2 - b.y0 * s };
    applyView();
  }
  $("zoomIn").onclick = () => zoomCenter(1.25);
  $("zoomOut").onclick = () => zoomCenter(0.8);
  $("zoomFit").onclick = fitView;
  new ResizeObserver(() => { if (!doc) fitView(); }).observe($("stage"));

  // drag & drop from the palette
  svg.addEventListener("dragover", (e) => { if (doc && mode === "design") { e.preventDefault(); e.dataTransfer.dropEffect = "copy"; } });
  svg.addEventListener("drop", (e) => {
    if (!doc || mode !== "design") return;
    e.preventDefault();
    const type = e.dataTransfer.getData("application/x-scada-symbol") || e.dataTransfer.getData("text/plain");
    if (!SYM[type]) return;
    const w = worldPoint(e);
    addNode(type, w.x, w.y);
    setTool({ kind: "select" });
  });

  // keyboard
  document.addEventListener("keydown", (e) => {
    const tag = (e.target.tagName || "").toLowerCase();
    const typing = tag === "input" || tag === "textarea" || tag === "select";
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") { e.preventDefault(); save(); return; }
    if (typing) return;
    if (e.key === " ") { spaceDown = true; e.preventDefault(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "z") { e.preventDefault(); e.shiftKey ? redo() : undo(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "y") { e.preventDefault(); redo(); return; }
    switch (e.key) {
      case "Escape": if (draft) { draft = null; requestRender(); } else if (tool.kind !== "select") setTool({ kind: "select" }); else if (sel) select(null); break;
      case "Delete": case "Backspace": e.preventDefault(); deleteSelection(); break;
      case "r": case "R": rotateSelection(); break;
      case "v": case "V": setTool({ kind: "select" }); break;
      case "p": case "P": if (doc) setTool({ kind: "pipe" }); break;
      case "+": case "=": zoomCenter(1.25); break;
      case "-": case "_": zoomCenter(0.8); break;
      case "0": fitView(); break;
      case "ArrowLeft": case "ArrowRight": case "ArrowUp": case "ArrowDown": {
        if (!sel || sel.type !== "node" || mode !== "design") break;
        e.preventDefault();
        const g = grid(), d = { ArrowLeft: [-g, 0], ArrowRight: [g, 0], ArrowUp: [0, -g], ArrowDown: [0, g] }[e.key];
        mutate(() => { const n = nodeById(sel.id); n.x = clamp(n.x + d[0], 0, doc.grid_bounds.width); n.y = clamp(n.y + d[1], 0, doc.grid_bounds.height); rerouteAttached(n.id); });
        break;
      }
    }
  });
  document.addEventListener("keyup", (e) => { if (e.key === " ") spaceDown = false; });

  // ------------------------------------------------------------------ palette
  function buildPalette() {
    const box = $("symbols");
    box.textContent = "";
    for (const type of PALETTE_ORDER) {
      const def = SYM[type];
      const b = h("button", "sym-btn");
      b.dataset.type = type; b.draggable = true; b.title = def.label; b.setAttribute("aria-pressed", "false");
      const [x, y, w, hh] = def.box;
      const icon = sv("svg", { viewBox: `${x - 6} ${y - 6} ${w + 12} ${hh + 12}` });
      const holder = sv("g", { class: "node" }, icon);
      def.draw(sv("g", { class: "sym" }, holder));
      b.append(icon, h("span", "", def.label));
      b.addEventListener("click", () => { if (!doc) { toast("Open or create a station first", "err"); return; } setTool(tool.kind === "place" && tool.type === type ? { kind: "select" } : { kind: "place", type }); });
      b.addEventListener("dragstart", (e) => { e.dataTransfer.setData("application/x-scada-symbol", type); e.dataTransfer.setData("text/plain", type); e.dataTransfer.effectAllowed = "copy"; });
      box.append(b);
    }
  }

  // ------------------------------------------------------------------ inspector
  function field(parent, label, input) { const r = h("div", "row"); r.append(h("label", "", label), input); parent.append(r); return r; }
  function textInput(value, onCommit, opts) {
    const i = h("input"); i.type = "text"; i.value = value == null ? "" : value;
    if (opts && opts.maxLength) i.maxLength = opts.maxLength;
    if (opts && opts.placeholder) i.placeholder = opts.placeholder;
    if (opts && opts.readonly) i.readOnly = true;
    i.addEventListener("change", () => onCommit(i.value, i));
    return i;
  }
  function numInput(value, onCommit, opts) {
    const i = h("input"); i.type = "number"; i.value = value == null ? "" : value;
    if (opts) { if (opts.min != null) i.min = opts.min; if (opts.max != null) i.max = opts.max; if (opts.step != null) i.step = opts.step; }
    i.addEventListener("change", () => onCommit(i.value === "" ? null : Number(i.value), i));
    return i;
  }
  function tagPicker(parent, label, value, kind, onCommit) {
    const input = textInput(value, (v, el) => {
      v = v.trim();
      if (v && !TAG_RE.test(v)) { el.classList.add("invalid"); toast("Tag ids may contain letters, digits, _ . : - (max 128)", "err"); return; }
      el.classList.remove("invalid");
      onCommit(v); showHint(v);
    }, { placeholder: kind === "analog" ? "pick an analog tag…" : "pick a digital tag…", maxLength: 128 });
    input.setAttribute("list", kind === "analog" ? "dl-analog" : "dl-digital");
    input.setAttribute("autocomplete", "off");
    field(parent, label, input);
    const hint = h("div", "sub"); parent.append(hint);
    function showHint(v) {
      const t = v && tagsById.get(v);
      hint.className = "sub";
      if (!v) hint.textContent = "";
      else if (t) { hint.className = "sub ok"; hint.textContent = `live: ${t.type === "digital" ? String(t.state) : (t.value !== undefined ? t.value.toFixed(1) : "—") + " " + (t.unit || "")} · ${t.quality}`; }
      else { hint.className = "sub bad"; hint.textContent = "not in the RTDB yet (it will resolve once a FEP sends it)"; }
    }
    showHint(value);
    return input;
  }

  function renderInspector() {
    const el = $("inspector");
    el.textContent = "";
    if (!doc) { el.append(h("p", "hint", "Open or create a station to start.")); return; }
    if (mode === "live") return inspectLive(el);
    if (!sel) return inspectStation(el);
    if (sel.type === "node") { const n = nodeById(sel.id); if (n) return inspectNode(el, n); }
    if (sel.type === "pipe") { const p = pipeById(sel.id); if (p) return inspectPipe(el, p); }
    sel = null; inspectStation(el);
  }

  function inspectStation(el) {
    el.append(h("h2", "", "Station"));
    field(el, "Station id", textInput(doc.station_id, () => {}, { readonly: true }));
    field(el, "Name", textInput(doc.name, (v) => mutate(() => { doc.name = v.trim(); }, { inspector: false }), { maxLength: 128 }));
    const ta = h("textarea"); ta.rows = 3; ta.value = doc.description || ""; ta.maxLength = 1000;
    ta.addEventListener("change", () => mutate(() => { doc.description = ta.value.trim(); }, { inspector: false }));
    field(el, "Description", ta);
    el.append(h("h2", "", "Canvas"));
    const b = doc.grid_bounds;
    field(el, "Width (px)", numInput(b.width, (v, i) => { if (v == null || v < 200 || v > 10000) { i.value = b.width; return; } mutate(() => { b.width = Math.round(v); }, { inspector: false }); }, { min: 200, max: 10000, step: b.grid_size }));
    field(el, "Height (px)", numInput(b.height, (v, i) => { if (v == null || v < 200 || v > 10000) { i.value = b.height; return; } mutate(() => { b.height = Math.round(v); }, { inspector: false }); }, { min: 200, max: 10000, step: b.grid_size }));
    field(el, "Grid size (px)", numInput(b.grid_size, (v, i) => { if (v == null || v < 5 || v > 100) { i.value = b.grid_size; return; } mutate(() => { b.grid_size = Math.round(v); }, { inspector: false }); }, { min: 5, max: 100, step: 5 }));

    el.append(h("h2", "", "Pipe colour by pressure"));
    el.append(h("p", "hint", "Pipes take the colour of the first band whose limit is >= the bound pressure."));
    const bands = doc.pressure_bands && doc.pressure_bands.length ? doc.pressure_bands : null;
    if (!bands) {
      const bt = h("button", "small", "Use default bands"); bt.onclick = () => mutate(() => { doc.pressure_bands = JSON.parse(JSON.stringify(DEFAULT_BANDS)); });
      el.append(bt);
    } else {
      bands.forEach((band, i) => {
        const r = h("div", "rule");
        const up = numInput(band.up_to >= 1e8 ? "" : band.up_to, (v) => mutate(() => { band.up_to = v == null ? 1e9 : v; }, { inspector: false }), { step: "any" });
        up.placeholder = "∞"; up.style.gridColumn = "span 2"; up.title = "Upper pressure limit (empty = no limit)";
        const col = h("input"); col.type = "color"; col.value = band.color;
        col.addEventListener("change", () => mutate(() => { band.color = col.value; }, { inspector: false }));
        const del = h("button", "small danger", "×"); del.title = "Remove band";
        del.onclick = () => mutate(() => { doc.pressure_bands.splice(i, 1); });
        r.append(up, col, del); el.append(r);
      });
      const add = h("button", "small", "+ Add band");
      add.onclick = () => mutate(() => {
        const last = doc.pressure_bands[doc.pressure_bands.length - 1];
        if (last && last.up_to >= 1e8) last.up_to = (doc.pressure_bands.length > 1 ? doc.pressure_bands[doc.pressure_bands.length - 2].up_to : 0) + 50;
        doc.pressure_bands.push({ up_to: 1e9, color: "#f0883e" });
      });
      el.append(add);
    }
    el.append(h("h2", "", "Summary"));
    const kv = h("div", "kv");
    for (const [k, v] of [["Components", doc.components.length], ["Pipelines", doc.pipelines.length],
      ["Bound tags", boundTags().length], ["Saved version", doc.version || "—"],
      ["Deployed version", status && status.deployed_version ? status.deployed_version : "—"]]) {
      kv.append(h("span", "", k), h("span", "", String(v)));
    }
    el.append(kv);
  }

  function boundTags() {
    const s = new Set();
    for (const n of doc.components) { if (n.bindings.primary_tag) s.add(n.bindings.primary_tag); if (n.bindings.state_tag) s.add(n.bindings.state_tag); }
    for (const p of doc.pipelines) if (p.bound_pressure_tag) s.add(p.bound_pressure_tag);
    return [...s].sort();
  }

  function inspectNode(el, n) {
    const def = SYM[n.type];
    el.append(h("h2", "", (def ? def.label : n.type)));
    field(el, "Id", textInput(n.id, (v, i) => { if (!renameNode(n, v)) i.value = n.id; }, { maxLength: 64 }));
    field(el, "Label", textInput(n.display.label || "", (v) => mutate(() => { n.display.label = v.trim() || undefined; }, { inspector: false }), { maxLength: 64, placeholder: "shown instead of the id" }));
    const g = grid();
    field(el, "X", numInput(n.x, (v, i) => { if (v == null) { i.value = n.x; return; } mutate(() => { n.x = clamp(Math.round(v / g) * g, 0, doc.grid_bounds.width); rerouteAttached(n.id); }); }, { step: g }));
    field(el, "Y", numInput(n.y, (v, i) => { if (v == null) { i.value = n.y; return; } mutate(() => { n.y = clamp(Math.round(v / g) * g, 0, doc.grid_bounds.height); rerouteAttached(n.id); }); }, { step: g }));
    const rot = h("div"); rot.style.display = "flex"; rot.style.gap = "6px";
    rot.append(h("span", "", n.rotation + "°"));
    const rb = h("button", "small", "Rotate 90° (R)"); rb.onclick = rotateSelection; rot.append(rb);
    field(el, "Rotation", rot);

    el.append(h("h2", "", "Tag bindings"));
    tagPicker(el, "Primary tag", n.bindings.primary_tag || "", "analog", (v) => mutate(() => { n.bindings.primary_tag = v || undefined; }, { inspector: false }));
    tagPicker(el, "State / alarm tag", n.bindings.state_tag || "", "digital", (v) => mutate(() => { n.bindings.state_tag = v || undefined; }, { inspector: false }));

    el.append(h("h2", "", "Display"));
    field(el, "Label prefix", textInput(n.display.label_prefix || "", (v) => mutate(() => { n.display.label_prefix = v.trim() || undefined; }, { inspector: false }), { maxLength: 32, placeholder: "e.g. P-IN" }));
    field(el, "Unit", textInput(n.display.unit || "", (v) => mutate(() => { n.display.unit = v.trim() || undefined; }, { inspector: false }), { maxLength: 16, placeholder: "from the tag" }));
    field(el, "Decimals", numInput(n.display.decimals, (v, i) => {
      if (v != null && (v < 0 || v > 6)) { i.value = n.display.decimals == null ? "" : n.display.decimals; return; }
      mutate(() => { n.display.decimals = v == null ? undefined : Math.round(v); }, { inspector: false });
    }, { min: 0, max: 6, step: 1 }));
    const inv = h("input"); inv.type = "checkbox"; inv.checked = !!n.display.invert_state; inv.style.width = "auto";
    inv.addEventListener("change", () => mutate(() => { n.display.invert_state = inv.checked || undefined; }, { inspector: false }));
    field(el, "Invert state", inv);
    el.append(h("p", "hint", "Valves are shown OPEN when the state tag is true; tick this if true means closed."));

    el.append(h("h2", "", "Colour rules"));
    el.append(h("p", "hint", "The first matching rule colours the live value. List the most severe first. Digital tags compare as 1 (true) / 0 (false)."));
    (n.display.color_rules || []).forEach((r, i) => {
      const rr = h("div", "rule");
      const op = h("select"); for (const o of [">", ">=", "<", "<=", "==", "!="]) { const opt = h("option", "", o); opt.value = o; op.append(opt); } op.value = r.op;
      op.addEventListener("change", () => mutate(() => { r.op = op.value; }, { inspector: false }));
      const val = numInput(r.value, (v, inp) => { if (v == null) { inp.value = r.value; return; } mutate(() => { r.value = v; }, { inspector: false }); }, { step: "any" });
      const col = h("input"); col.type = "color"; col.value = r.color;
      col.addEventListener("change", () => mutate(() => { r.color = col.value; }, { inspector: false }));
      const del = h("button", "small danger", "×"); del.title = "Remove rule"; del.onclick = () => mutate(() => { n.display.color_rules.splice(i, 1); });
      rr.append(op, val, col, del); el.append(rr);
    });
    const add = h("button", "small", "+ Add colour rule");
    add.disabled = (n.display.color_rules || []).length >= 8;
    add.onclick = () => mutate(() => { (n.display.color_rules = n.display.color_rules || []).push({ op: ">=", value: 0, color: "#f0883e" }); });
    el.append(add);

    el.append(h("h2", "", "Danger zone"));
    const del = h("button", "danger", "Delete element (Del)"); del.onclick = deleteSelection; el.append(del);
  }

  function inspectPipe(el, p) {
    el.append(h("h2", "", "Pipeline"));
    field(el, "Id", textInput(p.id, (v, i) => { if (!renamePipe(p, v)) i.value = p.id; }, { maxLength: 64 }));
    field(el, "From", textInput(`${p.from_node_id} · ${p.from_terminal}`, () => {}, { readonly: true }));
    field(el, "To", textInput(`${p.to_node_id} · ${p.to_terminal}`, () => {}, { readonly: true }));
    const dia = h("select");
    const opts = DIAMETERS.includes(p.nominal_diameter) ? DIAMETERS : [p.nominal_diameter, ...DIAMETERS];
    for (const d of opts) { const o = h("option", "", d); o.value = d; dia.append(o); }
    dia.value = p.nominal_diameter;
    dia.addEventListener("change", () => { lastDiameter = dia.value; mutate(() => { p.nominal_diameter = dia.value; }, { inspector: false }); });
    field(el, "Diameter", dia);
    tagPicker(el, "Pressure tag", p.bound_pressure_tag || "", "analog", (v) => mutate(() => { p.bound_pressure_tag = v || undefined; }, { inspector: false }));
    el.append(h("p", "hint", "The pipe is coloured live by this pressure (see the bands in the station settings)."));
    el.append(h("h2", "", "Route"));
    el.append(h("p", "hint", `${(p.path_points || []).length} bend point(s). Moving or rotating a connected element re-routes the pipe automatically.`));
    const rr = h("button", "small", "Re-route automatically"); rr.onclick = () => mutate(() => autoRoutePipe(p)); el.append(rr);
    el.append(h("h2", "", "Danger zone"));
    const del = h("button", "danger", "Delete pipeline (Del)"); del.onclick = deleteSelection; el.append(del);
  }

  function inspectLive(el) {
    el.append(h("h2", "", sel ? "Live data" : "Live station"));
    const kv = h("div", "kv");
    const add = (k, v) => kv.append(h("span", "", k), h("span", "", String(v)));
    if (!sel) {
      add("Connection", $("conntxt").textContent);
      add("Tags received", liveTags.size);
      add("Active alarms", liveAlarms.size);
      add("Unacknowledged", [...liveAlarms.values()].filter((a) => a.state === "UNACK_ALARM" || a.state === "UNACK_RTN").length);
      el.append(kv);
      el.append(h("p", "hint", "Click an element to see its live values. Valves show OPEN/CLOSED, transmitters their value; elements with an active alarm flash red until acknowledged on the dashboard."));
      return;
    }
    let tags = [], title = "";
    if (sel.type === "node") { const n = nodeById(sel.id); title = n.id; tags = [n.bindings.primary_tag, n.bindings.state_tag]; }
    else { const p = pipeById(sel.id); title = p.id; tags = [p.bound_pressure_tag]; }
    add("Element", title);
    for (const t of tags.filter(Boolean)) {
      const v = liveTags.get(t);
      add(t, !v ? "no data" : v.type === "digital" ? String(v.state) : `${v.value !== undefined ? v.value.toFixed(2) : "—"} ${v.unit || ""}`);
      if (v) add("  quality", v.quality);
    }
    el.append(kv);
    const alarms = [...liveAlarms.values()].filter((a) => tags.includes(a.tag_id));
    el.append(h("h2", "", "Alarms"));
    if (!alarms.length) el.append(h("p", "hint", "No active alarms for this element."));
    for (const a of alarms) {
      const d = h("div", ""); d.style.marginBottom = "8px";
      const pill = h("span", "pill s" + a.severity_code, a.severity);
      d.append(pill, h("span", "", " " + a.alarm_id), h("div", "hint", `${a.message} · ${a.state.replaceAll("_", " ")}`));
      el.append(d);
    }
  }

  // ------------------------------------------------------------------ problems panel
  function localIssues() {
    const out = [];
    if (!doc) return out;
    const connected = new Set();
    for (const p of doc.pipelines) {
      connected.add(p.from_node_id); connected.add(p.to_node_id);
      if (!p.bound_pressure_tag) out.push({ severity: "warning", id: p.id, message: `Pipe ${p.id} has no pressure tag, it will not change colour with pressure` });
    }
    for (const n of doc.components) {
      const def = SYM[n.type];
      if (!def) { out.push({ severity: "error", id: n.id, message: `${n.id} has an unknown type ${n.type}` }); continue; }
      if (def.kind === "instrument" && !n.bindings.primary_tag) out.push({ severity: "warning", id: n.id, message: `${n.id} has no primary tag bound` });
      if (def.kind === "valve" && !n.bindings.state_tag) out.push({ severity: "warning", id: n.id, message: `${n.id} has no state tag bound, so its position cannot be shown live` });
      if (!connected.has(n.id)) out.push({ severity: "warning", id: n.id, message: `${n.id} is not connected to any pipe` });
    }
    return out;
  }
  function renderProblems() {
    const list = $("problemsList");
    list.textContent = "";
    const items = serverIssues.filter((i) => i.severity === "error").concat(localIssues());
    const errs = items.filter((i) => i.severity === "error").length, warns = items.length - errs;
    $("problemsCount").textContent = items.length ? `Problems: ${errs} error${errs === 1 ? "" : "s"}, ${warns} warning${warns === 1 ? "" : "s"}` : "Problems: none";
    if (!items.length) { list.append(h("li", "none", "No problems found.")); return; }
    for (const i of items) {
      const li = h("li");
      li.append(h("span", i.severity === "error" ? "sev-err" : "sev-warn", i.severity === "error" ? "error" : "warning"), h("span", "", i.message));
      li.onclick = () => {
        if (mode !== "design" || !i.id) return;
        if (nodeById(i.id)) select({ type: "node", id: i.id }); else if (pipeById(i.id)) select({ type: "pipe", id: i.id });
      };
      list.append(li);
    }
  }
  $("problemsToggle").onclick = () => {
    const p = $("problems"); p.classList.toggle("collapsed");
    $("problemsArrow").innerHTML = p.classList.contains("collapsed") ? "&#9656;" : "&#9662;";
  };
  function showProblems() { $("problems").classList.remove("collapsed"); $("problemsArrow").innerHTML = "&#9662;"; }

  // ------------------------------------------------------------------ chrome (status, buttons)
  function updateChrome() {
    const st = $("status");
    const has = !!doc;
    $("stationTitle").textContent = has ? `· ${doc.name} (${doc.station_id})` : "";
    $("btnSave").disabled = !has || mode !== "design";
    $("btnDeploy").disabled = !has;
    $("btnExport").disabled = !has;
    $("btnUndo").disabled = !undoStack.length || mode !== "design";
    $("btnRedo").disabled = !redoStack.length || mode !== "design";
    if (!has) { st.className = "badge"; st.textContent = "no station"; renderProblems(); return; }
    let t, cls = "badge";
    if (!doc.version) { t = "new · not saved"; cls += " dirty"; }
    else if (dirty) { t = `unsaved changes (saved v${doc.version})`; cls += " dirty"; }
    else {
      t = `v${doc.version} saved`;
      if (status && status.deployed_version) t += status.deployed_version === doc.version ? ` · deployed v${status.deployed_version}` : ` · deployed v${status.deployed_version} (outdated)`;
      else t += " · not deployed";
      cls += " ok";
    }
    st.className = cls; st.textContent = t;
    renderProblems();
  }

  // ------------------------------------------------------------------ server API
  async function api(method, path, body) {
    const headers = {};
    if (TOKEN) headers.Authorization = "Bearer " + TOKEN;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let r;
    try { r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: "no-store" }); }
    catch (e) { const err = new Error("Cannot reach the gateway: " + e.message); err.status = 0; throw err; }
    let j = {};
    try { j = await r.json(); } catch (e) { /* empty body */ }
    if (!r.ok) {
      let msg = j.error || "HTTP " + r.status;
      if (r.status === 401) msg = "Authentication required: sign in on the dashboard first (Dashboard link, top right), then come back here.";
      if (r.status === 403) msg = "Not allowed: " + (j.error || "your role may not do this") + " (editing and deploying layouts needs the engineer role).";
      const err = new Error(msg); err.status = r.status; err.data = j; throw err;
    }
    return j;
  }

  async function loadTags() {
    try {
      const j = await api("GET", "/api/v1/telemetry/current");
      tagsById.clear();
      const an = $("dl-analog"), di = $("dl-digital");
      an.textContent = ""; di.textContent = "";
      for (const t of j.tags) {
        tagsById.set(t.tag_id, t);
        const o = h("option"); o.value = t.tag_id;
        o.label = t.type === "digital" ? `${t.state} · ${t.quality}` : `${t.value !== undefined ? t.value.toFixed(1) : "—"} ${t.unit || ""} · ${t.quality}`;
        (t.type === "digital" ? di : an).append(o);
      }
    } catch (e) { toast("Tag list unavailable (is the core running?): " + e.message, "err"); }
  }

  async function refreshStations(selectId) {
    try {
      const j = await api("GET", "/api/v1/topology");
      const selEl = $("stationSel");
      selEl.textContent = "";
      const first = h("option", "", j.stations.length ? "— saved stations —" : "— none saved yet —"); first.value = ""; selEl.append(first);
      for (const st of j.stations) { const o = h("option", "", `${st.station_id} (v${st.version})`); o.value = st.station_id; selEl.append(o); }
      if (selectId) selEl.value = selectId;
      return j.stations;
    } catch (e) { toast("Cannot list stations: " + e.message, "err"); return []; }
  }

  function setDoc(d, st) {
    doc = d; status = st || null; dirty = false; sel = null; draft = null; serverIssues = [];
    undoStack.length = 0; redoStack.length = 0;
    if (!doc.pressure_bands) doc.pressure_bands = [];
    $("stationId").value = doc.station_id;
    const u = new URL(location.href); u.searchParams.set("station", doc.station_id); history.replaceState(null, "", u);
    renderInspector(); fitView(); requestRender();
  }
  function confirmDiscard() { return !dirty || window.confirm("You have unsaved changes. Discard them?"); }

  async function openStation(id) {
    id = (id || "").trim();
    if (!/^[A-Za-z0-9_\-]{1,64}$/.test(id)) { toast("Enter a valid station id (letters, digits, _ or -)", "err"); return; }
    if (!confirmDiscard()) return;
    try {
      const j = await api("GET", "/api/v1/topology/" + encodeURIComponent(id));
      setDoc(j.topology, j.status);
      $("stationSel").value = id;
      toast(`Opened ${id} (v${j.status.version})`, "ok");
    } catch (e) {
      if (e.status === 404) toast(`Station "${id}" is not saved yet. Use "New blank" or "New from example".`, "err");
      else toast(e.message, "err");
    }
  }
  function newBlank() {
    const id = $("stationId").value.trim();
    if (!/^[A-Za-z0-9_\-]{1,64}$/.test(id)) { toast("Type a valid station id first (letters, digits, _ or -)", "err"); $("stationId").focus(); return; }
    if (!confirmDiscard()) return;
    setDoc({
      station_id: id, name: id, description: "", version: 0, grid_bounds: { width: 1200, height: 600, grid_size: 20 },
      components: [], pipelines: [], pressure_bands: JSON.parse(JSON.stringify(DEFAULT_BANDS)),
    }, null);
    markDirty(); toast("Blank drawing created. Drag symbols from the left, then save.", "ok");
  }
  async function newExample() {
    const id = $("stationId").value.trim();
    if (!/^[A-Za-z0-9_\-]{1,64}$/.test(id)) { toast("Type a valid station id first (letters, digits, _ or -)", "err"); $("stationId").focus(); return; }
    if (!confirmDiscard()) return;
    try {
      const j = await api("GET", "/api/v1/topology/" + encodeURIComponent(id) + "/example");
      setDoc(j.topology, null); markDirty();
      toast("Example station loaded. Review the bindings, then save.", "ok");
    } catch (e) { toast(e.message, "err"); }
  }

  async function save() {
    if (!doc || mode !== "design") return false;
    const user = operator();
    if (!user) { toast("Enter your operator name first (top right)", "err"); $("user").focus(); return false; }
    if (!doc.name.trim()) { toast("The station needs a name", "err"); return false; }
    doc.updated_by = user;
    try {
      const j = await api("POST", "/api/v1/topology/" + encodeURIComponent(doc.station_id), doc);
      doc = j.topology; if (!doc.pressure_bands) doc.pressure_bands = [];
      status = j.status; dirty = false; serverIssues = [];
      fixSelection(); renderInspector(); requestRender();
      refreshStations(doc.station_id);
      toast(`Saved ${doc.station_id} as v${doc.version}`, "ok");
      return true;
    } catch (e) {
      if (e.status === 422 && e.data && e.data.issues) { serverIssues = e.data.issues; showProblems(); updateChrome(); toast(e.message, "err"); }
      else if (e.status === 409) toast(`${e.message}. Someone else saved a newer version; export your work, reload the station and re-apply it.`, "err");
      else toast("Save failed: " + e.message, "err");
      return false;
    }
  }

  // ------------------------------------------------------------------ deploy
  const dlg = $("deployDlg");
  async function openDeploy() {
    if (!doc) return;
    const user = operator();
    if (!user) { toast("Enter your operator name first (top right)", "err"); $("user").focus(); return; }
    if (dirty || !doc.version) { if (!(await save())) return; }
    const tags = boundTags();
    $("deployTitle").textContent = `Deploy ${doc.station_id} v${doc.version} to the runtime`;
    const body = $("deployBody"); body.textContent = "";
    body.append(h("p", "", `This publishes the saved layout (${doc.components.length} components, ${doc.pipelines.length} pipelines, ${tags.length} bound tags) as ${user}.`));
    body.append(h("p", "hint", "The core stores the configuration and notifies every connected FEP immediately; nothing is restarted. FEPs started with -config-filter then stream only the bound tags."));
    $("deployGo").style.display = ""; $("deployGo").disabled = false; $("deployGo").textContent = "Deploy";
    $("deployCancel").textContent = "Cancel";
    dlg.showModal();
  }
  async function runDeploy() {
    const body = $("deployBody");
    $("deployGo").disabled = true;
    try {
      const j = await api("POST", `/api/v1/topology/${encodeURIComponent(doc.station_id)}/deploy`, { deployed_by: operator() });
      status = Object.assign({}, status, { deployed_version: j.version, deployed_by: j.deployed_by, changed_since_deploy: false });
      $("deployTitle").textContent = `Deployed ${doc.station_id} v${j.version}`;
      body.textContent = "";
      body.append(h("p", "", `Configuration applied. ${j.subscribers_notified} runtime subscriber(s) (FEPs, services) were notified.`));
      const box = h("div", "taglist");
      for (const t of j.live_tags) box.append(h("div", "live", "✔ " + t + "  (live in the RTDB)"));
      for (const t of j.unresolved_tags) box.append(h("div", "unres", "⚠ " + t + "  (no data received yet)"));
      body.append(h("p", "", `${j.bound_tags.length} bound tags: ${j.live_tags.length} live, ${j.unresolved_tags.length} unresolved.`), box);
      if (j.unresolved_tags.length) body.append(h("p", "hint", "Unresolved tags are not an error: they resolve as soon as a FEP delivers a value. Check the tag ids if they never do."));
      $("deployGo").style.display = "none"; $("deployCancel").textContent = "Close";
      updateChrome();
    } catch (e) {
      body.append(h("p", "sev-err", "Deployment failed: " + e.message));
      if (e.data && e.data.issues) { serverIssues = e.data.issues; showProblems(); updateChrome(); }
      $("deployGo").disabled = false;
    }
  }
  $("deployGo").onclick = runDeploy;
  $("deployCancel").onclick = () => dlg.close();

  function exportJson() {
    const blob = new Blob([JSON.stringify(doc, null, 2)], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob); a.download = `${doc.station_id}_v${doc.version || 0}.json`;
    document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  // ------------------------------------------------------------------ wiring
  $("modeDesign").onclick = () => setMode("design");
  $("modeLive").onclick = () => { if (!doc) { toast("Open a station first", "err"); return; } setMode("live"); };
  $("toolSelect").onclick = () => setTool({ kind: "select" });
  $("toolPipe").onclick = () => { if (!doc) { toast("Open or create a station first", "err"); return; } setTool(tool.kind === "pipe" ? { kind: "select" } : { kind: "pipe" }); };
  $("btnOpen").onclick = () => openStation($("stationId").value);
  $("stationId").addEventListener("keydown", (e) => { if (e.key === "Enter") openStation($("stationId").value); });
  $("stationSel").onchange = (e) => { if (e.target.value) openStation(e.target.value); };
  $("btnNewBlank").onclick = newBlank;
  $("btnNewExample").onclick = newExample;
  $("btnSave").onclick = save;
  $("btnDeploy").onclick = openDeploy;
  $("btnExport").onclick = exportJson;
  $("btnUndo").onclick = undo;
  $("btnRedo").onclick = redo;
  $("user").addEventListener("change", () => { try { localStorage.setItem(USER_KEY, $("user").value.trim()); } catch (e) { /* storage blocked */ } });
  window.addEventListener("beforeunload", (e) => { if (dirty) { e.preventDefault(); e.returnValue = ""; } });
  if (URL_TOKEN) $("dashLink").href = "/?token=" + encodeURIComponent(URL_TOKEN);

  // ------------------------------------------------------------------ start
  (async function init() {
    try { $("user").value = localStorage.getItem(USER_KEY) || ""; } catch (e) { /* storage blocked */ }
    if (SESSION && SESSION.user) { $("user").value = SESSION.user; $("user").readOnly = true; $("user").title = "Signed in on the dashboard"; }
    initScene(); buildPalette(); syncTools(); renderInspector(); fitView(); requestRender();
    await loadTags();
    const stations = await refreshStations();
    const wanted = params.get("station");
    if (wanted) await openStation(wanted);
    else if (stations.length) await openStation(stations[0].station_id);
  })();
})();
