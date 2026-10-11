"use strict";
/*
 * Shared schematic rendering used by the Engineering Studio (editor.js) and the
 * operator dashboard (dashboard.js): symbol library, geometry, SVG drawing of
 * nodes and pipes, and the live-state logic (valve positions, values, pressure
 * colours, alarm highlighting).
 *
 * Terminal names and the symbol geometry must match pkg/model/station.go.
 */
const Schematic = (() => {
  const SVGNS = "http://www.w3.org/2000/svg";
  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
  const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();

  function sv(tag, attrs, parent) {
    const e = document.createElementNS(SVGNS, tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  const line = (g, x1, y1, x2, y2, cls) => sv("line", { x1, y1, x2, y2, class: cls || "" }, g);
  const poly = (g, pts, cls) => sv("polygon", { points: pts, class: cls || "" }, g);
  const text = (g, x, y, t) => { const e = sv("text", { x, y }, g); e.textContent = t; return e; };
  const bowtie = (g, half, hh, cls) => { poly(g, `${-half},${-hh} 0,0 ${-half},${hh}`, cls); poly(g, `${half},${-hh} 0,0 ${half},${hh}`, cls); };

  // term: name -> [x, y, outwardDx, outwardDy] in the symbol's own (unrotated) frame.
  const SYM = {
    BALL_VALVE: {
      label: "Ball valve", prefix: "BV", kind: "valve", box: [-22, -20, 44, 34],
      term: { in: [-20, 0, -1, 0], out: [20, 0, 1, 0] },
      draw(g) { bowtie(g, 20, 11, "vbody"); line(g, 0, 0, 0, -16); line(g, -7, -16, 7, -16); },
    },
    SLAM_SHUT_VALVE: {
      label: "Slam-shut valve", prefix: "SSV", kind: "valve", box: [-22, -34, 44, 48],
      term: { in: [-20, 0, -1, 0], out: [20, 0, 1, 0] },
      draw(g) { bowtie(g, 20, 11, "vbody"); line(g, 0, 0, 0, -12); sv("rect", { x: -9, y: -30, width: 18, height: 18, class: "sfill" }, g); text(g, 0, -21, "SS"); },
    },
    REGULATOR: {
      label: "Regulator", prefix: "REG", kind: "equipment", box: [-42, -38, 84, 52],
      term: { in: [-40, 0, -1, 0], out: [40, 0, 1, 0] },
      draw(g) {
        line(g, -40, 0, -14, 0, "stub"); line(g, 14, 0, 40, 0, "stub");
        bowtie(g, 14, 10, "sfill"); line(g, 0, 0, 0, -12);
        sv("circle", { cx: 0, cy: -24, r: 12, class: "sfill" }, g); line(g, -12, -24, 12, -24);
      },
    },
    FILTER: {
      label: "Filter", prefix: "FLT", kind: "equipment", box: [-42, -20, 84, 40],
      term: { in: [-40, 0, -1, 0], out: [40, 0, 1, 0] },
      draw(g) {
        line(g, -40, 0, -26, 0, "stub"); line(g, 26, 0, 40, 0, "stub");
        sv("rect", { x: -26, y: -16, width: 52, height: 32, class: "sfill" }, g);
        for (const x of [-14, 0, 14]) line(g, x - 6, 12, x + 6, -12);
      },
    },
    ODORIZER: {
      label: "Odorizer", prefix: "ODR", kind: "equipment", box: [-42, -22, 84, 44],
      term: { in: [-40, 0, -1, 0], out: [40, 0, 1, 0] },
      draw(g) {
        line(g, -40, 0, -26, 0, "stub"); line(g, 26, 0, 40, 0, "stub");
        sv("rect", { x: -26, y: -18, width: 52, height: 36, rx: 4, class: "sfill" }, g); text(g, 0, 0, "ODR");
      },
    },
    PRESSURE_TRANSMITTER: {
      label: "Pressure transmitter", prefix: "PT", kind: "instrument", unit: "PSI", box: [-18, -24, 36, 46],
      term: { port: [0, 20, 0, 1] },
      draw(g) { line(g, 0, 6, 0, 20, "stub"); sv("circle", { cx: 0, cy: -8, r: 14, class: "sfill" }, g); text(g, 0, -8, "PT"); },
    },
    TEMPERATURE_TRANSMITTER: {
      label: "Temperature transmitter", prefix: "TT", kind: "instrument", unit: "°C", box: [-18, -24, 36, 46],
      term: { port: [0, 20, 0, 1] },
      draw(g) { line(g, 0, 6, 0, 20, "stub"); sv("circle", { cx: 0, cy: -8, r: 14, class: "sfill" }, g); text(g, 0, -8, "TT"); },
    },
    PRESSURE_GAUGE: {
      label: "Pressure gauge", prefix: "PG", kind: "instrument", unit: "PSI", box: [-18, -24, 36, 46],
      term: { port: [0, 20, 0, 1] },
      draw(g) { line(g, 0, 6, 0, 20, "stub"); sv("circle", { cx: 0, cy: -8, r: 14, class: "sfill" }, g); line(g, 0, -8, 8, -16); sv("circle", { cx: 0, cy: -8, r: 1.5, class: "dot" }, g); },
    },
    JUNCTION: {
      label: "Junction (tee)", prefix: "J", kind: "junction", box: [-22, -22, 44, 44],
      term: { w: [-20, 0, -1, 0], e: [20, 0, 1, 0], n: [0, -20, 0, -1], s: [0, 20, 0, 1] },
      draw(g) { line(g, -20, 0, 20, 0, "stub"); line(g, 0, -20, 0, 20, "stub"); sv("circle", { cx: 0, cy: 0, r: 5, class: "dot" }, g); },
    },
    FLANGE: {
      label: "Station flange", prefix: "FL", kind: "boundary", box: [-12, -16, 34, 32],
      term: { port: [20, 0, 1, 0] },
      draw(g) { sv("rect", { x: -8, y: -14, width: 6, height: 28, class: "sfill" }, g); line(g, -2, 0, 20, 0, "stub"); },
    },
  };

  const DEFAULT_BANDS = [
    { up_to: 10, color: "#6e7681" }, { up_to: 100, color: "#3fb950" }, { up_to: 200, color: "#d29922" }, { up_to: 1e9, color: "#f0883e" },
  ];

  // ---------------------------------------------------------------- geometry
  function rotv(x, y, deg) {
    switch (((deg % 360) + 360) % 360) {
      case 0: return [x, y];
      case 90: return [-y, x];
      case 180: return [-x, -y];
      default: return [y, -x];
    }
  }
  function termInfo(node, name) {
    const def = SYM[node.type];
    const t = def && def.term[name];
    if (!t) return null;
    const [rx, ry] = rotv(t[0], t[1], node.rotation);
    const [dx, dy] = rotv(t[2], t[3], node.rotation);
    return { x: node.x + rx, y: node.y + ry, dx, dy };
  }
  function rotBox(box, rot) {
    const [x, y, w, hh] = box;
    const pts = [[x, y], [x + w, y], [x, y + hh], [x + w, y + hh]].map(([a, b]) => rotv(a, b, rot));
    const xs = pts.map((p) => p[0]), ys = pts.map((p) => p[1]);
    return { x0: Math.min(...xs), x1: Math.max(...xs), y0: Math.min(...ys), y1: Math.max(...ys) };
  }
  function pipeWidth(p) {
    let inches = 4;
    const m = /^(\d+(?:\.\d)?)-inch$/.exec(p.nominal_diameter || "");
    if (m) inches = parseFloat(m[1]);
    const dn = /^DN(\d+)$/.exec(p.nominal_diameter || "");
    if (dn) inches = parseInt(dn[1], 10) / 25.4;
    return clamp(Math.round(2 + inches * 0.55), 3, 12);
  }
  function pipePoints(doc, p) {
    const a = doc.components.find((n) => n.id === p.from_node_id), b = doc.components.find((n) => n.id === p.to_node_id);
    if (!a || !b) return null;
    const s = termInfo(a, p.from_terminal), e = termInfo(b, p.to_terminal);
    if (!s || !e) return null;
    return [{ x: s.x, y: s.y }, ...(p.path_points || []), { x: e.x, y: e.y }];
  }
  function labelFor(n) {
    if (n.display && n.display.label) return n.display.label;
    return n.type === "JUNCTION" ? "" : n.id;
  }
  // Bounding box of everything drawn (with room for the texts).
  function contentBounds(doc) {
    if (!doc || !doc.components.length) return { x0: 0, y0: 0, x1: doc ? doc.grid_bounds.width : 1200, y1: doc ? doc.grid_bounds.height : 600 };
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const n of doc.components) {
      const def = SYM[n.type]; if (!def) continue;
      const rb = rotBox(def.box, n.rotation);
      x0 = Math.min(x0, n.x + rb.x0); x1 = Math.max(x1, n.x + rb.x1 + 60);   // + room for side labels
      y0 = Math.min(y0, n.y + rb.y0 - 24); y1 = Math.max(y1, n.y + rb.y1 + 24);
    }
    for (const p of doc.pipelines) for (const q of p.path_points || []) { x0 = Math.min(x0, q.x); x1 = Math.max(x1, q.x); y0 = Math.min(y0, q.y); y1 = Math.max(y1, q.y); }
    return { x0: x0 - 30, y0: y0 - 30, x1: x1 + 30, y1: y1 + 30 };
  }

  // ---------------------------------------------------------------- drawing
  // Both return the element references applyLive() needs.
  function drawPipe(layer, doc, p, selected) {
    const pts = pipePoints(doc, p);
    if (!pts) return null;
    const g = sv("g", { class: "pipe-g" + (selected ? " sel" : ""), "data-pipe": p.id }, layer);
    const str = pts.map((q) => `${q.x},${q.y}`).join(" ");
    sv("polyline", { points: str, class: "pipe-hit" }, g);
    const body = sv("polyline", { points: str, class: "pipe", "stroke-width": pipeWidth(p) }, g);
    const flow = sv("polyline", { points: str, class: "pipe-flow" }, g);
    // diameter label on the longest segment
    let best = 0, mid = null;
    for (let i = 0; i < pts.length - 1; i++) {
      const len = Math.abs(pts[i + 1].x - pts[i].x) + Math.abs(pts[i + 1].y - pts[i].y);
      if (len > best) { best = len; mid = { x: (pts[i].x + pts[i + 1].x) / 2, y: (pts[i].y + pts[i + 1].y) / 2, horiz: pts[i].y === pts[i + 1].y }; }
    }
    if (mid && best >= 100) {
      const t = sv("text", { x: mid.x + (mid.horiz ? 0 : 14), y: mid.y + (mid.horiz ? -9 : 4), class: "pipe-lbl" }, g);
      t.textContent = p.nominal_diameter;
    }
    return { g, body, flow };
  }

  function drawNode(layer, n, selected) {
    const def = SYM[n.type];
    const g = sv("g", { class: "node" + (selected ? " sel" : ""), "data-node": n.id, transform: `translate(${n.x},${n.y})` }, layer);
    const rot = sv("g", { transform: `rotate(${n.rotation})` }, g);
    if (!def) {
      sv("rect", { x: -20, y: -20, width: 40, height: 40, fill: "none", stroke: "#f85149", "stroke-dasharray": "4 3" }, rot);
      const t = sv("text", { x: 0, y: 4, class: "idl" }, g); t.textContent = "? " + n.type;
      return { g, val: null, idl: t, def: null };
    }
    const [bx, by, bw, bh] = def.box;
    sv("rect", { class: "halo", x: bx - 4, y: by - 4, width: bw + 8, height: bh + 8 }, rot);
    const symG = sv("g", { class: "sym" }, rot);
    def.draw(symG);
    if (n.rotation) {   // keep text inside symbols upright
      for (const t of symG.querySelectorAll("text")) t.setAttribute("transform", `rotate(${-n.rotation} ${t.getAttribute("x")} ${t.getAttribute("y")})`);
    }
    sv("rect", { class: "hit", x: bx, y: by, width: bw, height: bh }, rot);
    const rb = rotBox(def.box, n.rotation);
    let idl, val;
    if (def.kind === "instrument") {   // instruments sit on a pipe stub: id beside the body, value above it
      idl = sv("text", { class: "idl", x: rb.x1 + 6, y: (rb.y0 + rb.y1) / 2 - 4 }, g); idl.style.textAnchor = "start";
      val = sv("text", { class: "val", x: 0, y: rb.y0 - 7 }, g);
    } else {
      idl = sv("text", { class: "idl", x: 0, y: rb.y1 + 13 }, g);
      val = sv("text", { class: "val", x: 0, y: rb.y0 - 7 }, g);
    }
    idl.textContent = labelFor(n);
    return { g, val, idl, def };
  }

  // ---------------------------------------------------------------- live state
  function cmp(v, op, ref) {
    switch (op) {
      case ">": return v > ref; case ">=": return v >= ref; case "<": return v < ref;
      case "<=": return v <= ref; case "==": return v === ref; case "!=": return v !== ref;
    }
    return false;
  }
  function ruleColor(rules, v) {
    for (const r of rules || []) if (cmp(v, r.op, r.value)) return r.color;
    return null;
  }
  // tag_id -> { sev, unacked } for the active alarms.
  function alarmIndex(alarms) {
    const idx = new Map();
    for (const a of alarms) {
      const unacked = a.state === "UNACK_ALARM" || a.state === "UNACK_RTN";
      const cur = idx.get(a.tag_id) || { sev: -1, unacked: false };
      idx.set(a.tag_id, { sev: Math.max(cur.sev, a.severity_code), unacked: cur.unacked || unacked });
    }
    return idx;
  }
  function nodeAlarm(n, idx) {
    let best = null;
    for (const t of [n.bindings.primary_tag, n.bindings.state_tag]) {
      const a = t && idx.get(t);
      if (a && (!best || a.sev > best.sev || (a.sev === best.sev && a.unacked))) best = a;
    }
    return best;
  }
  const fmtNum = (v, dec) => v.toFixed(dec === undefined || dec === null ? 1 : dec);

  // Position of a valve node from its live state tag.
  //   known=false: no data or bad quality;  open: valve is open;  tripped: a slam-shut that closed.
  function valveState(n, liveTags) {
    const t = n.bindings.state_tag && liveTags.get(n.bindings.state_tag);
    if (!t || t.state === undefined || t.quality !== "GOOD") return { known: false, tag: t || null };
    const open = (n.display && n.display.invert_state) ? !t.state : t.state;
    return { known: true, open, tripped: !open && n.type === "SLAM_SHUT_VALVE", tag: t };
  }

  // Applies the live state to the drawn elements. With live=false everything is reset.
  function applyLive(doc, nodeEls, pipeEls, liveTags, alarmIdx, live) {
    for (const n of doc ? doc.components : []) {
      const e = nodeEls.get(n.id);
      if (!e || !e.def) continue;
      e.g.classList.remove("v-open", "v-closed", "v-tripped", "v-unknown", "alm-ack", "alm-unack");
      e.val.textContent = ""; e.val.classList.remove("bad"); e.val.style.fill = "";
      if (!live) continue;
      const d = n.display || {};
      let color = null;
      if (e.def.kind === "valve") {
        const v = valveState(n, liveTags);
        if (!v.known) {
          e.g.classList.add("v-unknown"); e.val.textContent = v.tag ? "NO DATA" : "UNBOUND"; e.val.classList.add("bad");
        } else {
          e.g.classList.add(v.open ? "v-open" : "v-closed");
          if (v.tripped) e.g.classList.add("v-tripped");
          e.val.textContent = v.open ? "OPEN" : (v.tripped ? "TRIPPED" : "CLOSED");
          color = ruleColor(d.color_rules, v.tag.state ? 1 : 0);
        }
      } else if (n.bindings.primary_tag || n.bindings.state_tag) {
        const pt = n.bindings.primary_tag && liveTags.get(n.bindings.primary_tag);
        const stt = n.bindings.state_tag && liveTags.get(n.bindings.state_tag);
        if (n.bindings.primary_tag) {
          if (!pt || pt.value === undefined || pt.quality !== "GOOD") { e.val.textContent = (d.label_prefix ? d.label_prefix + " " : "") + "---"; e.val.classList.add("bad"); }
          else {
            const unit = d.unit || pt.unit || "";
            e.val.textContent = `${d.label_prefix ? d.label_prefix + " " : ""}${fmtNum(pt.value, d.decimals)}${unit ? " " + unit : ""}`;
            color = ruleColor(d.color_rules, pt.value);
          }
        } else if (stt && stt.state !== undefined) {
          const on = d.invert_state ? !stt.state : stt.state;
          e.val.textContent = (d.label_prefix ? d.label_prefix + " " : "") + (on ? "ON" : "OFF");
          color = ruleColor(d.color_rules, stt.state ? 1 : 0);
        }
      }
      if (color) e.val.style.fill = color;
      const al = nodeAlarm(n, alarmIdx);
      if (al) e.g.classList.add(al.unacked ? "alm-unack" : "alm-ack");
    }
    for (const p of doc ? doc.pipelines : []) {
      const e = pipeEls.get(p.id);
      if (!e) continue;
      e.g.classList.remove("flowing", "alm-ack", "alm-unack");
      e.body.style.stroke = "";
      if (!live || !p.bound_pressure_tag) continue;
      const t = liveTags.get(p.bound_pressure_tag);
      if (!t || t.value === undefined || t.quality !== "GOOD") { e.body.style.stroke = cssVar("--bad"); continue; }
      const bands = (doc.pressure_bands && doc.pressure_bands.length ? doc.pressure_bands : DEFAULT_BANDS);
      let col = bands[bands.length - 1].color;
      for (const b of bands) if (t.value <= b.up_to) { col = b.color; break; }
      e.body.style.stroke = col;
      if (t.value > 1) e.g.classList.add("flowing");
      const al = alarmIdx.get(p.bound_pressure_tag);
      if (al) e.g.classList.add(al.unacked ? "alm-unack" : "alm-ack");
    }
  }

  return { SVGNS, SYM, DEFAULT_BANDS, sv, rotv, rotBox, termInfo, pipePoints, pipeWidth, labelFor, contentBounds,
    drawPipe, drawNode, ruleColor, alarmIndex, nodeAlarm, valveState, applyLive };
})();
