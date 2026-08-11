// manzanas-broker dashboard: read-only aggregated fleet view over the
// broker's own endpoints (same origin): /v0/fleet/hosts for per-host
// health, /v0/targets and /v0/leases for the federated, host-annotated
// listings. The broker has no WS surface, so a 5s poll is the update
// mechanism. Live streams are negotiated against — and served directly
// from — each target's owning daemon (host_addr); media never flows
// through the broker.
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

  // ---- auth token (--auth-token broker/daemons) --------------------------
  // A one-time ?token= in the page URL is stored in localStorage and
  // stripped; thereafter every API call carries it as a bearer header. The
  // same token is presented to daemons for direct stream negotiation and
  // view links (media never flows through the broker), so a fleet using
  // the dash's live view should share one token across broker and daemons.

  const TOKEN_KEY = "manzanas_token";
  let memToken = ""; // fallback when localStorage is blocked (private mode)
  (() => {
    const u = new URL(location.href);
    const t = u.searchParams.get("token");
    if (t) {
      memToken = t;
      try { localStorage.setItem(TOKEN_KEY, t); } catch { /* private mode */ }
      u.searchParams.delete("token");
      history.replaceState(null, "", u);
    }
  })();

  const token = () => {
    try { return localStorage.getItem(TOKEN_KEY) || memToken; } catch { return memToken; }
  };

  const authHeaders = () =>
    token() ? { Authorization: "Bearer " + token() } : {};

  const withToken = (url) => token()
    ? url + (url.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(token())
    : url;

  // On a 401, ask for the token once per page load and reload with it.
  let tokenPrompted = false;
  const promptToken = () => {
    if (tokenPrompted) return;
    tokenPrompted = true;
    const t = window.prompt("This broker requires an auth token (--auth-token). Paste it to continue:");
    if (t) {
      // Reload with ?token= so the init path picks it up even when
      // localStorage is blocked; it is stored and stripped on load.
      const u = new URL(location.href);
      u.searchParams.set("token", t);
      location.replace(u);
    }
  };

  const getJSON = async (url) => {
    const r = await fetch(url, { headers: authHeaders() });
    if (r.status === 401) promptToken();
    if (!r.ok) throw new Error(url + " -> " + r.status);
    return r.json();
  };

  // hostAddrs maps host name -> daemon base URL, from the last
  // /v0/fleet/hosts fetch; live-view links and stream negotiation need it.
  let hostAddrs = {};

  // ---- header / health ---------------------------------------------------

  async function refreshHealth() {
    const h = $("health");
    try {
      const j = await getJSON("/v0/healthz");
      h.textContent = j.ok
        ? "healthy · " + (j.build || j.version || "") + " · " + (j.hosts ?? "?") + " hosts"
        : "unhealthy";
      h.classList.toggle("err", !j.ok);
    } catch {
      h.textContent = "broker unreachable";
      h.classList.add("err");
    }
  }

  // ---- hosts table --------------------------------------------------------

  function renderHosts(hosts) {
    hostAddrs = {};
    const tbody = $("hosts").querySelector("tbody");
    tbody.replaceChildren();
    $("hosts-empty").hidden = hosts.length > 0;
    let up = 0, targets = 0;
    // Version skew: highlight any daemon build that differs from the
    // majority build across up hosts.
    const buildCounts = {};
    for (const h of hosts) {
      if (h.up && h.build) buildCounts[h.build] = (buildCounts[h.build] || 0) + 1;
    }
    const majorityBuild = Object.keys(buildCounts)
      .sort((a, b) => buildCounts[b] - buildCounts[a])[0] || "";
    for (const h of hosts) {
      hostAddrs[h.name] = h.addr;
      if (h.up) { up++; targets += h.targets || 0; }
      const tr = el("tr");
      tr.appendChild(el("td")).appendChild(el("b", "", h.name || ""));
      tr.appendChild(el("td")).appendChild(el("code", "", h.addr || ""));
      const st = tr.appendChild(el("td"));
      st.appendChild(chip(h.up ? "up" : "down", h.up ? "booted" : "expiring"));
      if (!h.up && h.last_error) {
        st.appendChild(document.createTextNode(" "));
        st.appendChild(el("span", "meta err", h.last_error));
      }
      const lbl = tr.appendChild(el("td"));
      for (const l of h.labels || []) lbl.appendChild(chip(l, "label"));
      const bld = tr.appendChild(el("td"));
      if (h.build) {
        const skewed = majorityBuild && h.build !== majorityBuild;
        bld.appendChild(chip(h.build, skewed ? "expiring" : ""));
        if (skewed) bld.title = "version skew: majority of hosts run " + majorityBuild;
      } else {
        bld.textContent = "—";
      }
      tr.appendChild(el("td", "", String(h.targets ?? 0)));
      tr.appendChild(el("td", "", String(h.active_leases ?? 0)));
      tr.appendChild(el("td", "", fmtTime(h.last_probe)));
      tbody.appendChild(tr);
    }
    $("fleet-summary").textContent =
      up + "/" + hosts.length + " hosts up · " + targets + " targets";
  }

  // ---- targets / leases tables --------------------------------------------

  function renderTargets(targets) {
    const tbody = $("targets").querySelector("tbody");
    tbody.replaceChildren();
    $("targets-empty").hidden = targets.length > 0;
    for (const t of targets) {
      const tr = el("tr");
      tr.appendChild(el("td")).appendChild(el("b", "", t.host || ""));
      tr.appendChild(el("td", "", t.name || ""));
      tr.appendChild(el("td", "", t.runtime || ""));
      tr.appendChild(el("td", "", t.device_type || ""));
      const st = tr.appendChild(el("td"));
      st.appendChild(chip(t.state || "?", (t.state || "").toLowerCase()));
      if (t.warm) st.appendChild(document.createTextNode(" ")),
        st.appendChild(chip("parked", "parked"));
      if (t.recording) st.appendChild(document.createTextNode(" ")),
        st.appendChild(chip("rec", "rec"));
      const lbl = tr.appendChild(el("td"));
      for (const l of t.labels || []) lbl.appendChild(chip(l, "label"));
      tr.appendChild(el("td")).appendChild(el("code", "", t.udid || ""));
      const act = tr.appendChild(el("td"));
      const btn = el("button", "", "live view");
      const addr = hostAddrs[t.host];
      // Parked sims are frozen (streaming would hang) and non-booted sims
      // have nothing to show; the view page is served by the owning daemon.
      if (t.warm || t.state !== "Booted" || !addr) {
        btn.disabled = true;
        btn.title = t.warm ? "parked in warm pool"
          : t.state !== "Booted" ? "not booted" : "host address unknown";
        act.appendChild(btn);
      } else {
        const a = el("a");
        a.href = withToken(addr + "/view/" + encodeURIComponent(t.udid));
        a.target = "_blank";
        a.appendChild(btn);
        act.appendChild(a);
      }
      tbody.appendChild(tr);
    }
  }

  function renderLeases(leases) {
    const tbody = $("leases").querySelector("tbody");
    tbody.replaceChildren();
    $("leases-empty").hidden = leases.length > 0;
    const now = Date.now();
    for (const l of leases) {
      const tr = el("tr");
      tr.appendChild(el("td")).appendChild(el("b", "", l.host || ""));
      tr.appendChild(el("td", "", l.agent_id || ""));
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
  async function refreshFleet() {
    if (refreshing) return;
    refreshing = true;
    try {
      // Hosts first: targets/leases rendering and multiview negotiation
      // resolve host_addr through the hosts map.
      const [h, t, l] = await Promise.all([
        getJSON("/v0/fleet/hosts"),
        getJSON("/v0/targets"),
        getJSON("/v0/leases"),
      ]);
      renderHosts(h.hosts || []);
      renderTargets(t.targets || []);
      renderLeases(l.leases || []);
      renderMultiview(t.targets || []);
    } catch {
      /* header health shows reachability; keep the last good tables */
    } finally {
      refreshing = false;
    }
  }

  setInterval(() => { refreshHealth(); refreshFleet(); }, POLL_MS);

  // ---- multiviewer ---------------------------------------------------------
  // Streams are strictly opt-in per tile: activating a tile negotiates a
  // stream via POST {host_addr}/v0/streams against the owning daemon and
  // attaches the offer's MJPEG URL (also on the daemon — the broker never
  // carries media). Streams are shared per target (Open is idempotent), so
  // deactivating only detaches this tile's viewer — never DELETE, which
  // would cut off other viewers of the same target; the no-viewer linger
  // stops capture shortly after the last viewer detaches.

  // keys ("host/udid") of tiles with an attached stream.
  const mvActive = new Set();

  const mvKey = (t) => (t.host || "") + "/" + t.udid;

  const mvStreamable = (t) =>
    t.kind !== "device" && !t.warm && t.state === "Booted" && hostAddrs[t.host];

  async function mvStart(key, tile) {
    // Mark the tile wanted before the async negotiation so a stop during
    // the round-trip (toggle click, "stop all") retracts the intent and
    // the resolved offer is discarded instead of attaching anyway.
    mvActive.add(key);
    const addr = tile.dataset.addr;
    const udid = tile.dataset.udid;
    const status = tile.querySelector(".mv-status");
    status.textContent = "negotiating…";
    status.classList.remove("err");
    try {
      const r = await fetch(addr + "/v0/streams", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ udid, format: "mjpeg" }),
      });
      if (r.status === 401) promptToken();
      const offer = await r.json();
      if (!r.ok) {
        mvActive.delete(key);
        status.textContent = offer.code === "stream_limit"
          ? "stream limit reached on this host (--stream-max-streams); stop another of its tiles first"
          : "error: " + (offer.message || r.status);
        status.classList.add("err");
        return;
      }
      // Reconciled away or stopped mid-negotiation: drop the offer.
      if (!tile.isConnected || !mvActive.has(key)) return;
      tile.classList.add("live");
      const img = tile.querySelector("img");
      // Negotiation only reserves the stream; the <img> attach itself can
      // still fail (viewer limit, or the stream got reaped in between).
      img.onerror = () => {
        mvStop(key, tile);
        status.textContent = "stream attach failed (viewer limit, or the stream was reaped)";
        status.classList.add("err");
      };
      // The offer's URLs are daemon-relative; the media is fetched from
      // the owning host, not the broker.
      img.src = withToken(addr + offer.mjpeg_url);
      img.hidden = false;
      status.textContent = "live · " + offer.fps + " fps";
    } catch (e) {
      mvActive.delete(key);
      status.textContent = "error: " + e;
      status.classList.add("err");
    }
  }

  function mvStop(key, tile) {
    mvActive.delete(key);
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

  const mvTile = (key) => {
    for (const tile of $("mv-grid").children) {
      if (tile.dataset.key === key) return tile;
    }
    return null;
  };

  // renderMultiview reconciles the tile grid with the latest federated
  // target list: tiles come and go with target state (and host
  // reachability), but an active tile's <img> is never touched (replacing
  // it would drop the open MJPEG connection).
  function renderMultiview(targets) {
    const grid = $("mv-grid");
    const want = targets.filter(mvStreamable);
    const wantKeys = new Set(want.map(mvKey));
    for (const tile of [...grid.children]) {
      const key = tile.dataset.key;
      if (!wantKeys.has(key)) {
        // Detach this tile's viewer before dropping it: removing the
        // <img> from the DOM does not reliably abort an MJPEG fetch.
        mvStop(key, tile); // target gone, no longer streamable, or host down
        tile.remove();
      }
    }
    for (const t of want) {
      const key = mvKey(t);
      let tile = mvTile(key);
      if (!tile) {
        tile = el("div", "mv-tile");
        tile.dataset.key = key;
        tile.dataset.udid = t.udid;
        const head = tile.appendChild(el("div", "mv-name"));
        head.appendChild(el("b", "", t.name || t.udid));
        head.appendChild(el("span", "meta",
          " " + [t.host, t.runtime].filter(Boolean).join(" · ")));
        const img = tile.appendChild(el("img"));
        img.alt = "live stream of " + (t.name || t.udid) + " on " + (t.host || "?");
        img.hidden = true;
        tile.appendChild(el("div", "mv-status", "click to start stream"));
        tile.onclick = () => {
          if (mvActive.has(key)) mvStop(key, tile);
          else mvStart(key, tile);
        };
        grid.appendChild(tile);
      }
      // The owning host's address can change across broker restarts; keep
      // the tile's negotiation address current (attached streams keep
      // their already-resolved img.src).
      tile.dataset.addr = hostAddrs[t.host];
    }
    $("mv-empty").hidden = grid.children.length > 0;
  }

  $("mv-start-all").onclick = () => {
    for (const tile of $("mv-grid").children) {
      if (!mvActive.has(tile.dataset.key)) mvStart(tile.dataset.key, tile);
    }
  };
  $("mv-stop-all").onclick = () => {
    for (const tile of $("mv-grid").children) {
      if (mvActive.has(tile.dataset.key)) mvStop(tile.dataset.key, tile);
    }
  };

  // ---- tabs -----------------------------------------------------------------

  for (const btn of document.querySelectorAll(".tab")) {
    btn.onclick = () => {
      for (const b of document.querySelectorAll(".tab")) {
        b.classList.toggle("active", b === btn);
      }
      $("tab-fleet").hidden = btn.dataset.tab !== "fleet";
      $("tab-multiview").hidden = btn.dataset.tab !== "multiview";
    };
  }

  // ---- boot -----------------------------------------------------------------

  refreshHealth();
  refreshFleet();
})();
