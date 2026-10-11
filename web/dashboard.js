"use strict";
/*
 * Operator dashboard: live values, alarms, trend and the interactive station
 * schematic (HMI) with the equipment control faceplate.
 *
 * Control path: faceplate -> POST /api/v1/control/command (two-step confirmation
 * in the UI, "confirmed": true in the request) -> gateway -> core -> FEP -> Modbus.
 */
(function () {
  const params = new URLSearchParams(location.search);
  const URL_TOKEN = params.get("token") || "";
  const $ = (id) => document.getElementById(id);
  const SESSION_KEY = "scada.session";
  const SEV = ["INFO", "LOW", "MEDIUM", "HIGH", "CRITICAL"];
  const SEV_CLASS = ["info", "low", "med", "high", "crit"];
  const SEV_COLOR = ["--info", "--low", "--med", "--high", "--crit"];
  const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();

  // Gauge scales for the three analog cards.
  const GAUGES = {
    p_in:  { suffix: "_P_IN",  title: "Inlet pressure",  min: 245, max: 255, dec: 1 },
    p_out: { suffix: "_P_OUT", title: "Outlet pressure", min: 56,  max: 64,  dec: 1 },
    temp:  { suffix: "_TEMP",  title: "Gas temperature", min: 10,  max: 20,  dec: 1 }
  };
  const LEAK_SUFFIX = "_LEAK";

  const tags = new Map();    // tag_id -> tag json
  const alarms = new Map();  // alarm_id -> alarm json (active only)
  let lastMsgAt = 0;
  let station = "";

  // ------------------------------------------------------------------ helpers
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function toast(msg, isErr) {
    const t = $("toast");
    t.textContent = msg; t.className = isErr ? "err" : ""; t.style.display = "block";
    clearTimeout(toast.t); toast.t = setTimeout(() => { t.style.display = "none"; }, 4000);
  }
  function fmtTime(ms) {
    if (!ms) return "-";
    return new Date(ms).toLocaleTimeString([], { hour12: false });
  }
  function ago(ms) {
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s ago";
    return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m ago";
  }
  function tagBySuffix(suffix) {
    for (const [id, t] of tags) if (id.endsWith(suffix)) return t;
    return null;
  }
  function severityForTag(tagId) {
    let max = -1;
    for (const a of alarms.values()) if (a.tag_id === tagId && a.severity_code > max) max = a.severity_code;
    return max;
  }

  // ------------------------------------------------------------------ sign-in / session
  // The session lives in sessionStorage (this tab only) and is sent as a bearer token.
  let session = null;
  try { session = JSON.parse(sessionStorage.getItem(SESSION_KEY) || "null"); } catch (e) { /* storage blocked */ }
  let me = { user: "", role: "", authenticated: false, auth_required: false, control_allowed: false, can_set_setpoints: false, lab_mode: false };
  const token = () => (session && session.token) || URL_TOKEN;

  function authHeaders(extra) {
    const h = Object.assign({}, extra || {});
    if (token()) h["Authorization"] = "Bearer " + token();
    return h;
  }
  // Fetch helper: returns { ok, status, json }. A 401 opens the sign-in dialog.
  async function api(method, path, body) {
    const opts = { method, headers: authHeaders(body !== undefined ? { "Content-Type": "application/json" } : {}), cache: "no-store" };
    if (body !== undefined) opts.body = JSON.stringify(body);
    const r = await fetch(path, opts);
    let json = {};
    try { json = await r.json(); } catch (e) { /* empty body */ }
    if (r.status === 401 && !$("signin").open) openSignIn(true);
    return { ok: r.ok, status: r.status, json };
  }

  const userInput = $("user");
  try { userInput.value = localStorage.getItem("scada.user") || ""; } catch (e) { /* storage blocked */ }
  userInput.addEventListener("change", () => { try { localStorage.setItem("scada.user", userInput.value.trim()); } catch (e) { /* ignore */ } });

  function operatorName() { return me.user || userInput.value.trim(); }

  function renderWho() {
    const b = $("who");
    if (me.user) {
      b.textContent = `${me.user} · ${me.role}`; b.title = "Signed in";
      userInput.value = me.user; userInput.readOnly = true;
      $("btnSignIn").textContent = "Sign out";
    } else {
      b.textContent = me.lab_mode ? "lab mode (unverified)" : (me.control_allowed ? "operator" : "viewing only");
      b.title = me.auth_required ? "A token is in use without a personal account" : "Sign in to operate equipment";
      userInput.readOnly = false;
      $("btnSignIn").textContent = "Sign in";
    }
  }
  async function loadWhoami() {
    const r = await api("GET", "/api/v1/control/whoami");
    if (r.ok) { me = Object.assign(me, r.json); renderWho(); refreshFaceplate(true); }
    return r;
  }

  const signinDlg = $("signin");
  function openSignIn(required) {
    $("siErr").textContent = "";
    $("signinSub").textContent = required
      ? "This gateway requires authentication. Sign in with your operator account."
      : "Sign in with your operator account to operate equipment.";
    $("siCancel").style.display = required ? "none" : "";
    $("siToken").value = "";
    if (!signinDlg.open) signinDlg.showModal();
    $("siUser").focus();
  }
  $("btnSignIn").onclick = () => {
    if (me.user) { try { sessionStorage.removeItem(SESSION_KEY); } catch (e) { /* ignore */ } location.reload(); return; }
    openSignIn(false);
  };
  $("siCancel").onclick = () => signinDlg.close();
  signinDlg.addEventListener("cancel", (e) => { if ($("siCancel").style.display === "none") e.preventDefault(); });
  $("signinForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = $("siUser").value.trim(), tok = $("siToken").value;
    if (!name || !tok) { $("siErr").textContent = "Enter your account and token."; return; }
    const prev = session;
    session = { user: name, token: tok };
    const r = await fetch("/api/v1/control/whoami", { headers: authHeaders(), cache: "no-store" });
    const j = await r.json().catch(() => ({}));
    if (!r.ok || !j.user) {
      session = prev;
      $("siErr").textContent = r.status === 401 ? "Invalid account or token." : (j.error || "Sign-in failed. The token does not belong to a personal account.");
      return;
    }
    if (j.user !== name) { session = prev; $("siErr").textContent = "That token belongs to a different account."; return; }
    try { sessionStorage.setItem(SESSION_KEY, JSON.stringify(session)); } catch (err) { /* storage blocked: session lasts until reload */ }
    signinDlg.close();
    me = Object.assign(me, j); renderWho();
    toast("Signed in as " + me.user + " (" + me.role + ")");
    reconnect(); loadHmi(true); loadCaps(); loadTrend(); startLoops();
  });

  // ------------------------------------------------------------------ cards
  const ARC_LEN = 100;
  function buildGaugeCard(card, g) {
    card.textContent = "";
    const name = el("div", "name");
    name.append(el("span", "", g.title), el("span", "tag", "—"));
    const val = el("div", "value"); val.append(document.createTextNode("--"), el("span", "unit", ""));
    const svgNS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(svgNS, "svg");
    svg.setAttribute("viewBox", "0 0 160 104"); svg.setAttribute("class", "gauge");
    const mk = (cls, d) => { const p = document.createElementNS(svgNS, "path"); p.setAttribute("d", d); p.setAttribute("class", cls); p.setAttribute("pathLength", ARC_LEN); return p; };
    const d = "M 14 82 A 66 66 0 0 1 146 82";
    const track = mk("track", d), fill = mk("fill", d);
    fill.setAttribute("stroke-dasharray", "0 " + ARC_LEN);
    const t0 = document.createElementNS(svgNS, "text"); t0.setAttribute("x", 14); t0.setAttribute("y", 101); t0.setAttribute("text-anchor", "middle"); t0.textContent = g.min;
    const t1 = document.createElementNS(svgNS, "text"); t1.setAttribute("x", 146); t1.setAttribute("y", 101); t1.setAttribute("text-anchor", "end"); t1.textContent = g.max;
    svg.append(track, fill, t0, t1);
    const meta = el("div", "meta"); meta.append(el("span", "q", "—"), el("span", "ts", "—"));
    card.append(name, val, svg, meta);
    card._ref = { tagEl: name.lastChild, valEl: val.firstChild, unitEl: val.lastChild, fill, q: meta.firstChild, ts: meta.lastChild };
  }
  function buildLeakCard(card) {
    card.textContent = "";
    const name = el("div", "name"); name.append(el("span", "", "Gas leak detector"), el("span", "tag", "—"));
    const leak = el("div", "leak"); leak.append(el("div", "lamp"), el("div", "txt", "--"));
    const meta = el("div", "meta"); meta.append(el("span", "q", "—"), el("span", "ts", "—"));
    card.append(name, leak, meta);
    card._ref = { tagEl: name.lastChild, leak, txt: leak.lastChild, q: meta.firstChild, ts: meta.lastChild };
  }
  for (const k of Object.keys(GAUGES)) buildGaugeCard($("card-" + k), GAUGES[k]);
  buildLeakCard($("card-leak"));

  function setQuality(ref, t) {
    const good = t.quality === "GOOD";
    ref.q.textContent = good ? "Quality: GOOD" : "Quality: " + t.quality.replaceAll("_", " ");
    ref.q.className = "q " + (good ? "q-good" : "q-bad");
    ref.ts.textContent = fmtTime(t.timestamp_ms);
  }
  function setCardSeverity(card, tagId) {
    const s = severityForTag(tagId);
    card.className = "card" + (s >= 0 ? " s-" + SEV_CLASS[s] : "");
  }
  function renderCards() {
    for (const k of Object.keys(GAUGES)) {
      const g = GAUGES[k], card = $("card-" + k), ref = card._ref, t = tagBySuffix(g.suffix);
      if (!t) continue;
      ref.tagEl.textContent = t.tag_id;
      const good = t.quality === "GOOD" && t.value !== undefined;
      ref.valEl.textContent = t.value === undefined ? "--" : t.value.toFixed(g.dec);
      ref.unitEl.textContent = t.unit || "";
      const frac = t.value === undefined ? 0 : Math.min(1, Math.max(0, (t.value - g.min) / (g.max - g.min)));
      ref.fill.setAttribute("stroke-dasharray", (frac * ARC_LEN).toFixed(2) + " " + ARC_LEN);
      const s = severityForTag(t.tag_id);
      ref.fill.style.stroke = !good ? cssVar("--bad") : s >= 0 ? cssVar(SEV_COLOR[s]) : cssVar("--ok");
      setQuality(ref, t); setCardSeverity(card, t.tag_id);
    }
    const lt = tagBySuffix(LEAK_SUFFIX), lc = $("card-leak"), lr = lc._ref;
    if (lt) {
      lr.tagEl.textContent = lt.tag_id;
      const on = lt.state === true, bad = lt.quality !== "GOOD";
      lr.leak.className = "leak" + (bad ? " bad" : on ? " on" : "");
      lr.txt.textContent = bad ? "NO DATA" : on ? "LEAK DETECTED" : "NORMAL";
      setQuality(lr, lt); setCardSeverity(lc, lt.tag_id);
    }
    const first = [...tags.keys()][0];
    if (first) { station = first.replace(/_(P_IN|P_OUT|P_OUT_SP|TEMP|LEAK|INLET_VLV|OUTLET_VLV|SLAM_SHUT)$/, ""); $("station").textContent = station; }
  }

  // ------------------------------------------------------------------ alarms
  function renderAlarms() {
    const list = [...alarms.values()].sort((a, b) => b.severity_code - a.severity_code || a.trigger_time_ms - b.trigger_time_ms);
    const body = $("alarmbody"); body.textContent = "";
    if (!list.length) {
      const tr = el("tr"), td = el("td", "empty", "No active alarms"); td.colSpan = 8; tr.append(td); body.append(tr);
    }
    let unacked = 0;
    for (const a of list) {
      const needsAck = a.state === "UNACK_ALARM" || a.state === "UNACK_RTN";
      if (needsAck) unacked++;
      const tr = el("tr");
      const sev = el("td"); sev.append(el("span", "sevtag sev-" + a.severity_code, a.severity));
      const st = el("td", "state-" + a.state, a.state.replaceAll("_", " "));
      const act = el("td"); const btn = el("button", "", "ACK");
      btn.disabled = !needsAck; btn.setAttribute("aria-label", "Acknowledge " + a.alarm_id);
      btn.addEventListener("click", () => acknowledge(a.alarm_id, btn));
      act.append(btn);
      tr.append(sev, el("td", "", a.alarm_id), el("td", "msg", a.message), el("td", "", a.trigger_value.toFixed(1)),
        st, el("td", "", fmtTime(a.trigger_time_ms)), el("td", "", a.ack_by_user || ""), act);
      body.append(tr);
    }
    const banner = $("banner"), top = list.length ? list.reduce((m, a) => Math.max(m, a.severity_code), 0) : -1;
    banner.className = list.length ? "alarm" + (unacked ? " unacked" : "") : "";
    banner.firstChild.style.background = top >= 0 ? cssVar(SEV_COLOR[top]) : cssVar("--ok");
    $("bannertxt").textContent = list.length
      ? list.length + " active alarm" + (list.length > 1 ? "s" : "") + " · " + unacked + " unacknowledged · highest " + SEV[top]
      : "No active alarms";
    renderCards();
  }
  async function acknowledge(id, btn) {
    const user = operatorName();
    if (!user) { toast("Enter your operator name first (or sign in)", true); userInput.focus(); return; }
    btn.disabled = true;
    try {
      const r = await api("POST", "/api/v1/alarms/ack", { alarm_id: id, user });
      if (!r.ok) { toast("Acknowledge failed: " + (r.json.error || r.status), true); btn.disabled = false; return; }
      toast("Acknowledged " + id);
    } catch (e) { toast("Acknowledge failed: " + e.message, true); btn.disabled = false; }
  }
  function applyAlarm(a) {
    if (a.active) alarms.set(a.alarm_id, a); else alarms.delete(a.alarm_id);
  }

  // ------------------------------------------------------------------ WebSocket
  let ws = null, retry = 1000, wsTimer = null;
  function setConn(state, text) { $("conn").className = "badge " + state; $("conntxt").textContent = text; }
  function connect() {
    clearTimeout(wsTimer);
    const url = (location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws/telemetry" + (token() ? "?token=" + encodeURIComponent(token()) : "");
    setConn("offline", "CONNECTING");
    const sock = new WebSocket(url);
    ws = sock;
    sock.onopen = () => { retry = 1000; setConn("live", "LIVE"); };
    sock.onmessage = (ev) => {
      let m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      lastMsgAt = Date.now();
      if (m.type === "snapshot") {
        tags.clear(); alarms.clear();
        m.tags.forEach((t) => tags.set(t.tag_id, t));
        m.alarms.forEach(applyAlarm);
        if (m.error) toast("Core data incomplete: " + m.error, true);
        renderAlarms(); updateHmi();
      } else if (m.type === "telemetry") {
        m.tags.forEach((t) => {
          const prev = tags.get(t.tag_id);
          const next = Object.assign({}, prev, t);
          // remember when a digital tag last changed state
          if (t.type === "digital" && (!prev || prev.state !== t.state)) next.last_change_ms = t.timestamp_ms;
          tags.set(t.tag_id, next);
        });
        renderCards(); updateHmi();
      } else if (m.type === "alarm") {
        applyAlarm(m.alarm); renderAlarms(); updateHmi();
      }
    };
    sock.onclose = () => {
      if (ws !== sock) return;   // replaced on purpose
      setConn("offline", "OFFLINE · retry in " + Math.round(retry / 1000) + "s");
      wsTimer = setTimeout(connect, retry); retry = Math.min(retry * 2, 10000);
    };
    sock.onerror = () => { try { sock.close(); } catch (e) { /* ignore */ } };
  }
  function reconnect() { const s = ws; ws = null; if (s) { try { s.close(); } catch (e) { /* ignore */ } } retry = 1000; connect(); }
  setInterval(() => {
    $("clock").textContent = new Date().toLocaleTimeString([], { hour12: false });
    if (ws && ws.readyState === 1) {
      const stale = Date.now() - lastMsgAt > 5000;
      setConn(stale ? "stale" : "live", stale ? "NO DATA" : "LIVE");
    }
    if (fp.node && $("faceplate").open) renderFaceplateStatus();
  }, 1000);

  // ------------------------------------------------------------------ HMI (deployed schematic)
  const hmiSvg = $("hmi");
  let hmiDoc = null, hmiVersion = 0, hmiNodes = new Map(), hmiPipes = new Map(), hmiAlarmIdx = new Map();
  let hmiQueued = false;

  function updateHmi() {
    if (!hmiDoc || hmiQueued) return;
    hmiQueued = true;
    requestAnimationFrame(() => {
      hmiQueued = false;
      hmiAlarmIdx = Schematic.alarmIndex(alarms.values());
      Schematic.applyLive(hmiDoc, hmiNodes, hmiPipes, tags, hmiAlarmIdx, true);
      if ($("faceplate").open) refreshFaceplate(false);
    });
  }

  function hmiMessage(html) {
    hmiSvg.toggleAttribute("hidden", true); $("hmiMsg").hidden = false; $("hmiMsg").innerHTML = html; $("hmiLegend").textContent = ""; $("hmiInfo").textContent = "";
  }

  function buildHmi(doc, st) {
    hmiDoc = doc; hmiVersion = st.deployed_version;
    hmiSvg.textContent = "";
    hmiNodes = new Map(); hmiPipes = new Map();
    const b = Schematic.contentBounds(doc);
    hmiSvg.setAttribute("viewBox", `${b.x0} ${b.y0} ${b.x1 - b.x0} ${b.y1 - b.y0}`);
    const pipesLayer = Schematic.sv("g", {}, hmiSvg), nodesLayer = Schematic.sv("g", {}, hmiSvg);
    for (const p of doc.pipelines) { const r = Schematic.drawPipe(pipesLayer, doc, p, false); if (r) hmiPipes.set(p.id, r); }
    for (const n of doc.components) {
      const r = Schematic.drawNode(nodesLayer, n, false);
      hmiNodes.set(n.id, r);
      if (r.def && (n.bindings.state_tag || n.bindings.primary_tag)) {
        r.g.classList.add("interactive");
        r.g.setAttribute("tabindex", "0"); r.g.setAttribute("role", "button");
        r.g.setAttribute("aria-label", `${n.display.label || n.id}: open the equipment faceplate`);
      }
    }
    hmiSvg.toggleAttribute("hidden", false); $("hmiMsg").hidden = true;
    $("hmiInfo").textContent = `${doc.name} · layout v${hmiVersion} deployed${st.deployed_by ? " by " + st.deployed_by : ""}`;
    renderLegend(doc);
    updateHmi();
  }

  function renderLegend(doc) {
    const lg = $("hmiLegend"); lg.textContent = "";
    const bands = doc.pressure_bands && doc.pressure_bands.length ? doc.pressure_bands : Schematic.DEFAULT_BANDS;
    let lo = 0;
    for (const bd of bands) {
      const item = el("span"); const sw = el("i"); sw.style.background = bd.color;
      item.append(sw, document.createTextNode(bd.up_to >= 1e8 ? `> ${lo} PSI` : `${lo}–${bd.up_to} PSI`));
      lg.append(item); lo = bd.up_to;
    }
    for (const [cls, col, txt] of [["sq", "var(--ok)", "valve open"], ["sq", "var(--crit)", "closed / tripped"], ["sq", "var(--bad)", "no data"]]) {
      const item = el("span"); const sw = el("i", cls); sw.style.borderColor = col;
      item.append(sw, document.createTextNode(txt)); lg.append(item);
    }
    lg.append(el("span", "", "Click an element to open its faceplate."));
  }

  async function loadHmi(force) {
    try {
      const list = await api("GET", "/api/v1/topology");
      if (!list.ok) { if (list.status !== 401) hmiMessage("The station layout could not be loaded (" + (list.json.error || list.status) + ")."); return; }
      const deployed = list.json.stations.filter((s) => s.deployed_version > 0);
      if (!deployed.length) {
        hmiMessage('No station layout has been deployed yet. Design one in the <a href="/studio.html">Engineering Studio</a> and click <em>Deploy to Runtime</em>.');
        hmiDoc = null; hmiVersion = 0; return;
      }
      // prefer the station whose tags we are receiving
      const pick = deployed.find((s) => [...tags.keys()].some((t) => t.startsWith(s.station_id + "_"))) || deployed[0];
      if (!force && hmiDoc && hmiDoc.station_id === pick.station_id && hmiVersion === pick.deployed_version) return;
      const r = await api("GET", "/api/v1/topology/" + encodeURIComponent(pick.station_id) + "?version=deployed");
      if (!r.ok) { hmiMessage("The deployed layout could not be loaded (" + (r.json.error || r.status) + ")."); return; }
      buildHmi(r.json.topology, r.json.status);
    } catch (e) { hmiMessage("The station layout could not be loaded: " + e.message); }
  }

  // ------------------------------------------------------------------ control capabilities
  let caps = new Map();      // tag_id -> { actions, min_value, max_value, unit }
  let stationOnline = false;
  async function loadCaps() {
    try {
      const r = await api("GET", "/api/v1/control/capabilities");
      if (!r.ok) return;
      caps = new Map(); stationOnline = false;
      for (const s of r.json.stations) { if (s.online) stationOnline = true; for (const c of s.capabilities) caps.set(c.tag_id, c); }
      refreshFaceplate(true);
    } catch (e) { /* the core may be down; keep the old capabilities */ }
  }

  // ------------------------------------------------------------------ faceplate
  const fpDlg = $("faceplate");
  // view: "main" | "confirm" | "sending" | "done"
  const fp = { node: null, view: "main", pending: null, result: null, spDraft: "", sig: "" };

  hmiSvg.addEventListener("click", (e) => {
    const g = e.target.closest(".node.interactive");
    if (g) openFaceplate(g.dataset.node);
  });
  hmiSvg.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const g = e.target.closest && e.target.closest(".node.interactive");
    if (g) { e.preventDefault(); openFaceplate(g.dataset.node); }
  });

  function nodeTags(n) { return [n.bindings.state_tag, n.bindings.primary_tag].filter(Boolean); }
  function nodeTitle(n) { return n.display && n.display.label ? `${n.display.label} (${n.id})` : n.id; }

  function openFaceplate(nodeId) {
    const n = hmiDoc && hmiDoc.components.find((x) => x.id === nodeId);
    if (!n) return;
    fp.node = n; fp.view = "main"; fp.pending = null; fp.result = null; fp.spDraft = ""; fp.sig = "";
    $("fpTitle").textContent = nodeTitle(n);
    $("fpSub").textContent = Schematic.SYM[n.type] ? Schematic.SYM[n.type].label : n.type;
    $("fpResult").hidden = true; $("fpConfirm").hidden = true; $("fpRecentWrap").textContent = "";
    if (!fpDlg.open) fpDlg.showModal();
    refreshFaceplate(true);
    loadRecent();
  }
  $("fpClose").onclick = () => { if (fp.view !== "sending") fpDlg.close(); };
  fpDlg.addEventListener("close", () => { if (fp.view !== "sending") { fp.node = null; fp.view = "main"; } });
  fpDlg.addEventListener("cancel", (e) => {
    if (fp.view === "sending") { e.preventDefault(); return; }      // never abandon a command in flight
    if (fp.view === "confirm") { e.preventDefault(); cancelConfirm(); }
  });

  // What the operator may do with this element right now.
  function faceplateActions(n) {
    const out = [];
    const why = [];
    if (!stationOnline) why.push("The station's FEP is offline: commands cannot be delivered.");
    if (!me.control_allowed) why.push(me.control_denied_reason || "Sign in with an operator account to operate equipment.");
    const ready = stationOnline && me.control_allowed;

    if (n.type === "BALL_VALVE" || n.type === "SLAM_SHUT_VALVE") {
      const tag = n.bindings.state_tag, c = tag && caps.get(tag), v = Schematic.valveState(n, tags);
      if (c) {
        const has = (a) => c.actions.includes(a);
        if (has("CMD_OPEN_VALVE")) out.push({ key: "open", label: "OPEN", cls: "", tag, action: "CMD_OPEN_VALVE", enabled: ready && v.known && !v.open });
        if (has("CMD_CLOSE_VALVE")) out.push({ key: "close", label: "CLOSE", cls: "warn", tag, action: "CMD_CLOSE_VALVE", enabled: ready && v.known && v.open });
        if (has("CMD_RESET_TRIP")) out.push({ key: "reset", label: "RESET", cls: "danger", tag, action: "CMD_RESET_TRIP", enabled: ready && v.known && !v.open });
        if (!v.known) why.push("The valve position is unknown (no data), so commands are blocked.");
      } else if (tag) why.push("This valve has no remote control.");
    } else {
      const tag = n.bindings.primary_tag, c = tag && caps.get(tag);
      if (c && c.actions.includes("CMD_SET_SETPOINT")) {
        const canSet = ready && me.can_set_setpoints;
        if (ready && !me.can_set_setpoints) why.push("Changing setpoints needs the engineer role.");
        out.push({ key: "setpoint", label: "SET", cls: "", tag, action: "CMD_SET_SETPOINT", enabled: canSet, cap: c });
      }
    }
    return { buttons: out, why };
  }

  function refreshFaceplate(structural) {
    if (!fp.node || !fpDlg.open) return;
    renderFaceplateStatus();
    const { buttons, why } = faceplateActions(fp.node);
    const sig = fp.view + "|" + buttons.map((b) => b.key + b.enabled).join(",") + "|" + why.join("|");
    if (structural || sig !== fp.sig) { fp.sig = sig; renderFaceplateActions(buttons, why); }
  }

  function renderFaceplateStatus() {
    const n = fp.node; if (!n) return;
    const kv = $("fpStatus"); kv.textContent = "";
    const add = (k, v) => { const dd = el("dd"); if (v instanceof Node) dd.append(v); else dd.textContent = v; kv.append(el("dt", "", k), dd); };
    if (n.type === "BALL_VALVE" || n.type === "SLAM_SHUT_VALVE") {
      const v = Schematic.valveState(n, tags);
      const pill = el("span", "state-pill " + (!v.known ? "" : v.open ? "open" : v.tripped ? "tripped" : "closed"),
        !v.known ? (v.tag ? "NO DATA" : "UNBOUND") : v.open ? "OPEN" : v.tripped ? "TRIPPED" : "CLOSED");
      add("Position", pill);
      if (v.tag) {
        add("State tag", v.tag.tag_id);
        add("Quality", v.tag.quality.replaceAll("_", " "));
        add("Last changed", v.tag.last_change_ms ? `${fmtTime(v.tag.last_change_ms)} (${ago(v.tag.last_change_ms)})` : "no change since monitoring started");
        add("Last update", fmtTime(v.tag.timestamp_ms));
      } else if (n.bindings.state_tag) add("State tag", n.bindings.state_tag + " (no data yet)");
    } else {
      for (const t of nodeTags(n)) {
        const v = tags.get(t);
        add("Tag", t);
        if (!v) { add("Value", "no data yet"); continue; }
        add(v.type === "digital" ? "State" : "Value", v.type === "digital" ? String(v.state) : `${v.value !== undefined ? v.value.toFixed(2) : "—"} ${v.unit || ""}`);
        add("Quality", v.quality.replaceAll("_", " "));
        add("Last update", fmtTime(v.timestamp_ms));
        if (v.type === "digital" && v.last_change_ms) add("Last changed", `${fmtTime(v.last_change_ms)} (${ago(v.last_change_ms)})`);
      }
    }
    const box = $("fpAlarms"); box.textContent = "";
    for (const a of alarms.values()) {
      if (!nodeTags(n).includes(a.tag_id)) continue;
      const d = el("div", "fp-alarm");
      d.append(el("span", "sevtag sev-" + a.severity_code, a.severity), document.createTextNode(`${a.alarm_id} · ${a.state.replaceAll("_", " ")}: ${a.message}`));
      box.append(d);
    }
  }

  function renderFaceplateActions(buttons, why) {
    const box = $("fpActions"); box.textContent = "";
    const note = $("fpNote"); note.textContent = "";
    $("fpConfirm").hidden = fp.view !== "confirm";
    $("fpResult").hidden = !(fp.view === "done" || fp.view === "sending");
    if (fp.view === "confirm") { renderConfirm(); return; }
    if (fp.view === "sending") return;
    if (fp.view === "done") {
      const r = $("fpResult"); r.hidden = false;
      const back = el("button", "secondary", "Back"); back.onclick = () => { fp.view = "main"; fp.result = null; refreshFaceplate(true); };
      box.append(back);
      return;
    }
    $("fpResult").hidden = true;
    for (const b of buttons) {
      if (b.key === "setpoint") {
        const wrap = el("div", "sp");
        const inp = el("input"); inp.type = "number"; inp.step = "0.1"; inp.min = b.cap.min_value; inp.max = b.cap.max_value;
        inp.id = "fpSetpoint"; inp.setAttribute("aria-label", "New setpoint"); inp.value = fp.spDraft;
        const cur = tags.get(b.tag); if (!inp.value && cur && cur.value !== undefined) inp.placeholder = cur.value.toFixed(1);
        inp.disabled = !b.enabled;
        inp.addEventListener("input", () => { fp.spDraft = inp.value; });
        const btn = el("button", "", "SET"); btn.disabled = !b.enabled; btn.id = "fpBtn-setpoint";
        btn.onclick = () => {
          const v = parseFloat(inp.value);
          if (!isFinite(v)) { toast("Enter a setpoint value", true); inp.focus(); return; }
          if (v < b.cap.min_value || v > b.cap.max_value) { toast(`Setpoint must be between ${b.cap.min_value} and ${b.cap.max_value} ${b.cap.unit || ""}`, true); inp.focus(); return; }
          askConfirm(b, v);
        };
        wrap.append(inp, el("span", "fp-note", b.cap.unit || ""), btn);
        box.append(wrap);
        note.textContent = `Allowed range ${b.cap.min_value}–${b.cap.max_value} ${b.cap.unit || ""}.`;
      } else {
        const btn = el("button", b.cls, b.label); btn.id = "fpBtn-" + b.key; btn.disabled = !b.enabled;
        btn.onclick = () => askConfirm(b);
        box.append(btn);
      }
    }
    if (!buttons.length && !why.length) note.textContent = "This element is read-only (no remote commands).";
    if (why.length) note.textContent = (note.textContent ? note.textContent + " " : "") + why.join(" ");
  }

  // ---- two-step confirmation
  function confirmText(b, value) {
    const n = fp.node;
    switch (b.action) {
      case "CMD_OPEN_VALVE": return `Are you sure you want to OPEN valve ${n.id}?`;
      case "CMD_CLOSE_VALVE": return `Are you sure you want to CLOSE valve ${n.id}?`;
      case "CMD_RESET_TRIP": return `Are you sure you want to RESET the trip of slam-shut valve ${n.id}?`;
      default: {
        const cur = tags.get(b.tag);
        return `Are you sure you want to change the setpoint of ${n.id} from ${cur && cur.value !== undefined ? cur.value.toFixed(1) : "?"} to ${value.toFixed(1)} ${b.cap.unit || ""}?`;
      }
    }
  }
  function askConfirm(b, value) {
    fp.pending = { b, value }; fp.view = "confirm"; fp.sig = ""; refreshFaceplate(true);
  }
  function renderConfirm() {
    const { b, value } = fp.pending;
    const box = $("fpConfirm"); box.hidden = false; box.textContent = "";
    box.append(el("div", "", confirmText(b, value)));
    box.append(el("div", "fp-note", `Operator: ${operatorName() || "?"}. The command is sent to the field immediately and recorded in the audit log.`));
    const act = $("fpActions"); act.textContent = "";
    const cancel = el("button", "secondary big", "Cancel"); cancel.id = "fpCancel"; cancel.onclick = cancelConfirm;
    const yes = el("button", (b.cls || "") + " big", `Yes, ${b.key === "setpoint" ? "set " + value.toFixed(1) : b.label}`); yes.id = "fpConfirmYes";
    yes.onclick = () => sendCommand(b, value);
    act.append(cancel, yes);
    cancel.focus();      // the safe choice has the focus
  }
  function cancelConfirm() { fp.view = "main"; fp.pending = null; fp.sig = ""; refreshFaceplate(true); }

  async function sendCommand(b, value) {
    const n = fp.node;
    fp.view = "sending"; fp.sig = ""; refreshFaceplate(true);
    const res = $("fpResult"); res.hidden = false; res.className = "fp-result"; res.textContent = "Sending the command to the field…";
    const body = { station_id: hmiDoc.station_id, tag_id: b.tag, action: b.action, confirmed: true };
    if (me.user) body.user = me.user; else body.user = operatorName();
    if (b.action === "CMD_SET_SETPOINT") body.setpoint_value = value;
    let r;
    try { r = await api("POST", "/api/v1/control/command", body); }
    catch (e) { r = { ok: false, status: 0, json: { status: "NOT_SENT", error: "The gateway could not be reached: " + e.message } }; }
    const j = r.json || {};
    fp.view = "done"; fp.pending = null; fp.sig = "";
    refreshFaceplate(true);
    const out = $("fpResult"); out.hidden = false;
    if (r.ok && j.success) {
      out.className = "fp-result ok";
      out.textContent = `✔ ${j.message || "Executed"}${j.latency_ms !== undefined ? ` (${j.latency_ms} ms)` : ""}`;
      toast(`${n.id}: command executed`);
    } else {
      out.className = "fp-result fail";
      const st = j.status ? j.status.replaceAll("_", " ") : "FAILED";
      out.textContent = `✖ ${st}: ${j.message || j.error || "HTTP " + r.status}` +
        (j.status === "TIMEOUT" ? " Check the live state before trying again." : "");
    }
    loadRecent();
  }

  async function loadRecent() {
    const n = fp.node, box = $("fpRecentWrap");
    if (!n || !me.user) { box.textContent = ""; return; }
    const tagIds = nodeTags(n);
    const rows = [];
    for (const t of tagIds) {
      const r = await api("GET", "/api/v1/control/audit?limit=5&tag_id=" + encodeURIComponent(t));
      if (r.ok) rows.push(...r.json.entries);
    }
    rows.sort((a, b) => b.time_ms - a.time_ms);
    box.textContent = "";
    if (!rows.length) return;
    box.append(el("div", "fp-note", "Recent commands for this element"));
    const tb = el("table", "fp-recent");
    for (const e of rows.slice(0, 5)) {
      const tr = el("tr");
      tr.append(el("td", "", fmtTime(e.time_ms)), el("td", "", e.operator || "?"),
        el("td", "", e.action.replace("CMD_", "") + (e.setpoint_value !== undefined ? " " + e.setpoint_value : "")), el("td", "st-" + e.status, e.status));
      tb.append(tr);
    }
    box.append(tb);
  }

  // ------------------------------------------------------------------ trend chart (canvas)
  const cv = $("chart"), ctx = cv.getContext("2d"), tip = $("tip"), msg = $("chartmsg");
  let trend = null;  // {start_ms, end_ms, series:[{tag_id, points}]}
  const SERIES_COLORS = ["#58a6ff", "#f0883e"];
  const PAD = { l: 58, r: 58, t: 14, b: 30 };

  function sizeCanvas() {
    const r = cv.getBoundingClientRect(), dpr = window.devicePixelRatio || 1;
    cv.width = Math.round(r.width * dpr); cv.height = Math.round(r.height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { w: r.width, h: r.height };
  }
  function niceRange(min, max) {
    if (!isFinite(min) || !isFinite(max)) return [0, 1];
    if (max - min < 1e-6) { min -= 0.5; max += 0.5; }
    const pad = (max - min) * 0.08; return [min - pad, max + pad];
  }
  function drawChart(hoverX) {
    const { w, h } = sizeCanvas();
    ctx.clearRect(0, 0, w, h);
    if (!trend) return;
    const x0 = PAD.l, x1 = w - PAD.r, y0 = PAD.t, y1 = h - PAD.b;
    const tmin = trend.start_ms, tmax = trend.end_ms;
    const X = (t) => x0 + (t - tmin) / (tmax - tmin) * (x1 - x0);
    const grid = cssVar("--border"), muted = cssVar("--muted");
    ctx.font = "11px system-ui, sans-serif"; ctx.textBaseline = "middle";

    const scales = trend.series.slice(0, 2).map((s) => {
      let lo = Infinity, hi = -Infinity;
      s.points.forEach((p) => { lo = Math.min(lo, p.min); hi = Math.max(hi, p.max); });
      const [a, b] = niceRange(lo, hi);
      return { lo: a, hi: b, Y: (v) => y1 - (v - a) / (b - a) * (y1 - y0) };
    });

    ctx.strokeStyle = grid; ctx.lineWidth = 1; ctx.fillStyle = muted;
    for (let i = 0; i <= 4; i++) {
      const y = y0 + (y1 - y0) * i / 4;
      ctx.beginPath(); ctx.moveTo(x0, y); ctx.lineTo(x1, y); ctx.stroke();
      scales.forEach((sc, k) => {
        const v = sc.hi - (sc.hi - sc.lo) * i / 4;
        ctx.fillStyle = SERIES_COLORS[k]; ctx.textAlign = k === 0 ? "right" : "left";
        ctx.fillText(v.toFixed(1), k === 0 ? x0 - 8 : x1 + 8, y);
      });
    }
    ctx.fillStyle = muted; ctx.textAlign = "center";
    const longRange = (tmax - tmin) > 3 * 3600e3;
    const nt = w < 520 ? 3 : 5;
    for (let i = 0; i <= nt; i++) {
      const t = tmin + (tmax - tmin) * i / nt, x = X(t);
      ctx.beginPath(); ctx.moveTo(x, y1); ctx.lineTo(x, y1 + 4); ctx.stroke();
      const d = new Date(t);
      ctx.fillText(d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: longRange ? undefined : "2-digit", hour12: false }), x, y1 + 16);
    }

    trend.series.slice(0, 2).forEach((s, k) => {
      const sc = scales[k], col = SERIES_COLORS[k];
      if (!s.points.length) return;
      ctx.save(); ctx.beginPath(); ctx.rect(x0, y0, x1 - x0, y1 - y0); ctx.clip();
      ctx.globalAlpha = 0.16; ctx.fillStyle = col; ctx.beginPath();
      s.points.forEach((p, i) => { const x = X(p.t); i ? ctx.lineTo(x, sc.Y(p.max)) : ctx.moveTo(x, sc.Y(p.max)); });
      for (let i = s.points.length - 1; i >= 0; i--) ctx.lineTo(X(s.points[i].t), sc.Y(s.points[i].min));
      ctx.closePath(); ctx.fill();
      ctx.globalAlpha = 1; ctx.strokeStyle = col; ctx.lineWidth = 1.75; ctx.lineJoin = "round"; ctx.beginPath();
      s.points.forEach((p, i) => { const x = X(p.t), y = sc.Y(p.avg); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      ctx.stroke();
      if (s.points.length < 60) { ctx.fillStyle = col; s.points.forEach((p) => { ctx.beginPath(); ctx.arc(X(p.t), sc.Y(p.avg), 2.5, 0, 6.3); ctx.fill(); }); }
      ctx.restore();
    });

    if (hoverX != null && hoverX >= x0 && hoverX <= x1) {
      const t = tmin + (hoverX - x0) / (x1 - x0) * (tmax - tmin);
      ctx.strokeStyle = muted; ctx.setLineDash([4, 4]); ctx.beginPath(); ctx.moveTo(hoverX, y0); ctx.lineTo(hoverX, y1); ctx.stroke(); ctx.setLineDash([]);
      const rows = [];
      trend.series.slice(0, 2).forEach((s, k) => {
        let best = null;
        s.points.forEach((p) => { if (!best || Math.abs(p.t - t) < Math.abs(best.t - t)) best = p; });
        if (best) {
          ctx.fillStyle = SERIES_COLORS[k]; ctx.beginPath(); ctx.arc(X(best.t), scales[k].Y(best.avg), 4, 0, 6.3); ctx.fill();
          rows.push({ k, s, best });
        }
      });
      tip.textContent = "";
      if (rows.length) {
        tip.append(el("div", "", new Date(rows[0].best.t).toLocaleString([], { hour12: false })));
        rows.forEach((r) => {
          const ln = el("div", "", r.s.tag_id.replace(/^.*_(P_IN|P_OUT)$/, "$1") + ": " + r.best.avg.toFixed(2) + "  (min " + r.best.min.toFixed(2) + " / max " + r.best.max.toFixed(2) + ")");
          ln.style.color = SERIES_COLORS[r.k]; tip.append(ln);
        });
        tip.style.display = "block";
        const tw = tip.offsetWidth; tip.style.left = Math.min(Math.max(hoverX + 12, 4), w - tw - 4) + "px"; tip.style.top = (y0 + 6) + "px";
      } else tip.style.display = "none";
    } else tip.style.display = "none";
  }
  cv.addEventListener("mousemove", (e) => drawChart(e.clientX - cv.getBoundingClientRect().left));
  cv.addEventListener("mouseleave", () => drawChart(null));
  new ResizeObserver(() => drawChart(null)).observe($("chartwrap"));

  async function loadTrend() {
    const pin = tagBySuffix("_P_IN"), pout = [...tags.keys()].find((k) => k.endsWith("_P_OUT"));
    if (!pin || !pout) { msg.textContent = "Waiting for tags…"; msg.style.display = "flex"; return; }
    $("lg0").textContent = pin.tag_id; $("lg1").textContent = pout;
    const secs = parseInt($("range").value, 10), end = Date.now(), start = end - secs * 1000;
    const q = new URLSearchParams({ start: start, end: end, buckets: 240 });
    q.append("tag_id", pin.tag_id); q.append("tag_id", pout);
    try {
      const r = await api("GET", "/api/v1/historian/trend?" + q);
      if (!r.ok) throw new Error(r.json.error || ("HTTP " + r.status));
      trend = r.json;
      const n = trend.series.reduce((a, s) => a + s.points.length, 0);
      if (n === 0) { msg.textContent = "No historian data in this range. Is the historian running?"; msg.style.display = "flex"; }
      else msg.style.display = "none";
    } catch (e) {
      trend = null; msg.textContent = "Trend unavailable: " + e.message; msg.style.display = "flex";
    }
    drawChart(null);
  }
  $("range").addEventListener("change", loadTrend);
  setInterval(loadTrend, 10000);
  const waitTags = setInterval(() => { if (tagBySuffix("_P_IN")) { clearInterval(waitTags); loadTrend(); } }, 300);

  // ------------------------------------------------------------------ start
  let loopsStarted = false;
  function startLoops() {
    if (loopsStarted) return;
    loopsStarted = true;
    setInterval(() => loadHmi(false), 20000);
    setInterval(loadCaps, 15000);
  }
  (async function init() {
    renderWho();
    const w = await loadWhoami().catch(() => ({ ok: false }));
    if (w && w.status === 401) return;      // the sign-in dialog is open; it restarts everything after sign-in
    connect();
    loadHmi(true);
    loadCaps();
    startLoops();
  })();
})();
