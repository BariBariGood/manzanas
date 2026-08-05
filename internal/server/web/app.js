// manzanasd dashboard: read-only fleet + journal view over the daemon's own
// v0 API (same origin). Poll is the baseline; /v0/ws events are used purely
// as invalidation signals to re-fetch the lists.
(() => {
  "use strict";

  const POLL_MS = 5000;
  const $ = (id) => document.getElementById(id);

  // ---- helpers ----------------------------------------------------------

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };

  const chip = (text, kind) => el("span", "chip " + (kind || ""), text);

  const fmtTime = (iso) => {
    if (!iso) return "";
    const d = new Date(iso);
    return isNaN(d) ? String(iso) : d.toLocaleString();
  };

  const fmtCountdown = (ms) => {
    if (ms <= 0) return "expired";
    const s = Math.floor(ms / 1000);
    if (s < 60) return s + "s";
    const m = Math.floor(s / 60);
    if (m < 60) return m + "m " + (s % 60) + "s";
    return Math.floor(m / 60) + "h " + (m % 60) + "m";
  };

  const getJSON = async (url) => {
    const r = await fetch(url);
    if (!r.ok) throw new Error(url + " -> " + r.status);
    return r.json();
  };

  const postJSON = async (url) => {
    const r = await fetch(url, { method: "POST" });
    let j = {};
    try { j = await r.json(); } catch { /* error shape is best-effort */ }
    if (!r.ok) throw new Error(j.message || (url + " -> " + r.status));
    return j;
  };

  // ---- dash controls (gated by --dash-readonly) --------------------------

  // Controls stay hidden until /v0/dash/config confirms they are enabled.
  let controlsEnabled = false;

  async function loadDashConfig() {
    try {
      const j = await getJSON("/v0/dash/config");
      controlsEnabled = !j.readonly;
    } catch {
      controlsEnabled = false;
    }
  }

  // confirmPost guards every mutation behind a confirm dialog, POSTs, and
  // refreshes the fleet view; failures surface as an alert.
  function confirmPost(question, url) {
    if (!window.confirm(question)) return;
    postJSON(url)
      .then(() => refreshFleet())
      .catch((e) => window.alert("failed: " + e.message));
  }

  const ctlButton = (label, question, url) => {
    const b = el("button", "ctl", label);
    b.onclick = () => confirmPost(question, url);
    return b;
  };

  // ---- header / health ---------------------------------------------------

  async function refreshHealth() {
    const h = $("health");
    try {
      const j = await getJSON("/v0/healthz");
      h.textContent = j.ok ? "healthy · " + (j.version || "") : "unhealthy";
      h.classList.toggle("err", !j.ok);
    } catch {
      h.textContent = "daemon unreachable";
      h.classList.add("err");
    }
  }

  // ---- fleet tables ------------------------------------------------------

  let lastLeases = [];

  // activeLeaseTargets is the set of target UDIDs held by an active lease,
  // derived from the last leases fetch (renderLeases runs first).
  const activeLeaseTargets = () => new Set(
    lastLeases.filter((l) => l.state === "active" && l.target_udid)
      .map((l) => l.target_udid));

  function renderTargets(targets) {
    const leased = activeLeaseTargets();
    const tbody = $("targets").querySelector("tbody");
    tbody.replaceChildren();
    $("targets-empty").hidden = targets.length > 0;
    let parked = 0, booted = 0;
    for (const t of targets) {
      if (t.warm) parked++;
      else if (t.state === "Booted") booted++;
      const tr = el("tr");
      tr.appendChild(el("td")).appendChild(el("b", "", t.name || ""));
      tr.appendChild(el("td", "", t.runtime || ""));
      tr.appendChild(el("td", "", t.device_type || ""));
      const st = tr.appendChild(el("td"));
      st.appendChild(chip(t.state || "?", (t.state || "").toLowerCase()));
      if (t.warm) st.appendChild(document.createTextNode(" ")),
        st.appendChild(chip("parked", "parked"));
      const lbl = tr.appendChild(el("td"));
      for (const l of t.labels || []) lbl.appendChild(chip(l, "label"));
      tr.appendChild(el("td")).appendChild(el("code", "", t.udid || ""));
      const act = tr.appendChild(el("td"));
      const btn = el("button", "", "live view");
      // Parked sims are frozen (streaming would hang) and non-booted sims
      // have nothing to show.
      if (t.warm || t.state !== "Booted") {
        btn.disabled = true;
        btn.title = t.warm ? "parked in warm pool" : "not booted";
        act.appendChild(btn);
      } else {
        const a = el("a");
        a.href = "/view/" + encodeURIComponent(t.udid);
        a.target = "_blank";
        a.appendChild(btn);
        act.appendChild(a);
      }
      const ctl = tr.appendChild(el("td", "controls"));
      if (controlsEnabled && !t.warm && t.kind !== "device") {
        const held = leased.has(t.udid);
        if (t.state === "Shutdown" && !held) {
          ctl.appendChild(ctlButton("boot",
            "Boot " + (t.name || t.udid) + "?",
            "/v0/dash/targets/" + encodeURIComponent(t.udid) + "/boot"));
        }
        if (t.state === "Booted" && !held) {
          ctl.appendChild(ctlButton("shutdown",
            "Shut down " + (t.name || t.udid) + "?",
            "/v0/dash/targets/" + encodeURIComponent(t.udid) + "/shutdown"));
        }
        if (t.recording) {
          ctl.appendChild(ctlButton("stop rec",
            "Stop the live recording on " + (t.name || t.udid) + "? " +
            "The recording is finalized and ingested into its run's journal.",
            "/v0/dash/targets/" + encodeURIComponent(t.udid) + "/recording/stop"));
        }
      }
      if (t.recording) {
        const st = tr.children[3];
        st.appendChild(document.createTextNode(" "));
        st.appendChild(chip("rec", "rec"));
      }
      tbody.appendChild(tr);
    }
    $("pool-summary").textContent =
      targets.length + " targets · " + booted + " booted · " + parked + " parked";
  }

  function renderLeases(leases) {
    lastLeases = leases;
    const tbody = $("leases").querySelector("tbody");
    tbody.replaceChildren();
    $("leases-empty").hidden = leases.length > 0;
    const now = Date.now();
    for (const l of leases) {
      const tr = el("tr");
      tr.appendChild(el("td")).appendChild(el("b", "", l.agent_id || ""));
      tr.appendChild(el("td")).appendChild(el("code", "", l.target_udid || "—"));
      tr.appendChild(el("td")).appendChild(chip(l.state || "?", l.state || ""));
      tr.appendChild(el("td", "", l.purpose || ""));
      const exp = tr.appendChild(el("td"));
      if (l.expires_at) {
        const ms = new Date(l.expires_at).getTime() - now;
        exp.appendChild(chip(fmtCountdown(ms), ms < 60000 ? "expiring" : ""));
        exp.dataset.expiresAt = l.expires_at;
      } else {
        exp.textContent = "—";
      }
      tr.appendChild(el("td", "", l.queue_position ? "#" + l.queue_position : ""));
      const ctl = tr.appendChild(el("td", "controls"));
      if (controlsEnabled && l.state === "active" && l.target_udid) {
        ctl.appendChild(ctlButton("release",
          "Release " + (l.agent_id || "this agent") + "'s lease on " +
          l.target_udid + "? Its post-lease reset (if any) still runs.",
          "/v0/dash/targets/" + encodeURIComponent(l.target_udid) + "/release"));
      }
      tbody.appendChild(tr);
    }
  }

  // Tick lease countdowns between refreshes without re-fetching.
  setInterval(() => {
    const now = Date.now();
    for (const td of $("leases").querySelectorAll("td[data-expires-at]")) {
      const ms = new Date(td.dataset.expiresAt).getTime() - now;
      const c = td.querySelector(".chip");
      if (c) {
        c.textContent = fmtCountdown(ms);
        c.classList.toggle("expiring", ms < 60000);
      }
    }
  }, 1000);

  let refreshing = false;
  let fleetDirty = false;
  async function refreshFleet() {
    if (refreshing) {
      fleetDirty = true; // coalesce: re-run once after the in-flight fetch
      return;
    }
    refreshing = true;
    try {
      const [t, l] = await Promise.all([
        getJSON("/v0/targets"),
        getJSON("/v0/leases"),
      ]);
      renderLeases(l.leases || []);
      renderTargets(t.targets || []);
      renderMultiview(t.targets || []);
    } catch {
      /* header health shows reachability; keep the last good tables */
    } finally {
      refreshing = false;
      if (fleetDirty) {
        fleetDirty = false;
        setTimeout(refreshFleet, INVALIDATE_MS);
      }
    }
  }

  // ---- live updates: WS invalidation with poll fallback -------------------

  const INVALIDATING = new Set(["lease.granted", "lease.expired", "target.state"]);
  const INVALIDATE_MS = 750; // coalesce event bursts into ~1 refresh per interval
  let wsLive = false;
  let wsBackoff = 1000;
  let invalidateTimer = 0;

  // Debounced invalidation: a burst of fleet events triggers at most one
  // refresh per INVALIDATE_MS (targets listing shells out on the host).
  function invalidateFleet() {
    if (invalidateTimer) return;
    invalidateTimer = setTimeout(() => {
      invalidateTimer = 0;
      refreshFleet();
    }, INVALIDATE_MS);
  }

  function setLive(live) {
    wsLive = live;
    const ind = $("live-indicator");
    ind.textContent = live ? "live" : "polling";
    ind.classList.toggle("live", live);
  }

  function connectWS() {
    let ws;
    try {
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      ws = new WebSocket(proto + "//" + location.host + "/v0/ws");
    } catch {
      scheduleReconnect();
      return;
    }
    ws.onopen = () => {
      wsBackoff = 1000;
      setLive(true);
      refreshFleet(); // catch anything missed while disconnected
    };
    ws.onmessage = (m) => {
      try {
        const env = JSON.parse(m.data);
        if (env.event && INVALIDATING.has(env.event)) invalidateFleet();
      } catch { /* ignore unparseable frames */ }
    };
    ws.onclose = () => { setLive(false); scheduleReconnect(); };
    ws.onerror = () => { try { ws.close(); } catch { /* already closed */ } };
  }

  function scheduleReconnect() {
    setTimeout(connectWS, wsBackoff);
    wsBackoff = Math.min(wsBackoff * 2, 30000);
  }

  // Poll is always on (5s): it is the fallback when the WS is down and a
  // safety net for transitions that emit no event (e.g. pool park/thaw).
  setInterval(() => { refreshHealth(); refreshFleet(); }, POLL_MS);

  // ---- multiviewer ---------------------------------------------------------
  // Streams are strictly opt-in per tile: activating a tile negotiates a
  // stream via POST /v0/streams and attaches the MJPEG URL. Streams are
  // shared per target (Open is idempotent), so deactivating only detaches
  // this tile's viewer — never DELETE, which would cut off other viewers
  // of the same target; the no-viewer linger stops capture shortly after
  // the last viewer detaches.

  // udids of tiles with an attached stream.
  const mvActive = new Set();

  const mvStreamable = (t) =>
    t.kind !== "device" && !t.warm && t.state === "Booted";

  async function mvStart(udid, tile) {
    // Mark the tile wanted before the async negotiation so a stop during
    // the round-trip (toggle click, "stop all") retracts the intent and
    // the resolved offer is discarded instead of attaching anyway.
    mvActive.add(udid);
    const status = tile.querySelector(".mv-status");
    status.textContent = "negotiating…";
    status.classList.remove("err");
    try {
      const r = await fetch("/v0/streams", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ udid, format: "mjpeg" }),
      });
      const offer = await r.json();
      if (!r.ok) {
        mvActive.delete(udid);
        status.textContent = offer.code === "stream_limit"
          ? "stream limit reached (--stream-max-streams); stop another tile first"
          : "error: " + (offer.message || r.status);
        status.classList.add("err");
        return;
      }
      // Reconciled away or stopped mid-negotiation: drop the offer.
      if (!tile.isConnected || !mvActive.has(udid)) return;
      tile.classList.add("live");
      const img = tile.querySelector("img");
      // Negotiation only reserves the stream; the <img> attach itself can
      // still fail (viewer limit, or the stream got reaped in between).
      img.onerror = () => {
        mvStop(udid, tile);
        status.textContent = "stream attach failed (viewer limit, or the stream was reaped)";
        status.classList.add("err");
      };
      img.src = offer.mjpeg_url;
      img.hidden = false;
      status.textContent = "live · " + offer.fps + " fps";
    } catch (e) {
      mvActive.delete(udid);
      status.textContent = "error: " + e;
      status.classList.add("err");
    }
  }

  function mvStop(udid, tile) {
    mvActive.delete(udid);
    if (tile) {
      tile.classList.remove("live");
      const img = tile.querySelector("img");
      img.onerror = null; // detaching must not look like an attach failure
      img.hidden = true;
      img.removeAttribute("src");
      tile.querySelector(".mv-status").textContent = "click to start stream";
      tile.querySelector(".mv-status").classList.remove("err");
    }
  }

  const mvTile = (udid) => document.querySelector('.mv-tile[data-udid="' + udid + '"]');

  // renderMultiview reconciles the tile grid with the latest target list:
  // tiles come and go with target state, but an active tile's <img> is
  // never touched (replacing it would drop the open MJPEG connection).
  function renderMultiview(targets) {
    const grid = $("mv-grid");
    const want = targets.filter(mvStreamable);
    const wantIDs = new Set(want.map((t) => t.udid));
    for (const tile of [...grid.children]) {
      const udid = tile.dataset.udid;
      if (!wantIDs.has(udid)) {
        // Detach this tile's viewer before dropping it: removing the
        // <img> from the DOM does not reliably abort an MJPEG fetch.
        mvStop(udid, tile); // target gone or no longer streamable
        tile.remove();
      }
    }
    for (const t of want) {
      let tile = mvTile(t.udid);
      if (!tile) {
        tile = el("div", "mv-tile");
        tile.dataset.udid = t.udid;
        const head = tile.appendChild(el("div", "mv-name"));
        head.appendChild(el("b", "", t.name || t.udid));
        head.appendChild(el("span", "meta", " " + (t.runtime || "")));
        const img = tile.appendChild(el("img"));
        img.alt = "live stream of " + (t.name || t.udid);
        img.hidden = true;
        tile.appendChild(el("div", "mv-status", "click to start stream"));
        tile.onclick = () => {
          if (mvActive.has(t.udid)) mvStop(t.udid, tile);
          else mvStart(t.udid, tile);
        };
        grid.appendChild(tile);
      }
    }
    $("mv-empty").hidden = grid.children.length > 0;
  }

  $("mv-start-all").onclick = () => {
    for (const tile of $("mv-grid").children) {
      if (!mvActive.has(tile.dataset.udid)) mvStart(tile.dataset.udid, tile);
    }
  };
  $("mv-stop-all").onclick = () => {
    for (const tile of $("mv-grid").children) {
      if (mvActive.has(tile.dataset.udid)) mvStop(tile.dataset.udid, tile);
    }
  };

  // ---- journal browser -----------------------------------------------------

  const PAGE_LIMIT = 100;
  let runNextSeq = 0;
  let runID = "";
  let runGen = 0; // bumped by openRun so stale page responses are discarded

  async function refreshRuns() {
    const tbody = $("runs").querySelector("tbody");
    try {
      const j = await getJSON("/v0/journal");
      const runs = j.runs || [];
      tbody.replaceChildren();
      $("runs-empty").textContent = "no journal runs";
      $("runs-empty").hidden = runs.length > 0;
      for (const r of runs) {
        const tr = el("tr");
        tr.appendChild(el("td")).appendChild(el("code", "", r.run_id || ""));
        tr.appendChild(el("td", "", (r.meta && r.meta.agent_id) || ""));
        tr.appendChild(el("td", "", (r.meta && (r.meta.target_name || r.meta.target_udid)) || ""));
        tr.appendChild(el("td", "", (r.meta && r.meta.purpose) || ""));
        tr.appendChild(el("td", "", String(r.last_seq ?? "")));
        tr.appendChild(el("td", "", fmtTime(r.updated_at)));
        tr.onclick = () => openRun(r.run_id);
        tbody.appendChild(tr);
      }
    } catch {
      tbody.replaceChildren();
      $("runs-empty").hidden = false;
      $("runs-empty").textContent = "journal unavailable";
    }
  }

  function renderEntry(e, id) {
    const box = el("div", "entry");
    const head = box.appendChild(el("div", "head"));
    head.appendChild(el("span", "", "#" + ((e.ref && e.ref.seq) ?? "?")));
    head.appendChild(el("b", "", e.kind || ""));
    const p = e.payload || {};
    if (p.action) head.appendChild(el("span", "", p.action));
    if (p.status) head.appendChild(el("span", p.status === "ok" ? "" : "err", p.status));
    if (p.ts) head.appendChild(el("span", "", fmtTime(p.ts)));
    if (p.error) box.appendChild(el("pre", "", String(p.error)));
    if (p.params && Object.keys(p.params).length) {
      box.appendChild(el("pre", "", JSON.stringify(p.params)));
    }
    for (const a of p.artifacts || []) {
      const path = a.path || "";
      const url = "/v0/journal/" + encodeURIComponent(id) + "/" + path;
      if (/\.(png|jpe?g|gif|webp)$/i.test(path)) {
        const img = el("img");
        img.loading = "lazy";
        img.src = url;
        img.alt = path;
        const link = el("a");
        link.href = url;
        link.target = "_blank";
        link.appendChild(img);
        box.appendChild(link);
      } else {
        const link = el("a", "", path);
        link.href = url;
        box.appendChild(link);
      }
    }
    return box;
  }

  async function loadRunPage() {
    const id = runID;
    const gen = runGen;
    const from = runNextSeq;
    const j = await getJSON("/v0/journal/" + encodeURIComponent(id) +
      "?from_seq=" + from + "&limit=" + PAGE_LIMIT);
    if (gen !== runGen) return;
    if (from === 0 && j.meta) {
      const m = j.meta;
      $("run-meta").textContent = [
        m.agent_id, m.target_name || m.target_udid, m.device_type, m.runtime,
        m.purpose, fmtTime(m.created_at),
      ].filter(Boolean).join(" · ");
    }
    const wrap = $("run-entries");
    for (const e of j.entries || []) wrap.appendChild(renderEntry(e, id));
    runNextSeq = j.next_seq || 0;
    $("run-more").hidden = runNextSeq === 0;
  }

  async function openRun(id) {
    runID = id;
    runGen++;
    runNextSeq = 0;
    $("journal-runs-pane").hidden = true;
    $("journal-run-pane").hidden = false;
    $("run-title").textContent = id;
    $("run-export").href = "/v0/journal/" + encodeURIComponent(id) + "/export.md";
    $("run-meta").textContent = "";
    $("run-entries").replaceChildren();
    $("run-more").hidden = true;
    try {
      await loadRunPage();
    } catch {
      $("run-meta").textContent = "failed to load run";
    }
  }

  let loadingPage = false;
  $("run-more").onclick = () => {
    if (loadingPage) return;
    loadingPage = true;
    loadRunPage().catch(() => {}).finally(() => { loadingPage = false; });
  };
  $("run-back").onclick = () => {
    $("journal-run-pane").hidden = true;
    $("journal-runs-pane").hidden = false;
    refreshRuns();
  };

  // ---- tabs -----------------------------------------------------------------

  for (const btn of document.querySelectorAll(".tab")) {
    btn.onclick = () => {
      for (const b of document.querySelectorAll(".tab")) {
        b.classList.toggle("active", b === btn);
      }
      $("tab-fleet").hidden = btn.dataset.tab !== "fleet";
      $("tab-multiview").hidden = btn.dataset.tab !== "multiview";
      $("tab-journal").hidden = btn.dataset.tab !== "journal";
      if (btn.dataset.tab === "journal" && $("journal-run-pane").hidden) {
        refreshRuns();
      }
    };
  }

  // ---- boot -----------------------------------------------------------------

  refreshHealth();
  loadDashConfig().then(refreshFleet);
  connectWS();
})();
