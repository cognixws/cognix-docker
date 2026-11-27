(async () => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  const ICONS = {
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
    chev: '<path d="m6 9 6 6 6-6"/>',
    chevRight: '<path d="m9 18 6-6-6-6"/>',
    x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  };
  const ico = (name, cls = "") => `<svg class="dk-ico ${cls}" viewBox="0 0 24 24" aria-hidden="true">${ICONS[name]}</svg>`;

  const doors = window.picode || { use: async () => null };
  await doors.use("theme");
  const proc = await doors.use("process");
  const confirmDoor = await doors.use("confirm");
  const eventsDoor = await doors.use("events");
  const ctxDoor = await doors.use("context");
  const linksDoor = await doors.use("links");
  const placesDoor = await doors.use("places");
  const workspaceDoor = await doors.use("workspace");

  const S = {
    view: "stacks",       // stacks | health | resources | plans
    search: "",
    filter: "all",        // all | running | stopped | attention
    collapsed: {},
    busyIds: new Set(),
    openPath: "",
    loading: true,
    overview: null,
    resources: null,
    audit: null,
    healthByProject: {},
    // Inspector state: { mode: "container"|"stack-logs"|"diagnosis", id, project, tab, detail, stackLogs, diagnosis, logQuery, logLevel }
    insp: null,
  };

  let toastTimer = null;
  function showToast(msg) {
    const el = $("dk-toast");
    if (!el) return;
    if (toastTimer) clearTimeout(toastTimer);
    el.textContent = msg;
    el.hidden = false;
    toastTimer = setTimeout(() => { el.hidden = true; }, 3200);
  }
  function hideToast() {
    const el = $("dk-toast");
    if (toastTimer) { clearTimeout(toastTimer); toastTimer = null; }
    if (el) el.hidden = true;
  }

  async function api(path, body, attempt = 0) {
    if (!proc) throw new Error("Turn Docker on in a workspace to manage containers here.");
    let r;
    try {
      r = await proc.request(path, body === undefined
        ? { method: "GET" }
        : { method: "POST", body: JSON.stringify(body), contentType: "application/json" });
    } catch (err) {
      if (attempt < 4 && String(err?.message || "").includes("did not answer")) {
        await new Promise((res) => setTimeout(res, 650));
        return api(path, body, attempt + 1);
      }
      // No answer from the extension's own process (not an error it sent).
      if (err && typeof err === "object") err.transport = true;
      throw err;
    }
    let data = {};
    try { data = JSON.parse(r.body || "{}"); } catch { data = { message: r.body }; }
    if (r.status >= 400) {
      throw new Error(data.message || data.error || ("Docker extension returned " + r.status));
    }
    return data;
  }

  function fmtBytes(bytes) {
    const n = Number(bytes || 0);
    if (!n || n < 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let idx = 0, val = n;
    while (val >= 1024 && idx < units.length - 1) { val /= 1024; idx++; }
    return (val >= 10 || idx === 0 ? val.toFixed(0) : val.toFixed(1)) + " " + units[idx];
  }

  function fmtRel(iso) {
    if (!iso) return "";
    const ms = Date.parse(iso);
    if (Number.isNaN(ms)) return "";
    const diffSec = Math.round((ms - Date.now()) / 1000);
    if (diffSec > 0) {
      const m = Math.ceil(diffSec / 60);
      return m <= 1 ? "in 1m" : `in ${m}m`;
    }
    const ago = Math.abs(diffSec);
    if (ago < 60) return "just now";
    if (ago < 3600) return `${Math.floor(ago / 60)}m ago`;
    if (ago < 86400) return `${Math.floor(ago / 3600)}h ago`;
    return `${Math.floor(ago / 86400)}d ago`;
  }

  function displayStack(p) {
    return p ? p : "Standalone containers";
  }

  function updateBadges() {
    const ov = S.overview;
    const reachable = !!(ov && ov.reachable);
    const stBadge = $("dk-badge-stacks");
    const hlBadge = $("dk-badge-health");
    const rsBadge = $("dk-badge-resources");
    const plBadge = $("dk-badge-plans");

    const total = reachable ? (ov.totalContainers || 0) : 0;
    if (total > 0) { stBadge.textContent = String(total); stBadge.hidden = false; } else { stBadge.hidden = true; }

    const incCount = reachable && Array.isArray(ov.openIncidents) ? ov.openIncidents.length : 0;
    if (incCount > 0) { hlBadge.textContent = String(incCount); hlBadge.hidden = false; } else { hlBadge.hidden = true; }

    const unusedCount = reachable && S.resources && Array.isArray(S.resources.items) ? S.resources.items.filter((x) => x.removable).length : 0;
    if (unusedCount > 0) { rsBadge.textContent = String(unusedCount); rsBadge.hidden = false; } else { rsBadge.hidden = true; }

    const planCount = ov && Array.isArray(ov.pendingPlans) ? ov.pendingPlans.length : 0;
    if (planCount > 0) { plBadge.textContent = String(planCount); plBadge.hidden = false; } else { plBadge.hidden = true; }
  }

  function renderSummary() {
    const sum = $("dk-summary");
    if (!proc) {
      sum.textContent = "Inactive in this workspace";
      return;
    }
    if (S.loading && !S.overview) {
      sum.textContent = "Loading local containers…";
      return;
    }
    const ov = S.overview;
    if (!ov || !ov.reachable) {
      sum.innerHTML = `<span class="dk-dot is-danger"></span><span>Local engine unreachable</span>`;
      return;
    }
    const epShort = (ov.endpoint || "").replace(/^unix:\/\//, "") || "local";
    const runTxt = `${ov.runningContainers} of ${ov.totalContainers} running`;
    sum.innerHTML = `<span class="dk-dot ${ov.openIncidents?.length ? "is-warn" : "is-ok"}"></span><span>${esc(runTxt)}</span><span class="dk-endpoint-pill">${esc(epShort.split("/").pop() || "local")}</span>`;
  }

  async function requestConfirmAndRun(payload) {
    try {
      const prep = await api("/confirmations", payload);
      if (!confirmDoor) {
        showToast("Confirmation requires opening Docker inside COGNIX[WS].");
        return false;
      }
      const ans = await confirmDoor.operation(prep.operationId);
      if (!ans || !ans.confirmed) return false;
      showToast("Completed.");
      await refreshAll(false);
      return true;
    } catch (e) {
      showToast(e.message);
      await refreshAll(false);
      return false;
    }
  }

  async function startContainerDirect(id) {
    S.busyIds.add(id);
    drawMain();
    try {
      const r = await api("/containers/" + encodeURIComponent(id) + "/start", {});
      showToast(r.message || "Started container.");
    } catch (e) {
      showToast(e.message);
    } finally {
      S.busyIds.delete(id);
      await refreshAll(false);
    }
  }

  async function copyText(text, label = "Copied to clipboard.") {
    try {
      if (linksDoor) await linksDoor.copy(text);
      else if (navigator.clipboard) await navigator.clipboard.writeText(text);
      showToast(label);
    } catch {
      showToast("Couldn't copy here. Select the command and copy it.");
    }
  }

  function parsePortUrl(portSpec) {
    // e.g. ":8080->80/tcp" or "127.0.0.1:5432->5432/tcp"
    const m = /^([^:]*):(\d+)->/.exec(portSpec || "");
    if (!m) return "";
    const host = m[1] && m[1] !== "0.0.0.0" && m[1] !== "::" ? m[1] : "localhost";
    return `http://${host}:${m[2]}`;
  }

  // ---------- Main Views ----------

  function drawMain() {
    renderSummary();
    updateBadges();
    const main = $("dk-main");
    if (!proc) {
      main.innerHTML = `<div class="dk-state-box">
        <h3>Docker is turned off here</h3>
        <p>Enable the Docker extension in your workspace settings to inspect stacks and run maintenance plans.</p>
        ${placesDoor ? `<button class="dk-btn is-primary" type="button" data-act="open-ext-settings">Open settings</button>` : ""}
      </div>`;
      return;
    }

    if (S.loading && !S.overview) {
      main.innerHTML = `<div class="dk-section-card" style="padding:18px;display:grid;gap:10px">
        <div class="dk-skel" style="width:38%"></div>
        <div class="dk-skel" style="width:82%"></div>
        <div class="dk-skel" style="width:64%"></div>
      </div>`;
      return;
    }

    const ov = S.overview;
    if (S.view === "stacks") drawStacksView(main, ov);
    else if (S.view === "health") drawHealthView(main, ov);
    else if (S.view === "resources") drawResourcesView(main, ov);
    else if (S.view === "plans") drawPlansView(main, ov);
  }

  // Docker cannot be used: which case (the server's problem, the same table
  // as COGNIX[WS]'s Software row), one sentence and one action. A helper
  // that did not start is its own case: Docker was never asked.
  function notUsableBox(ov) {
    const p = ov && ov.problem;
    if (ov && ov.helperFailed) {
      return `<div class="dk-state-box" role="alert">
        <h3>The Docker extension didn't start</h3>
        <p title="${esc(ov.error || "")}">Its background process didn't answer, so Docker wasn't checked. Restart the extension from its settings.</p>
        ${placesDoor
          ? `<button class="dk-btn is-primary" type="button" data-act="open-ext-settings">Open settings</button>`
          : `<button class="dk-btn is-primary" type="button" data-act="retry">Check again</button>`}
      </div>`;
    }
    if (!p) {
      return `<div class="dk-state-box" role="alert">
        <h3>Docker doesn't answer</h3>
        <p>${esc((ov && ov.error) || "Docker didn't answer on this computer.")}</p>
        <button class="dk-btn is-primary" type="button" data-act="retry">Check again</button>
      </div>`;
    }
    // After the command is copied, the one action is Check again.
    const cmd = p.action && p.action.kind === "copy" ? p.action.command : "";
    const copy = cmd && S.copiedFix !== cmd;
    return `<div class="dk-state-box" role="alert" data-state="${esc(p.state)}">
      <h3>${esc(p.title)}</h3>
      <p>${esc(p.sentence)}</p>
      ${cmd ? `<code class="dk-cmd">${esc(cmd)}</code>` : ""}
      ${copy
        ? `<button class="dk-btn is-primary" type="button" data-act="copy-fix" data-cmd="${esc(cmd)}">${esc(p.action.label)}</button>`
        : p.action && p.action.kind === "link" && p.action.url && linksDoor
          ? `<button class="dk-btn is-primary" type="button" data-act="open-fix" data-url="${esc(p.action.url)}">${esc(p.action.label)}</button>`
          : `<button class="dk-btn is-primary" type="button" data-act="retry">Check again</button>`}
    </div>`;
  }

  function drawStacksView(main, ov) {
    if (!ov || !ov.reachable) {
      main.innerHTML = notUsableBox(ov);
      return;
    }

    const parts = [];

    // 1. Attention strip (open incidents + pending 5-min plans)
    const attRows = [];
    for (const inc of ov.openIncidents || []) {
      attRows.push(`<div class="dk-att-row">
        <div class="dk-att-left">
          <span class="dk-dot is-danger"></span>
          <div class="dk-att-text"><b>${esc(inc.title)}</b><span class="dk-att-sub">${esc(displayStack(inc.project))} · ${esc(inc.detail)}</span></div>
        </div>
        <button class="dk-btn" type="button" data-act="diagnose-stack" data-project="${esc(inc.project)}">Diagnose</button>
      </div>`);
    }
    for (const pl of ov.pendingPlans || []) {
      attRows.push(`<div class="dk-att-row">
        <div class="dk-att-left">
          <span class="dk-dot is-warn"></span>
          <div class="dk-att-text"><b>${esc(pl.title)}</b><span class="dk-att-sub">From ${esc(pl.actor)} · expires ${esc(fmtRel(pl.expiresAt))}</span></div>
        </div>
        <div class="dk-actions-row">
          <button class="dk-btn is-ghost" type="button" data-act="go-plans">View plan</button>
          <button class="dk-btn is-primary" type="button" data-act="approve-plan" data-plan="${esc(pl.id)}">Approve plan</button>
        </div>
      </div>`);
    }
    if (attRows.length) {
      parts.push(`<section class="dk-attention" aria-label="Needs attention">${attRows.join("")}</section>`);
    }

    // 2. Open-With Compose banner
    if (ov.openCompose) {
      const cp = ov.openCompose;
      if (cp.valid) {
        const pills = (cp.services || []).map((s) => `<span class="dk-badge ${s.action === "unchanged" ? "is-ok" : "is-warn"}">${esc(s.service)}: ${esc(s.action)}</span>`).join("");
        parts.push(`<section class="dk-compose-banner">
          <div>
            <div><b>Compose stack ${esc(cp.project)}</b> <span style="color:var(--dk-muted);font-size:12px">(${esc((cp.files || []).join(", "))})</span></div>
            <div class="dk-compose-diffs">${pills || "Validated configuration"}</div>
          </div>
          <button class="dk-btn is-primary" type="button" data-act="deploy-open-compose">Deploy stack</button>
        </section>`);
      } else {
        parts.push(`<section class="dk-compose-banner">
          <div>
            <div><b>Compose validation issue</b></div>
            <div style="font-size:12px;color:var(--dk-danger);margin-top:3px">${esc(cp.error || "Invalid Compose file.")}</div>
          </div>
          <button class="dk-btn" type="button" data-act="open-compose-modal">Edit path</button>
        </section>`);
      }
    }

    // 3. Primary Empty state when zero containers & zero registered stacks
    if (!ov.groups || ov.groups.length === 0) {
      parts.push(`<div class="dk-state-box">
        <h3>No containers yet</h3>
        <p>No containers or stacks are on this computer yet, so compose a stack to start one here.</p>
        <button class="dk-btn is-primary" type="button" data-act="open-compose-modal">Compose stack</button>
      </div>`);
      main.innerHTML = parts.join("");
      return;
    }

    // 4. Search + Filter toolbar
    parts.push(`<div class="dk-toolbar">
      <div class="dk-search-wrap">
        ${ico("search")}
        <input class="dk-input" id="dk-search-in" type="search" placeholder="Filter stacks, containers, images…" value="${esc(S.search)}" aria-label="Filter containers">
        <span class="dk-kbd-hint">/</span>
      </div>
      <div class="dk-filters" role="group" aria-label="Container state filter">
        ${["all", "running", "stopped", "attention"].map((f) => `<button class="dk-filter-btn" type="button" data-filter="${f}" aria-pressed="${S.filter === f ? "true" : "false"}">${f[0].toUpperCase() + f.slice(1)}</button>`).join("")}
      </div>
    </div>`);

    // 5. Filtered Groups
    const q = S.search.trim().toLowerCase();
    let shownContainers = 0;
    let shownGroups = 0;

    for (const g of ov.groups) {
      const stackName = displayStack(g.project);
      const stackMatches = !q || stackName.toLowerCase().includes(q);
      const rows = (g.containers || []).filter((c) => {
        if (S.filter === "running" && c.state !== "running") return false;
        if (S.filter === "stopped" && c.state === "running") return false;
        if (S.filter === "attention" && !(c.health === "unhealthy" || c.state === "restarting")) return false;
        if (!q || stackMatches) return true;
        return c.name.toLowerCase().includes(q) || c.image.toLowerCase().includes(q) || (c.service || "").toLowerCase().includes(q) || (c.ports || []).some((p) => p.toLowerCase().includes(q));
      });

      if (rows.length === 0 && !(g.registered && S.filter === "all" && stackMatches)) {
        continue;
      }
      shownGroups++;
      shownContainers += rows.length;

      const key = g.project || "__standalone__";
      const isOpen = !S.collapsed[key];
      const canRestart = (g.containers || []).some((c) => c.state === "running");
      const canStop = (g.containers || []).some((c) => c.state === "running" || c.state === "restarting");
      const canStart = (g.containers || []).some((c) => c.state === "exited" || c.state === "created");

      const rowHTML = rows.map((c) => {
        const isSel = S.insp && S.insp.mode === "container" && S.insp.id === c.id;
        const isBusy = S.busyIds.has(c.id);
        const dotCls = c.health === "unhealthy" ? "is-danger" : c.state === "running" ? "is-ok" : c.state === "restarting" ? "is-warn" : "";
        const wordCls = c.health === "unhealthy" ? "is-danger" : c.state === "running" ? "is-ok" : c.state === "restarting" ? "is-warn" : "";
        const stateLabel = c.health === "unhealthy" ? "unhealthy" : (c.health === "healthy" ? "healthy" : c.state);

        const portsHTML = (c.ports || []).slice(0, 3).map((p) => {
          const short = p.replace(/^->/, "").split("->")[0];
          return `<button class="dk-port" type="button" data-act="open-port" data-port="${esc(p)}" title="Open ${esc(p)}">${esc(short)}</button>`;
        }).join("");

        let metricsHTML = `<div class="dk-metrics"></div>`;
        if (c.state === "running" && (c.cpuPercent != null || c.memoryBytes)) {
          const cpu = c.cpuPercent != null ? `${c.cpuPercent.toFixed(1)}%` : "—";
          const memPct = c.limitBytes ? Math.min(100, Math.round((c.memoryBytes * 100) / c.limitBytes)) : 0;
          const barCls = memPct >= 85 ? "is-danger" : memPct >= 70 ? "is-warn" : "";
          metricsHTML = `<div class="dk-metrics">
            <div class="dk-metric-top"><span>CPU ${esc(cpu)}</span><span>${esc(fmtBytes(c.memoryBytes))}</span></div>
            <div class="dk-bar"><i class="${barCls}" style="width:${Math.max(4, memPct)}%"></i></div>
          </div>`;
        }

        return `<div class="dk-row ${isSel ? "is-selected" : ""} ${isBusy ? "is-busy" : ""}" role="button" tabindex="0" data-act="inspect-container" data-id="${esc(c.id)}" aria-label="Inspect ${esc(c.name)}">
          <span class="dk-dot ${dotCls}"></span>
          <div class="dk-c-ident">
            <div class="dk-c-name-line"><span class="dk-c-name">${esc(c.name)}</span></div>
            <div class="dk-c-sub">${esc(c.image)}</div>
          </div>
          <div class="dk-ports">${portsHTML}</div>
          ${metricsHTML}
          <div class="dk-c-state"><span class="dk-state-word ${wordCls}">${esc(stateLabel)}</span>${ico("chevRight", "dk-row-chev")}</div>
        </div>`;
      }).join("");

      parts.push(`<section class="dk-group" data-open="${isOpen ? "true" : "false"}">
        <div class="dk-group-head">
          <button class="dk-group-toggle" type="button" data-act="toggle-group" data-key="${esc(key)}" aria-expanded="${isOpen ? "true" : "false"}">
            ${ico("chev", "dk-chev")}
            <span class="dk-stack-name">${esc(stackName)}</span>
            <span class="dk-group-meta">${g.runningCount}/${g.totalCount} running${g.monitored ? " · monitored" : ""}</span>
          </button>
          <div class="dk-group-actions">
            ${g.totalCount > 0 ? `<button class="dk-btn is-ghost" type="button" data-act="stack-logs" data-project="${esc(g.project)}">Logs</button>` : ""}
            ${g.totalCount > 0 ? `<button class="dk-btn is-ghost" type="button" data-act="diagnose-stack" data-project="${esc(g.project)}">Diagnose</button>` : ""}
            ${g.registered ? `<button class="dk-btn" type="button" data-act="deploy-registered" data-project="${esc(g.project)}">Deploy stack</button>` : ""}
            ${canStart ? `<button class="dk-btn" type="button" data-act="stack-op" data-project="${esc(g.project)}" data-op="start">Start stack</button>` : ""}
            ${canRestart ? `<button class="dk-btn" type="button" data-act="stack-op" data-project="${esc(g.project)}" data-op="restart">Restart stack</button>` : ""}
            ${canStop ? `<button class="dk-btn is-danger" type="button" data-act="stack-op" data-project="${esc(g.project)}" data-op="stop">Stop stack</button>` : ""}
          </div>
        </div>
        <div class="dk-rows">${rowHTML || `<div style="padding:12px 14px;color:var(--dk-muted);font-size:12.5px">Registered Compose stack has no containers yet. Use Deploy stack to start it.</div>`}</div>
      </section>`);
    }

    if (shownGroups === 0 && shownContainers === 0) {
      parts.push(`<div class="dk-state-box">
        <h3>No matching containers</h3>
        <p>Nothing matches the current search query or state filter.</p>
        <button class="dk-btn" type="button" data-act="clear-filters">Clear filters</button>
      </div>`);
    }

    const activeSearch = document.activeElement && document.activeElement.id === "dk-search-in";
    const selStart = activeSearch ? document.activeElement.selectionStart : null;
    main.innerHTML = parts.join("");
    if (activeSearch) {
      const input = $("dk-search-in");
      if (input) {
        input.focus();
        if (selStart != null) input.setSelectionRange(selStart, selStart);
      }
    }
  }

  function drawHealthView(main, ov) {
    if (!ov || !ov.reachable) {
      main.innerHTML = notUsableBox(ov);
      return;
    }

    const parts = [];
    const open = ov.openIncidents || [];
    if (open.length > 0) {
      parts.push(`<section class="dk-section-card">
        <div class="dk-sec-head"><h3>Open incidents</h3></div>
        ${open.map((inc) => `<div class="dk-res-row">
          <div class="dk-res-info">
            <div class="dk-res-title"><span class="dk-dot is-danger"></span><span>${esc(inc.title)}</span><span class="dk-badge is-danger">${esc(displayStack(inc.project))}</span></div>
            <div class="dk-res-sub">${esc(inc.detail)} · ${esc(inc.badStreak)} consecutive bad samples</div>
          </div>
          <button class="dk-btn" type="button" data-act="diagnose-stack" data-project="${esc(inc.project)}">Diagnose</button>
        </div>`).join("")}
      </section>`);
    }

    const groups = ov.groups || [];
    if (groups.length === 0) {
      parts.push(`<div class="dk-state-box">
        <h3>No stacks to monitor yet</h3>
        <p>No containers or stacks are running yet; view stacks to compose a stack first.</p>
        <button class="dk-btn" type="button" data-act="go-stacks">View stacks</button>
      </div>`);
      main.innerHTML = parts.join("");
      return;
    }

    parts.push(`<section class="dk-section-card">
      <div class="dk-sec-head"><h3>Stack health monitors</h3></div>
      ${groups.map((g) => {
        const hInfo = S.healthByProject[g.project] || {};
        const mon = hInfo.monitor || { enabled: g.monitored, intervalSeconds: 60, cpuPercent: 90, memoryPercent: 85, badSamples: 3, retentionDays: 7, revision: 0 };
        return `<div class="dk-res-row">
          <div class="dk-res-info">
            <div class="dk-res-title">
              <span class="dk-dot ${g.unhealthyCount ? "is-danger" : mon.enabled ? "is-ok" : ""}"></span>
              <span>${esc(displayStack(g.project))}</span>
              ${mon.enabled ? `<span class="dk-badge is-ok">Every ${esc(mon.intervalSeconds)}s</span>` : `<span class="dk-badge">Manual</span>`}
            </div>
            <div class="dk-res-sub">${g.runningCount}/${g.totalCount} running · Alert after ${esc(mon.badSamples)} bad samples (CPU ≥ ${esc(mon.cpuPercent)}%, Mem ≥ ${esc(mon.memoryPercent)}%)</div>
          </div>
          <div class="dk-actions-row">
            <select class="dk-select" aria-label="Cadence for ${esc(displayStack(g.project))}" data-mon-int="${esc(g.project)}">
              ${[30, 60, 300].map((sec) => `<option value="${sec}" ${mon.intervalSeconds === sec ? "selected" : ""}>Every ${sec}s</option>`).join("")}
            </select>
            <button class="dk-btn" type="button" data-act="check-health" data-project="${esc(g.project)}">Check health</button>
            <button class="dk-btn ${mon.enabled ? "" : "is-primary"}" type="button" data-act="toggle-monitor" data-project="${esc(g.project)}" data-enabled="${mon.enabled ? "1" : "0"}" data-rev="${esc(mon.revision || 0)}">${mon.enabled ? "Pause monitor" : "Enable monitor"}</button>
          </div>
        </div>`;
      }).join("")}
    </section>`);

    main.innerHTML = parts.join("");
  }

  function drawResourcesView(main, ov) {
    if (!ov || !ov.reachable) {
      main.innerHTML = notUsableBox(ov);
      return;
    }

    const items = (S.resources && S.resources.items) || [];
    if (!S.resources) {
      main.innerHTML = `<div class="dk-section-card" style="padding:18px;display:grid;gap:10px">
        <div class="dk-skel" style="width:45%"></div>
        <div class="dk-skel" style="width:75%"></div>
      </div>`;
      return;
    }
    if (items.length === 0) {
      main.innerHTML = `<div class="dk-state-box">
        <h3>No local resources yet</h3>
        <p>No images, volumes, or custom networks are stored right now; check again after pulling an image or starting a stack.</p>
        <button class="dk-btn" type="button" data-act="retry">Check again</button>
      </div>`;
      return;
    }

    const kinds = [
      ["image", "Images"],
      ["volume", "Volumes"],
      ["network", "Networks"],
    ];
    main.innerHTML = kinds.map(([kind, label]) => {
      const list = items.filter((x) => x.kind === kind);
      if (!list.length) return "";
      return `<section class="dk-section-card">
        <div class="dk-sec-head"><h3>${esc(label)}</h3></div>
        ${list.map((r) => {
          const consumers = r.consumers || [];
          const sz = r.reclaimableBytes != null ? fmtBytes(r.reclaimableBytes) : (r.sizeBytes != null ? fmtBytes(r.sizeBytes) : "");
          const sub = consumers.length
            ? `Used by ${consumers.map((c) => `${c.name} (${c.state})`).join(", ")}`
            : (r.removable ? `Unused${r.project ? ` · project ${r.project}` : ""}${sz ? ` · ${sz} reclaimable` : ""}` : (r.reason || "Protected"));
          return `<div class="dk-res-row">
            <div class="dk-res-info">
              <div class="dk-res-title">
                <span>${esc(r.name)}</span>
                ${r.removable ? `<span class="dk-badge is-warn">Unused</span>` : `<span class="dk-badge">Protected</span>`}
                ${sz ? `<span class="dk-badge">${esc(sz)}</span>` : ""}
              </div>
              <div class="dk-res-sub">${esc(sub)}</div>
            </div>
            ${r.removable ? `<button class="dk-btn is-danger" type="button" data-act="remove-resource" data-rkind="${esc(r.kind)}" data-rid="${esc(r.id)}">Remove ${esc(r.kind)}</button>` : ""}
          </div>`;
        }).join("")}
      </section>`;
    }).join("");
  }

  function drawPlansView(main, ov) {
    const plans = (ov && ov.pendingPlans) || (S.audit && S.audit.pendingPlans) || [];
    const jobs = (S.audit && S.audit.jobs) || [];
    const ops = (S.audit && S.audit.operations) || [];

    if (!plans.length && !jobs.length && !ops.length) {
      main.innerHTML = `<div class="dk-state-box">
        <h3>No maintenance plans yet</h3>
        <p>No repair plans or audited changes are waiting right now; view stacks to inspect containers or run a diagnosis.</p>
        <button class="dk-btn" type="button" data-act="go-stacks">View stacks</button>
      </div>`;
      return;
    }

    const parts = [];
    if (plans.length > 0) {
      parts.push(`<section class="dk-section-card">
        <div class="dk-sec-head"><h3>Awaiting confirmation</h3></div>
        ${plans.map((p) => `<div class="dk-res-row">
          <div class="dk-res-info">
            <div class="dk-res-title">
              <span class="dk-dot is-warn"></span>
              <span>${esc(p.title)}</span>
              <span class="dk-badge is-warn">Expires ${esc(fmtRel(p.expiresAt))}</span>
            </div>
            <div class="dk-res-sub">${esc(p.impact)} · Proposed by ${esc(p.actor)}</div>
          </div>
          <div class="dk-actions-row">
            ${p.inboxId && placesDoor ? `<button class="dk-btn is-ghost" type="button" data-act="open-inbox" data-inbox="${esc(p.inboxId)}">Open inbox</button>` : ""}
            <button class="dk-btn is-primary" type="button" data-act="approve-plan" data-plan="${esc(p.id)}">Approve plan</button>
          </div>
        </div>`).join("")}
      </section>`);
    }

    if (jobs.length > 0 || ops.length > 0) {
      const historyRows = [];
      for (const j of jobs.slice(0, 20)) {
        const badgeCls = j.state === "succeeded" ? "is-ok" : j.state === "failed" ? "is-danger" : "is-warn";
        historyRows.push(`<div class="dk-res-row">
          <div class="dk-res-info">
            <div class="dk-res-title">
              <span>${esc(j.title)}</span>
              <span class="dk-badge ${badgeCls}">${esc(j.state)}</span>
            </div>
            <div class="dk-res-sub">${esc(j.message || "")} · Actor ${esc(j.actor)} · Approved by ${esc(j.approvedBy)} · ${esc(fmtRel(j.finishedAt || j.createdAt))}</div>
          </div>
        </div>`);
      }
      for (const op of ops.slice(0, 20)) {
        const badgeCls = op.state === "succeeded" ? "is-ok" : op.state === "failed" ? "is-danger" : "is-warn";
        historyRows.push(`<div class="dk-res-row">
          <div class="dk-res-info">
            <div class="dk-res-title">
              <span>${esc(op.action)} ${esc(op.containerName)}</span>
              <span class="dk-badge ${badgeCls}">${esc(op.state)}</span>
            </div>
            <div class="dk-res-sub">${esc(op.message || "")} · Actor ${esc(op.actor)} · ${esc(fmtRel(op.finishedAt || op.createdAt))}</div>
          </div>
        </div>`);
      }
      parts.push(`<section class="dk-section-card">
        <div class="dk-sec-head"><h3>Audit trail</h3></div>
        ${historyRows.join("")}
      </section>`);
    }

    main.innerHTML = parts.join("");
  }

  // ---------- Inspector Drawer ----------

  function drawInspector() {
    const aside = $("dk-inspector");
    if (!S.insp) {
      aside.hidden = true;
      aside.innerHTML = "";
      return;
    }
    aside.hidden = false;

    if (S.insp.mode === "container") {
      const d = S.insp.detail;
      if (!d) {
        aside.innerHTML = `<div class="dk-insp-head"><div><div class="dk-insp-title">Loading container…</div></div><button class="dk-btn is-ghost" type="button" data-act="close-insp">${ico("x")}</button></div>
        <div class="dk-insp-body"><div class="dk-skel" style="width:60%"></div><div class="dk-skel" style="width:85%"></div></div>`;
        return;
      }
      const acts = d.actions || [];
      const tab = S.insp.tab || "logs";
      let bodyHTML = "";
      if (tab === "summary") {
        const memStr = d.memoryBytes ? `${fmtBytes(d.memoryBytes)}${d.limitBytes ? " / " + fmtBytes(d.limitBytes) : ""}` : "—";
        bodyHTML = `<dl class="dk-dl">
          <dt>State</dt><dd>${esc(d.status || d.state)}</dd>
          <dt>Health</dt><dd>${esc(d.health || (d.hasHealthCheck ? "starting" : "no probe"))}</dd>
          <dt>Stack</dt><dd>${esc(displayStack(d.project))}</dd>
          <dt>CPU</dt><dd>${d.cpuPercent != null ? esc(d.cpuPercent.toFixed(1) + "%") : "—"}</dd>
          <dt>Memory</dt><dd>${esc(memStr)}</dd>
          <dt>Restarts</dt><dd>${esc(d.restartCount)}${d.oomKilled ? " (OOMKilled)" : ""}</dd>
          <dt>Exit code</dt><dd>${esc(d.exitCode)}</dd>
          <dt>Ports</dt><dd>${esc((d.ports || []).join(", ") || "none")}</dd>
          <dt>Networks</dt><dd>${esc((d.networkNames || []).join(", ") || "none")}</dd>
          <dt>Shell</dt><dd>${esc(d.shellCommand || "")}</dd>
        </dl>`;
      } else if (tab === "mounts") {
        const mounts = d.mounts || [];
        bodyHTML = mounts.length
          ? mounts.map((m) => `<div style="padding:8px 0;border-bottom:1px solid var(--dk-border)">
              <div style="font-weight:600;font-family:var(--dk-mono);font-size:12px">${esc(m.destination)} <span class="dk-badge">${m.rw ? "rw" : "ro"}</span></div>
              <div style="font-size:11.5px;color:var(--dk-muted);margin-top:2px;word-break:break-all">${esc(m.type)}: ${esc(m.name || m.source || "anonymous")}</div>
            </div>`).join("")
          : `<p style="color:var(--dk-muted)">No volumes or bind mounts attached.</p>`;
      } else {
        bodyHTML = renderLogViewer(d.logEntries || [], d.logsError);
      }

      aside.innerHTML = `<div class="dk-insp-head">
        <div>
          <div class="dk-insp-title">${esc(d.name)}</div>
          <div class="dk-insp-sub">${esc(d.image)}</div>
        </div>
        <button class="dk-btn is-ghost" type="button" data-act="close-insp" aria-label="Close inspector">${ico("x")}</button>
      </div>
      <div class="dk-insp-acts">
        ${acts.includes("start") ? `<button class="dk-btn is-primary" type="button" data-act="c-start" data-id="${esc(d.id)}">Start</button>` : ""}
        ${acts.includes("restart") ? `<button class="dk-btn" type="button" data-act="c-restart" data-id="${esc(d.id)}">Restart</button>` : ""}
        ${acts.includes("stop") ? `<button class="dk-btn is-danger" type="button" data-act="c-stop" data-id="${esc(d.id)}">Stop</button>` : ""}
        <button class="dk-btn" type="button" data-act="open-shell" data-cmd="${esc(d.shellCommand || "")}" data-name="${esc(d.name)}">Open shell</button>
      </div>
      <div class="dk-insp-tabs" role="tablist">
        ${[["logs", "Logs"], ["summary", "Summary"], ["mounts", "Mounts"]].map(([id, lbl]) => `<button class="dk-insp-tab" type="button" role="tab" data-insp-tab="${id}" aria-selected="${tab === id ? "true" : "false"}">${lbl}</button>`).join("")}
      </div>
      <div class="dk-insp-body">${bodyHTML}</div>`;
      return;
    }

    if (S.insp.mode === "stack-logs") {
      const sl = S.insp.stackLogs;
      aside.innerHTML = `<div class="dk-insp-head">
        <div>
          <div class="dk-insp-title">${esc(displayStack(S.insp.project))} logs</div>
          <div class="dk-insp-sub">${sl ? esc((sl.services || []).join(", ") || "All services") : "Loading multiplexed stream…"}</div>
        </div>
        <button class="dk-btn is-ghost" type="button" data-act="close-insp" aria-label="Close inspector">${ico("x")}</button>
      </div>
      <div class="dk-insp-body">${sl ? renderLogViewer(sl.entries || [], "") : `<div class="dk-skel" style="width:75%"></div>`}</div>`;
      return;
    }

    if (S.insp.mode === "diagnosis") {
      const dg = S.insp.diagnosis;
      aside.innerHTML = `<div class="dk-insp-head">
        <div>
          <div class="dk-insp-title">Diagnose ${esc(displayStack(S.insp.project))}</div>
          <div class="dk-insp-sub">${dg ? esc(dg.summary) : "Inspecting containers and probes…"}</div>
        </div>
        <button class="dk-btn is-ghost" type="button" data-act="close-insp" aria-label="Close inspector">${ico("x")}</button>
      </div>
      <div class="dk-insp-body">${dg ? renderDiagnosisBody(dg) : `<div class="dk-skel" style="width:80%"></div>`}</div>`;
    }
  }

  function renderLogViewer(entries, errText) {
    if (errText) {
      return `<p style="color:var(--dk-danger)">Logs unavailable: ${esc(errText)}</p>`;
    }
    const q = (S.insp.logQuery || "").trim().toLowerCase();
    const lvl = S.insp.logLevel || "all";
    const filtered = entries.filter((e) => {
      if (lvl === "WRN" && e.level !== "WRN" && e.level !== "ERR") return false;
      if (lvl === "ERR" && e.level !== "ERR") return false;
      if (!q) return true;
      return (e.message || "").toLowerCase().includes(q) || (e.service || "").toLowerCase().includes(q);
    });

    const linesHTML = filtered.length
      ? filtered.slice(-250).map((e) => {
          const shortTs = (e.ts || "").replace(/^.*T/, "").slice(0, 8);
          let msgHTML = esc(e.message || "");
          if (q) {
            const safeQ = q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
            msgHTML = msgHTML.replace(new RegExp(`(${safeQ})`, "ig"), "<mark>$1</mark>");
          }
          return `<div class="dk-log-line">
            ${shortTs ? `<span class="dk-log-ts">${esc(shortTs)}</span>` : ""}
            <span class="dk-log-lvl ${esc(e.level || "INF")}">${esc(e.level || "INF")}</span>
            ${S.insp.mode === "stack-logs" ? `<span class="dk-log-svc">${esc(e.service || "")}</span>` : ""}
            <span class="dk-log-msg">${msgHTML}</span>
          </div>`;
        }).join("")
      : `<div style="color:var(--dk-muted);padding:8px 0">${entries.length ? "No matching log lines." : "No log output yet."}</div>`;

    return `<div class="dk-log-bar">
      <div class="dk-log-bar-row">
        <input class="dk-log-input" id="dk-log-q" type="search" placeholder="Search log lines…" value="${esc(S.insp.logQuery || "")}" aria-label="Search logs">
      </div>
      <div class="dk-log-bar-row" data-align-row>
        ${[["all", "All"], ["WRN", "Warnings"], ["ERR", "Errors"]].map(([k, lbl]) => `<button class="dk-log-pill" type="button" data-log-lvl="${k}" aria-pressed="${lvl === k ? "true" : "false"}">${lbl}</button>`).join("")}
      </div>
    </div>
    <div class="dk-log-stream">${linesHTML}</div>
    <div class="dk-actions-row">
      <button class="dk-btn" type="button" data-act="copy-logs">Copy logs</button>
      <button class="dk-btn" type="button" data-act="send-excerpt">Send excerpt</button>
    </div>`;
  }

  function renderDiagnosisBody(dg) {
    const findings = dg.findings || [];
    if (!findings.length) {
      return `<p style="color:var(--dk-ok)">${esc(dg.summary)}</p>`;
    }
    return findings.map((f) => `<div style="padding-bottom:12px;border-bottom:1px solid var(--dk-border);display:grid;gap:6px">
      <div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">
        <span class="dk-badge ${f.severity === "critical" ? "is-danger" : f.severity === "warning" ? "is-warn" : ""}">${esc(f.severity)}</span>
        <b>${esc(f.title)}</b>
      </div>
      <div style="font-size:12.5px"><b>Observation:</b> ${esc(f.observation)}</div>
      <div style="font-size:12.5px;color:var(--dk-muted)"><b>Hypothesis:</b> ${esc(f.hypothesis)}</div>
      ${f.logExcerpt ? `<pre class="dk-log-stream" style="max-height:110px;min-height:0">${esc(f.logExcerpt)}</pre>` : ""}
      ${f.procedure ? `<div><button class="dk-btn is-primary" type="button" data-act="run-procedure" data-project="${esc(dg.project)}" data-proc="${esc(f.procedure)}">Run repair</button></div>` : ""}
    </div>`).join("");
  }

  // ---------- Data Loading ----------

  async function refreshAll(includeAux = false) {
    S.loading = true;
    try {
      const q = S.openPath ? `?path=${encodeURIComponent(S.openPath)}` : "";
      S.overview = await api("/overview" + q);
      if (includeAux || S.view === "resources") {
        S.resources = await api("/resources").catch(() => S.resources);
      }
      if (includeAux || S.view === "plans") {
        S.audit = await api("/audit").catch(() => S.audit);
      }
      if (S.insp) {
        if (S.insp.mode === "container" && S.insp.id) {
          S.insp.detail = await api("/containers/" + encodeURIComponent(S.insp.id)).catch(() => null);
          if (!S.insp.detail) S.insp = null;
        } else if (S.insp.mode === "stack-logs") {
          S.insp.stackLogs = await api("/stacks/logs?project=" + encodeURIComponent(S.insp.project || "")).catch(() => S.insp.stackLogs);
        }
      }
    } catch (e) {
      // A transport error means the extension's own process did not
      // answer; an error it sent back (a store error) is not "didn't start".
      S.overview = { reachable: false, helperFailed: !!(e && e.transport), error: e.message, groups: [], openIncidents: [], pendingPlans: [] };
    } finally {
      S.loading = false;
      drawMain();
      drawInspector();
    }
  }

  async function openContainerInspector(id) {
    S.insp = { mode: "container", id, tab: "logs", detail: null, logQuery: "", logLevel: "all" };
    drawMain();
    drawInspector();
    try {
      const d = await api("/containers/" + encodeURIComponent(id));
      if (S.insp && S.insp.mode === "container" && S.insp.id === id) {
        S.insp.detail = d;
        drawInspector();
      }
    } catch (e) {
      showToast(e.message);
      S.insp = null;
      drawInspector();
    }
  }

  async function openStackLogs(project) {
    S.insp = { mode: "stack-logs", project, stackLogs: null, logQuery: "", logLevel: "all" };
    drawInspector();
    try {
      const sl = await api("/stacks/logs?project=" + encodeURIComponent(project || ""));
      if (S.insp && S.insp.mode === "stack-logs" && S.insp.project === project) {
        S.insp.stackLogs = sl;
        drawInspector();
      }
    } catch (e) {
      showToast(e.message);
    }
  }

  async function openDiagnosis(project) {
    S.insp = { mode: "diagnosis", project, diagnosis: null };
    drawInspector();
    try {
      const dg = await api("/diagnosis", { project });
      if (S.insp && S.insp.mode === "diagnosis" && S.insp.project === project) {
        S.insp.diagnosis = dg;
        drawInspector();
      }
    } catch (e) {
      showToast(e.message);
    }
  }

  function currentLogExcerpt() {
    if (!S.insp) return "";
    const entries = S.insp.mode === "container"
      ? (S.insp.detail && S.insp.detail.logEntries) || []
      : (S.insp.stackLogs && S.insp.stackLogs.entries) || [];
    const q = (S.insp.logQuery || "").trim().toLowerCase();
    const lvl = S.insp.logLevel || "all";
    return entries
      .filter((e) => {
        if (lvl === "WRN" && e.level !== "WRN" && e.level !== "ERR") return false;
        if (lvl === "ERR" && e.level !== "ERR") return false;
        if (!q) return true;
        return (e.message || "").toLowerCase().includes(q) || (e.service || "").toLowerCase().includes(q);
      })
      .slice(-40)
      .map((e) => `[${e.level}] ${e.ts || ""} ${e.service || ""}: ${e.message}`)
      .join("\n");
  }

  function switchView(view) {
    S.view = view;
    hideToast();
    document.querySelectorAll(".dk-tab").forEach((b) => {
      b.setAttribute("aria-selected", b.dataset.view === view ? "true" : "false");
    });
    if (view === "resources" && !S.resources) void api("/resources").then((r) => { S.resources = r; drawMain(); }).catch(() => {});
    if (view === "plans" && !S.audit) void api("/audit").then((a) => { S.audit = a; drawMain(); }).catch(() => {});
    drawMain();
  }

  // ---------- Event Delegation ----------

  document.addEventListener("click", async (ev) => {
    const tabBtn = ev.target.closest(".dk-tab[data-view]");
    if (tabBtn) { switchView(tabBtn.dataset.view); return; }

    const filterBtn = ev.target.closest(".dk-filter-btn[data-filter]");
    if (filterBtn) { S.filter = filterBtn.dataset.filter; drawMain(); return; }

    const inspTab = ev.target.closest(".dk-insp-tab[data-insp-tab]");
    if (inspTab && S.insp) { S.insp.tab = inspTab.dataset.inspTab; drawInspector(); return; }

    const logLvl = ev.target.closest(".dk-log-pill[data-log-lvl]");
    if (logLvl && S.insp) { S.insp.logLevel = logLvl.dataset.logLvl; drawInspector(); return; }

    const actEl = ev.target.closest("[data-act]");
    if (!actEl) return;
    const act = actEl.dataset.act;

    if (act === "open-port") {
      ev.stopPropagation();
      const url = parsePortUrl(actEl.dataset.port);
      if (url && linksDoor) await linksDoor.open(url);
      return;
    }
    if (act === "inspect-container") {
      const id = actEl.dataset.id;
      if (S.insp && S.insp.mode === "container" && S.insp.id === id) {
        S.insp = null;
        drawMain();
        drawInspector();
      } else {
        await openContainerInspector(id);
      }
      return;
    }
    if (act === "close-insp") { S.insp = null; drawMain(); drawInspector(); return; }
    if (act === "toggle-group") {
      const key = actEl.dataset.key;
      S.collapsed[key] = !S.collapsed[key];
      drawMain();
      return;
    }
    if (act === "clear-filters") { S.search = ""; S.filter = "all"; drawMain(); return; }
    if (act === "retry") {
      actEl.disabled = true;
      actEl.textContent = "Checking…";
      await refreshAll(true);
      return;
    }
    if (act === "open-fix") { await linksDoor.open(actEl.dataset.url || ""); return; }
    if (act === "copy-fix") { await copyText(actEl.dataset.cmd || "", "Copied. Run it in a COGNIX[WS] terminal, then check again."); S.copiedFix = actEl.dataset.cmd || ""; drawMain(); return; }
    if (act === "go-plans") { switchView("plans"); return; }
    if (act === "go-stacks") { switchView("stacks"); return; }
    if (act === "open-ext-settings" && placesDoor) { await placesDoor.settings(); return; }
    if (act === "open-inbox" && placesDoor) { await placesDoor.inbox(actEl.dataset.inbox || ""); return; }
    if (act === "open-compose-modal") { openComposeModal(); return; }

    if (act === "c-start") { await startContainerDirect(actEl.dataset.id); return; }
    if (act === "c-stop") { await requestConfirmAndRun({ kind: "container", containerId: actEl.dataset.id, action: "stop" }); return; }
    if (act === "c-restart") { await requestConfirmAndRun({ kind: "container", containerId: actEl.dataset.id, action: "restart" }); return; }

    if (act === "open-shell") {
      const cmd = actEl.dataset.cmd || "";
      if (workspaceDoor) {
        try {
          await workspaceDoor.newAgent({ name: "Docker Shell", prompt: `Inspect container ${actEl.dataset.name || ""} using \`${cmd}\` and help me check its runtime state.` });
          return;
        } catch { /* fallback to copy when not in a workspace */ }
      }
      await copyText(cmd, `Copied: ${cmd}`);
      return;
    }

    if (act === "copy-logs") {
      await copyText(currentLogExcerpt() || "No logs", "Copied log excerpt.");
      return;
    }
    if (act === "send-excerpt") {
      const excerpt = currentLogExcerpt();
      if (workspaceDoor && excerpt) {
        try {
          await workspaceDoor.newAgent({ name: "Docker Logs", prompt: `Investigate these Docker logs:\n\n\`\`\`\n${excerpt.slice(0, 3000)}\n\`\`\`` });
          return;
        } catch { /* fallback to copy */ }
      }
      await copyText(excerpt || "No logs", "Copied log excerpt for your agent.");
      return;
    }

    if (act === "stack-logs") { await openStackLogs(actEl.dataset.project || ""); return; }
    if (act === "diagnose-stack") { await openDiagnosis(actEl.dataset.project || ""); return; }
    if (act === "stack-op") {
      await requestConfirmAndRun({ kind: "project", project: actEl.dataset.project || "", action: actEl.dataset.op });
      return;
    }
    if (act === "run-procedure") {
      await requestConfirmAndRun({ kind: "procedure", project: actEl.dataset.project || "", procedure: actEl.dataset.proc });
      return;
    }
    if (act === "approve-plan") {
      await requestConfirmAndRun({ kind: "approve-plan", planId: actEl.dataset.plan });
      return;
    }
    if (act === "remove-resource") {
      await requestConfirmAndRun({ kind: "resource", resourceKind: actEl.dataset.rkind, resourceId: actEl.dataset.rid });
      return;
    }
    if (act === "deploy-open-compose" && S.overview && S.overview.openComposeReg) {
      await requestConfirmAndRun({ kind: "compose", compose: S.overview.openComposeReg });
      return;
    }
    if (act === "deploy-registered" && S.overview) {
      const reg = (S.overview.composeProjects || []).find((r) => r.project === actEl.dataset.project);
      if (reg) await requestConfirmAndRun({ kind: "compose", compose: reg });
      return;
    }

    if (act === "check-health") {
      const proj = actEl.dataset.project || "";
      try {
        const res = await api("/health/check", { project: proj });
        S.healthByProject[proj] = res;
        showToast("Health sampled.");
        await refreshAll(false);
      } catch (e) { showToast(e.message); }
      return;
    }
    if (act === "toggle-monitor") {
      const proj = actEl.dataset.project || "";
      const sel = document.querySelector(`[data-mon-int="${CSS.escape(proj)}"]`);
      const intervalSeconds = sel ? Number(sel.value) : 60;
      const enableNext = actEl.dataset.enabled !== "1";
      try {
        const res = await api("/monitors", {
          project: proj,
          enabled: enableNext,
          intervalSeconds,
          cpuPercent: 90,
          memoryPercent: 85,
          badSamples: 3,
          retentionDays: 7,
          revision: Number(actEl.dataset.rev || 0),
        });
        S.healthByProject[proj] = { monitor: res.monitor };
        showToast(enableNext ? "Monitor enabled." : "Monitor paused.");
        await refreshAll(false);
      } catch (e) { showToast(e.message); }
    }
  });

  document.addEventListener("input", (ev) => {
    if (ev.target.id === "dk-search-in") {
      S.search = ev.target.value;
      drawMain();
    } else if (ev.target.id === "dk-log-q" && S.insp) {
      const pos = ev.target.selectionStart;
      S.insp.logQuery = ev.target.value;
      drawInspector();
      const next = $("dk-log-q");
      if (next) { next.focus(); if (pos != null) next.setSelectionRange(pos, pos); }
    }
  });

  document.addEventListener("keydown", (ev) => {
    if (ev.key === "/" && document.activeElement && !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement.tagName)) {
      const searchIn = $("dk-search-in");
      if (searchIn) { ev.preventDefault(); searchIn.focus(); }
    } else if ((ev.key === "Enter" || ev.key === " ") && document.activeElement?.dataset?.act === "inspect-container") {
      ev.preventDefault();
      document.activeElement.click();
    } else if (ev.key === "Escape" && S.insp && !$("dk-compose-dialog").open) {
      S.insp = null;
      drawMain();
      drawInspector();
    }
  });

  // ---------- Compose Dialog ----------

  const cmpDialog = $("dk-compose-dialog");
  function openComposeModal() {
    $("dk-cmp-err").hidden = true;
    $("dk-cmp-preview").hidden = true;
    if (S.openPath && !$("dk-cmp-dir").value) {
      $("dk-cmp-dir").value = S.openPath.replace(/[\\/][^\\/]+\.(ya?ml)$/i, "");
    }
    cmpDialog.showModal();
  }
  $("dk-open-compose-btn").onclick = openComposeModal;
  $("dk-refresh-btn").onclick = () => refreshAll(true);
  $("dk-cmp-cancel").onclick = () => cmpDialog.close();

  function readComposeForm() {
    const workDir = $("dk-cmp-dir").value.trim();
    const project = $("dk-cmp-proj").value.trim();
    const filesRaw = $("dk-cmp-files").value.trim();
    const files = filesRaw ? filesRaw.split(",").map((x) => x.trim()).filter(Boolean) : [];
    return { workDir, project, files };
  }

  $("dk-cmp-check").onclick = async () => {
    const errEl = $("dk-cmp-err");
    const prevEl = $("dk-cmp-preview");
    errEl.hidden = true;
    try {
      const form = readComposeForm();
      const prev = await api("/compose/preview", { ...form, register: true });
      if (!prev.valid) {
        errEl.textContent = prev.error || "Compose configuration invalid.";
        errEl.hidden = false;
        prevEl.hidden = true;
        return;
      }
      prevEl.textContent = (prev.services || []).map((s) => `${s.service}: ${s.action} (${s.image})`).join("\n") || "Configuration valid.";
      prevEl.hidden = false;
      await refreshAll(false);
    } catch (e) {
      errEl.textContent = e.message;
      errEl.hidden = false;
    }
  };

  $("dk-compose-form").onsubmit = async (ev) => {
    ev.preventDefault();
    const errEl = $("dk-cmp-err");
    errEl.hidden = true;
    try {
      const form = readComposeForm();
      if (!form.workDir) {
        errEl.textContent = "Enter a workspace folder containing compose.yml.";
        errEl.hidden = false;
        return;
      }
      cmpDialog.close();
      await requestConfirmAndRun({ kind: "compose", compose: form });
    } catch (e) {
      errEl.textContent = e.message;
      errEl.hidden = false;
    }
  };

  // ---------- Live Events & Context Wiring (Zero Polling Loop!) ----------

  function applyHostContext(c) {
    if (c && typeof c.path === "string" && c.path !== S.openPath) {
      S.openPath = c.path;
      void refreshAll(false);
    }
  }
  if (ctxDoor) {
    applyHostContext(ctxDoor.current());
    ctxDoor.onChange(applyHostContext);
  }

  let eventDebounce = null;
  if (eventsDoor) {
    eventsDoor.onChange(() => {
      if (document.hidden || window.innerWidth === 0) return;
      if (eventDebounce) clearTimeout(eventDebounce);
      eventDebounce = setTimeout(() => { void refreshAll(false); }, 300);
    });
  }

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && window.innerWidth > 0) {
      void refreshAll(false);
    }
  });

  await refreshAll(true);
})();
