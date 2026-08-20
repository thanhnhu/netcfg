"use strict";

// el, t, tm, api, notify and withBusy come from common.js.

let state = { links: [], view: null };
let scanned = [];
let clockSkewMs = 0;
let countdownTimer = null;
let failoverViews = [];
// What the agent's failover monitor last reported, kept so an event can repaint
// the panel without a round trip.
let failoverStatus = {};
// Kept out of the DOM so tab switches can restore the banner without refetching.
let hotspotActive = false;
// A toast either describes what is currently on screen, or confirms something
// the operator just did. Only the first kind is dismissed when they look
// elsewhere; the second must survive the reload that follows an action.
let statusNoticeText = "";

function notifyStatus(message, kind) {
  notify(message, kind);
  statusNoticeText = message || "";
}

function clearStatusNotice() {
  if (statusNoticeText && el("toast").textContent === statusNoticeText) notify("");
  statusNoticeText = "";
}

const modeLabel = {
  dhcp: "Automatic (DHCP)",
  static: "Static",
  auto: "Automatic (RA / SLAAC)",
  disabled: "Disabled",
};

const securityLabel = {
  open: "Open",
  wep: "WEP (unsupported)",
  psk: "WPA2 Personal",
  sae: "WPA3 Personal",
  "psk-sae": "WPA2/WPA3",
};

/* ---------- state ---------- */

async function loadState(link) {
  const query = link ? `?link=${encodeURIComponent(link)}` : "";
  state = await api(`/api/v1/state${query}`);
  clockSkewMs = new Date(state.serverTime).getTime() - Date.now();

  renderLinks();
  renderStatus();
  renderProfiles();
  fillIPForm();
  renderPending(state.view.pending);
  renderHotspot(state.view.hotspot);

  clearStatusNotice();
  for (const notice of state.view.notices || []) notifyStatus(tm(notice), "warn");

  const wireless = state.view.link.wireless;
  document.querySelector('.tab[data-subtab="wifi"]').hidden = !wireless;
  if (!wireless && currentSubTab === "wifi") selectSubTab("ip");
}

function renderLinks() {
  const select = el("link");
  select.replaceChildren();
  const links = [...state.links].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { numeric: true }));
  for (const link of links) {
    const option = document.createElement("option");
    option.value = link.name;
    const tags = [];
    if (link.wireless) tags.push(t("Wi-Fi"));
    if (!link.operUp) tags.push(t("down"));
    option.textContent = tags.length ? `${link.name} (${tags.join(" · ")})` : link.name;
    option.selected = link.name === state.view.link.name;
    select.append(option);
  }
  window.syncListboxes?.();
}

function fillList(target, rows) {
  target.replaceChildren();
  for (const [key, value] of rows) {
    const dt = document.createElement("dt");
    dt.textContent = key;
    const dd = document.createElement("dd");
    dd.textContent = value;
    target.append(dt, dd);
  }
}

function renderStatus() {
  const view = state.view;
  const rows = [[t("Backend"), view.backend || t("unknown")]];
  if (view.wifi) {
    rows.push([t("Wi-Fi state"), view.wifi.state || "—"]);
    if (view.wifi.ssid) rows.push([t("SSID"), view.wifi.ssid]);
    if (view.wifi.signal) rows.push([t("Signal"), `${view.wifi.signal} dBm`]);
    if (view.wifi.freq) rows.push([t("Frequency"), `${view.wifi.freq} MHz`]);
  }
  rows.push([t("IPv4 mode"), t(modeLabel[view.ip.mode] || view.ip.mode)]);
  rows.push([t("IPv6 mode"), t(modeLabel[view.ip.mode6] || view.ip.mode6)]);
  rows.push([t("Addresses"), (view.link.addresses || []).join(", ") || t("none")]);
  if (view.link.gateway) rows.push([t("Gateway"), view.link.gateway]);
  if (view.ip.metric) rows.push([t("Route metric"), String(view.ip.metric)]);
  if ((view.link.dns || []).length) rows.push([t("DNS"), view.link.dns.join(", ")]);
  rows.push([t("MAC"), view.link.mac || "—"]);

  fillList(el("status"), rows);
  el("ip-backend").textContent = t("Managed by: %s", view.backend || t("unknown"));
  renderSystem();
}

/* ---------- pending / commit-confirm ---------- */

function changeList(changes) {
  const list = document.createElement("ul");
  list.className = "changes";
  for (const change of changes || []) {
    const li = document.createElement("li");
    const field = document.createElement("span");
    field.className = "field";
    field.textContent = change.field;
    const value = document.createElement("span");
    value.textContent = `${change.from || t("(empty)")} → ${change.to || t("(empty)")}`;
    li.append(field, value);
    list.append(li);
  }
  return list;
}

function renderPending(pending) {
  const panel = el("pending");
  clearInterval(countdownTimer);

  if (!pending) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;
  el("pending-title").textContent =
    pending.kind === "wifi" ? t("Wi-Fi change on %s", pending.link) : t("IP change on %s", pending.link);

  el("pending-changes").replaceChildren(...changeList(pending.summary).children);

  el("pending-probe").textContent = pending.probe?.detail
    ? t("Connectivity check: %s", tm(pending.probe.detail))
    : t("Checking connectivity…");

  const started = new Date(pending.startedAt).getTime();
  const deadline = new Date(pending.deadline).getTime();
  const total = Math.max(deadline - started, 1);

  const tick = () => {
    const remaining = deadline - (Date.now() + clockSkewMs);
    if (remaining <= 0) {
      el("pending-clock").textContent = t("rolling back…");
      el("pending-bar").style.width = "0%";
      clearInterval(countdownTimer);
      return;
    }
    el("pending-clock").textContent = `${Math.ceil(remaining / 1000)}s`;
    el("pending-bar").style.width = `${(remaining / total) * 100}%`;
  };
  tick();
  countdownTimer = setInterval(tick, 1000);

  el("pending-confirm").onclick = (event) => withBusy(event.target, async () => {
    await api(`/api/v1/pending/${pending.generation}/confirm`, { body: {} });
    notify(t("Change kept."));
    await loadState(state.view.link.name);
  });
  el("pending-rollback").onclick = (event) => withBusy(event.target, async () => {
    await api(`/api/v1/pending/${pending.generation}/rollback`, { body: {} });
    notify(t("Previous configuration restored."));
    await loadState(state.view.link.name);
  });
}

/* ---------- wifi ---------- */

function renderNetworks() {
  const list = el("networks");
  list.replaceChildren();

  if (!scanned.length) {
    const empty = document.createElement("li");
    empty.className = "muted";
    empty.textContent = t("No networks found.");
    list.append(empty);
    return;
  }

  const connected = state.view.wifi?.ssid;
  const saved = new Map((state.view.profiles || []).map((p) => [p.ssid, p]));
  for (const ap of scanned) {
    const li = document.createElement("li");

    const bar = document.createElement("div");
    bar.className = "bar";
    const fill = document.createElement("i");
    fill.style.width = `${ap.quality}%`;
    bar.append(fill);

    const info = document.createElement("div");
    info.className = "grow";
    const name = document.createElement("div");
    name.className = "name";
    name.textContent = ap.ssid;
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = `${t(securityLabel[ap.security] || ap.security)} · ${ap.band || ""} · ${ap.signal} dBm`;
    info.append(name, meta);

    if (ap.ssid === connected) {
      const badge = document.createElement("span");
      badge.className = "badge on";
      badge.textContent = t("Connected");
      info.append(badge);
    } else if (saved.has(ap.ssid)) {
      const badge = document.createElement("span");
      badge.className = "badge";
      badge.textContent = t("Saved");
      info.append(badge);
    }

    const action = document.createElement("button");
    action.disabled = ap.security === "wep";
    if (ap.ssid === connected) {
      action.className = "danger";
      action.textContent = t("Disconnect");
      action.addEventListener("click", () => withBusy(action, async () => {
        await api("/api/v1/disconnect", { body: { link: state.view.link.name } });
        notify(t("Wi-Fi disconnected."));
        await loadState(state.view.link.name);
      }));
    } else {
      action.className = "secondary";
      action.textContent = t("Connect");
      action.addEventListener("click", () => openConnectDialog(ap, saved.get(ap.ssid)));
    }

    li.append(bar, info, action);
    list.append(li);
  }
}

function renderProfiles() {
  const list = el("profiles");
  list.replaceChildren();

  const profiles = state.view.profiles || [];
  if (!profiles.length) {
    const empty = document.createElement("li");
    empty.className = "muted";
    empty.textContent = t("No saved networks yet.");
    list.append(empty);
    return;
  }

  for (const profile of profiles) {
    const li = document.createElement("li");

    const info = document.createElement("div");
    info.className = "grow";
    const name = document.createElement("div");
    name.className = "name";
    name.textContent = profile.ssid;
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = profile.current ? t("In use") : profile.enabled ? t("Enabled") : t("Disabled");
    info.append(name, meta);

    const use = document.createElement("button");
    use.className = "secondary";
    use.textContent = t("Use");
    use.addEventListener("click", () => withBusy(use, async () => {
      await api("/api/v1/profiles/select", { body: { link: state.view.link.name, id: profile.id } });
      notify(t("Switching to the selected network."));
    }));

    const secret = document.createElement("div");
    secret.className = "meta secret";
    secret.hidden = true;
    const reveal = document.createElement("button");
    reveal.className = "secondary";
    reveal.textContent = t("Show password");
    reveal.addEventListener("click", () => {
      if (!secret.hidden) {
        secret.hidden = true;
        secret.replaceChildren();
        reveal.textContent = t("Show password");
        return;
      }
      withBusy(reveal, async () => {
        const found = await api("/api/v1/profiles/secret", { body: { link: state.view.link.name, id: profile.id } });
        secret.replaceChildren();
        if (!found.value) {
          secret.textContent = t("This network has no password.");
        } else {
          const code = document.createElement("code");
          code.textContent = found.value;
          secret.append(code);
          if (found.hashed) {
            const note = document.createElement("div");
            note.textContent = t("Derived key, not the original password. It can still be used to join this network.");
            secret.append(note);
          }
        }
        secret.hidden = false;
        reveal.textContent = t("Hide password");
      });
    });

    const forget = document.createElement("button");    forget.className = "danger";
    forget.textContent = t("Forget");
    forget.addEventListener("click", () => {
      if (!confirm(t('Forget the network "%s"?', profile.ssid))) return;
      withBusy(forget, async () => {
        await api("/api/v1/profiles/remove", { body: { link: state.view.link.name, id: profile.id } });
        notify(t("Saved network removed."));
        await loadState(state.view.link.name);
      });
    });

    li.append(info, reveal);
    // Selecting the profile that is already associated would do nothing.
    if (!profile.current) li.append(use);
    li.append(forget, secret);
    list.append(li);
  }
}

/* ---------- connect dialog ---------- */

const dialog = el("connect-dialog");
let candidate = null;
let candidateProfile = null;

function openConnectDialog(ap, profile) {
  candidate = ap;
  candidateProfile = profile || null;
  el("connect-title").textContent = ap.ssid;
  el("connect-info").textContent = `${t(securityLabel[ap.security] || ap.security)} · ${ap.signal} dBm`;
  el("c-pass").value = "";
  el("c-pass").type = "password";
  const toggle = document.querySelector("#connect-pass-wrap .pw-toggle");
  if (toggle) {
    toggle.textContent = toggle.dataset.show;
    toggle.setAttribute("aria-pressed", "false");
  }
  el("c-norollback").checked = false;

  // A saved profile still holds the passphrase, so wpa_supplicant reuses it.
  const reusable = Boolean(candidateProfile) && ap.security !== "open";
  el("connect-saved").hidden = !reusable;
  el("connect-newpass").hidden = !reusable;
  el("connect-pass-wrap").hidden = ap.security === "open" || reusable;

  dialog.showModal();
  if (!el("connect-pass-wrap").hidden) el("c-pass").focus();
}

el("connect-newpass").addEventListener("click", () => {
  el("connect-newpass").hidden = true;
  el("connect-saved").hidden = true;
  el("connect-pass-wrap").hidden = false;
  el("c-pass").focus();
});

el("connect-cancel").addEventListener("click", () => dialog.close());

el("connect-ok").addEventListener("click", (event) => withBusy(event.target, async () => {
  if (candidateProfile && el("connect-pass-wrap").hidden && candidate.security !== "open") {
    await api("/api/v1/profiles/select", { body: { link: state.view.link.name, id: candidateProfile.id } });
    dialog.close();
    notify(t("Switching to the selected network."));
    await loadState(state.view.link.name);
    return;
  }

  const result = await api("/api/v1/wifi", {
    body: {
      link: state.view.link.name,
      ssid: candidate.ssid,
      security: candidate.security,
      passphrase: candidate.security === "open" ? "" : el("c-pass").value,
      hidden: false,
      confirmWindowSeconds: confirmWindow(),
      noRollback: el("c-norollback").checked,
    },
  });
  dialog.close();
  el("c-pass").value = "";
  afterApply(result);
}));

el("manual-form").addEventListener("submit", (event) => {
  event.preventDefault();
  withBusy(event.submitter, async () => {
    const security = el("m-sec").value;
    const result = await api("/api/v1/wifi", {
      body: {
        link: state.view.link.name,
        ssid: el("m-ssid").value,
        security,
        passphrase: security === "open" ? "" : el("m-pass").value,
        hidden: el("m-hidden").checked,
        confirmWindowSeconds: confirmWindow(),
        noRollback: el("m-norollback").checked,
      },
    });
    el("m-pass").value = "";
    afterApply(result);
  });
});

function afterApply(result) {
  if (result.warning) notify(tm(result.warning), "warn");
  if (result.pending) {
    renderPending(result.pending);
    notify(t("Applied. Confirm before the timer runs out to keep the change."), "warn");
  } else {
    notify(t("Change applied."));
  }
  setTimeout(() => loadState(state.view.link.name).catch(() => {}), 3000);
}

/* ---------- ip ---------- */

function ipFormBody() {
  const current = state.view.ip || {};
  return {
    link: state.view.link.name,
    mode: el("ip-mode").value,
    address: el("ip-address").value.trim(),
    gateway: el("ip-gateway").value.trim(),
    mode6: el("ip-mode6").value,
    address6: el("ip-address6").value.trim(),
    gateway6: el("ip-gateway6").value.trim(),
    // Failover lives on its own panel; carry the values through so an
    // addressing change does not silently reset them.
    metric: current.metric || 0,
    noDefaultRoute: !!current.noDefaultRoute,
    dns: el("ip-dns").value.split("\n").map((s) => s.trim()).filter(Boolean),
    confirmWindowSeconds: confirmWindow(),
    noRollback: el("ip-norollback").checked,
  };
}

function fillIPForm() {
  const ip = state.view.ip || {};
  el("ip-mode").value = ip.mode || "dhcp";
  el("ip-address").value = ip.address || "";
  el("ip-gateway").value = ip.gateway || "";
  el("ip-mode6").value = ip.mode6 || "auto";
  el("ip-address6").value = ip.address6 || "";
  el("ip-gateway6").value = ip.gateway6 || "";
  el("ip-dns").value = (ip.dns || []).join("\n");
  window.syncListboxes?.();
  toggleStaticFields();
}

function toggleStaticFields() {
  el("static-fields").hidden = el("ip-mode").value !== "static";
  el("static-fields6").hidden = el("ip-mode6").value !== "static";
}

el("ip-mode").addEventListener("change", toggleStaticFields);
el("ip-mode6").addEventListener("change", toggleStaticFields);

el("ip-form").addEventListener("submit", (event) => {
  event.preventDefault();
  withBusy(event.submitter, async () => {
    const body = ipFormBody();
    if (!(await confirmApply(body))) return;
    const result = await api("/api/v1/ip", { body });
    afterApply(result);
  });
});

/* ---------- hotspot ---------- */

function renderHotspot(status) {
  const rows = [];
  if (status?.active) {
    rows.push([t("SSID"), status.ssid]);
    rows.push([t("Password"), status.passphrase || "—"]);
    rows.push([t("Portal address"), status.portalUrl || status.address]);
    rows.push([t("Interface"), status.link]);
    rows.push([t("Mode"), status.mode === "exclusive" ? t("Exclusive (client role paused)") : t("Concurrent")]);
    rows.push([t("Connected clients"), String(status.clients ?? 0)]);
    if (status.reason) rows.push([t("Started because"), tm(status.reason)]);
  } else {
    rows.push([t("State"), status?.reason ? tm(status.reason) : t("Stopped")]);
  }

  fillList(el("hotspot-info"), rows);
  fillList(el("tools-hotspot"), rows);
  hotspotActive = !!status?.active;
  applyVisibility();
}

async function setHotspot(start, button) {
  if (start && !(await confirmHotspot())) return;
  await withBusy(button, async () => {
    const data = start
      ? await api("/api/v1/hotspot/start", { body: { link: state.view.link.wireless ? state.view.link.name : "" } })
      : await api("/api/v1/hotspot/stop", { body: {} });
    renderHotspot(data.status);
    notify(start ? t("Fallback access point started.") : t("Fallback access point stopped."));
  });
}

// Whether the radio can serve an AP and stay a client at once is only known
// once hostapd is asked, so the warning has to cover the worse outcome.
function confirmHotspot() {
  const body = document.createElement("div");
  const what = document.createElement("p");
  what.textContent = t("The device will broadcast its own Wi-Fi network with a captive portal.");
  const cost = document.createElement("p");
  cost.className = "muted small";
  cost.textContent = t("Most adapters cannot do this and stay connected to your Wi-Fi at the same time. If yours cannot, the client connection stops until the access point is turned off.");
  body.append(what, cost);

  const onWiFi = state?.view?.link?.wireless && state?.view?.wifi?.associated;
  return confirmDialog(body, onWiFi
    ? t("You are managing this device over Wi-Fi, so you may lose this page. Reach it again over the wired address or by joining the fallback network.")
    : "");
}

el("hotspot-start").addEventListener("click", (e) => setHotspot(true, e.target));
el("hotspot-stop").addEventListener("click", (e) => setHotspot(false, e.target));
el("hotspot-stop2").addEventListener("click", (e) => setHotspot(false, e.target));

/* ---------- failover ---------- */

// The agent tracks a single pending change, so each interface is applied on its
// own row rather than as one batch.
async function loadFailover() {
  const links = state.links.filter((link) => link.name !== "lo");
  const [views, failover] = await Promise.all([
    Promise.all(links.map((link) => api(`/api/v1/state?link=${encodeURIComponent(link.name)}`))),
    api("/api/v1/failover").catch(() => ({})),
  ]);
  failoverStatus = failover.status || {};
  // Sorting here settles both lists below: the rows read in a stable order, and
  // interfaces sharing a metric stop swapping places between refreshes.
  failoverViews = views.map((data) => data.view).filter(Boolean)
    .sort((a, b) => a.link.name.localeCompare(b.link.name, undefined, { numeric: true }));
  renderFailover();
}

function linkHealth(name) {
  return (failoverStatus.links || []).find((health) => health.link === name);
}

// healthBadge states the two things that decide whether a link can carry the
// default route: the kernel's view of the interface, and whether the path
// behind it actually answers.
function healthBadge(view) {
  const badge = document.createElement("span");
  badge.className = "badge";
  if (!view.link.operUp) {
    badge.classList.add("off");
    badge.textContent = t("down");
    return badge;
  }

  const health = linkHealth(view.link.name);
  if (!health || !failoverStatus.enabled) {
    badge.classList.add("on");
    badge.textContent = t("up");
    return badge;
  }
  if (health.demoted) {
    badge.classList.add("warn");
    badge.textContent = t("demoted");
    return badge;
  }
  badge.classList.add(health.reachable ? "on" : "off");
  badge.textContent = health.reachable ? t("reachable") : t("unreachable");
  return badge;
}

function renderFailover() {
  const monitor = el("failover-monitor");
  if (failoverStatus.enabled) {
    const targets = (failoverStatus.targets || []).join(", ");
    monitor.textContent = targets
      ? t("Active monitoring: every %s, %s failed checks demote a link. Probing %s.",
        formatSeconds(failoverStatus.interval), failoverStatus.fails, targets)
      : t("Active monitoring: every %s, %s failed checks demote a link. Probing each gateway.",
        formatSeconds(failoverStatus.interval), failoverStatus.fails);
  } else {
    monitor.textContent = failoverStatus.detail
      ? tm(failoverStatus.detail)
      : t("Active monitoring is off: the kernel still switches when a link goes down.");
  }

  const order = el("failover-order");
  order.replaceChildren();

  const routed = failoverViews
    .filter((view) => !view.ip.noDefaultRoute)
    .sort((a, b) => (a.ip.metric || Infinity) - (b.ip.metric || Infinity));

  if (!routed.length) {
    const li = document.createElement("li");
    li.className = "muted";
    li.textContent = t("No interface installs a default route.");
    order.append(li);
  }
  routed.forEach((view, index) => {
    const li = document.createElement("li");
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = `${index + 1}. ${view.link.name}`;
    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = view.ip.metric
      ? t("metric %s", view.ip.metric)
      // "none" is the domain's word for an unidentified backend, not a name.
      : t("metric decided by %s", view.backend && view.backend !== "none" ? view.backend : t("unknown"));
    li.append(name, meta);
    // The first healthy interface is the one traffic actually leaves by, which
    // is not always the first in the list once the monitor has demoted one.
    const health = linkHealth(view.link.name);
    if (view.link.operUp && !health?.demoted && !routed.slice(0, index).some(
      (other) => other.link.operUp && !linkHealth(other.link.name)?.demoted)) {
      const badge = document.createElement("span");
      badge.className = "badge on";
      badge.textContent = t("active");
      li.append(badge);
    }
    order.append(li);
  });

  const rows = el("failover-rows");
  rows.replaceChildren();
  for (const view of failoverViews) rows.append(failoverRow(view));
}

// formatSeconds turns the agent's nanosecond durations into something readable.
function formatSeconds(nanoseconds) {
  const seconds = Math.round((Number(nanoseconds) || 0) / 1e9);
  return seconds >= 60 ? t("%s min", Math.round(seconds / 60)) : t("%s s", seconds);
}

function failoverRow(view) {
  const row = document.createElement("div");
  row.className = "failover-row";

  const who = document.createElement("div");
  who.className = "who";
  const head = document.createElement("div");
  head.className = "who-head";
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = view.link.name;
  head.append(name, healthBadge(view));
  const meta = document.createElement("span");
  meta.className = "meta";
  const health = linkHealth(view.link.name);
  meta.textContent = [
    view.link.wireless ? t("Wi-Fi") : t("Wired"),
    view.link.operUp ? t("up") : t("down"),
    (view.link.addresses || []).join(", "),
    health && failoverStatus.enabled ? tm(health.detail) : "",
  ].filter(Boolean).join(" · ");
  who.append(head, meta);

  const metricLabel = document.createElement("label");
  metricLabel.className = "inline";
  metricLabel.append(document.createTextNode(t("Metric")));
  const metric = document.createElement("input");
  metric.type = "number";
  metric.min = "0";
  metric.max = "4294967295";
  metric.value = view.ip.metric || "";
  metric.placeholder = t("default");
  metricLabel.append(metric);

  const joinLabel = document.createElement("label");
  joinLabel.className = "check";
  const join = document.createElement("input");
  join.type = "checkbox";
  join.checked = !view.ip.noDefaultRoute;
  joinLabel.append(join, document.createTextNode(" " + t("In failover")));

  const apply = document.createElement("button");
  apply.className = "secondary accent";
  apply.textContent = t("Apply");
  apply.addEventListener("click", () => withBusy(apply, async () => {
    const body = failoverBody(view, metric.value, join.checked);
    if (!(await confirmApply(body))) return;
    const result = await api("/api/v1/ip", { body });
    afterApply(result);
    await loadFailover();
  }));

  row.append(who, metricLabel, joinLabel, apply);
  return row;
}

// failoverBody resends the link's whole addressing plan, because /api/v1/ip
// replaces it wholesale rather than patching single fields.
function failoverBody(view, metric, inFailover) {
  const ip = view.ip || {};
  return {
    link: view.link.name,
    mode: ip.mode || "dhcp",
    address: ip.address || "",
    gateway: ip.gateway || "",
    mode6: ip.mode6 || "auto",
    address6: ip.address6 || "",
    gateway6: ip.gateway6 || "",
    metric: Number(metric) || 0,
    noDefaultRoute: !inFailover,
    dns: ip.dns || [],
    confirmWindowSeconds: confirmWindow(),
    noRollback: el("failover-norollback").checked,
  };
}

// confirmApply dry runs the change first. A change that cannot be undone —
// either because it is harmless enough to commit outright, or because the
// operator turned rollback off — is the one worth showing before it happens.
async function confirmApply(body) {
  const diff = await api("/api/v1/plans", { body });
  if (!diff.changes?.length) {
    notify(t("Nothing would change."));
    return false;
  }
  if (!diff.disruptive && !body.noRollback) return true;

  const warning = diff.warning
    ? (body.noRollback
      ? `${tm(diff.warning)} ${t("Automatic rollback is off, so nothing will undo it for you.")}`
      : `${tm(diff.warning)} ${t("You then have %s seconds to confirm; without that the device restores the previous settings by itself.", confirmWindow())}`)
    : "";

  return confirmDialog(changeList(diff.changes), warning);
}

// confirmDialog shows body, an optional warning, and resolves to the answer.
function confirmDialog(body, warning) {
  const dialog = el("confirm-dialog");
  el("confirm-changes").replaceChildren(body);

  const warn = el("confirm-warning");
  warn.hidden = !warning;
  warn.textContent = warning || "";

  return new Promise((resolve) => {
    const finish = (answer) => {
      el("confirm-ok").removeEventListener("click", ok);
      el("confirm-cancel").removeEventListener("click", cancel);
      dialog.close();
      resolve(answer);
    };
    const ok = () => finish(true);
    const cancel = () => finish(false);
    el("confirm-ok").addEventListener("click", ok);
    el("confirm-cancel").addEventListener("click", cancel);
    dialog.showModal();
  });
}

/* ---------- system ---------- */

const windowKey = "netcfg.confirmWindow";

function confirmWindow() {
  const seconds = Number(el("sys-window").value);
  return Number.isFinite(seconds) && seconds >= 15 && seconds <= 600 ? seconds : 90;
}

el("sys-window").addEventListener("change", () => {
  el("sys-window").value = confirmWindow();
  try { localStorage.setItem(windowKey, String(confirmWindow())); } catch { /* private mode */ }
});

function restoreSystemPanel() {
  try {
    const saved = localStorage.getItem(windowKey);
    if (saved) el("sys-window").value = saved;
  } catch { /* private mode */ }
}

function renderSystem() {
  fillList(el("sys-info"), [
    [t("Backend"), state.view?.backend || t("unknown")],
    [t("Interfaces detected"), String(state.links.length)],
  ]);
}

/* ---------- system metrics ---------- */

let metricsTimer = null;

function formatBytes(bytes) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = Number(bytes) || 0;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value < 10 && unit > 0 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

function formatUptime(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days) return t("%s d %s h", days, hours);
  if (hours) return t("%s h %s min", hours, minutes);
  return t("%s min", minutes);
}

// meterRow renders one labelled percentage bar. Anything at or above 90% is
// flagged, which is the only reason an operator opens this panel in a hurry.
function meterRow(label, percent, detail) {
  const row = document.createElement("div");
  row.className = "meter-row";

  const head = document.createElement("div");
  head.className = "meter-head";
  const name = document.createElement("span");
  name.textContent = label;
  const value = document.createElement("span");
  value.className = "mono";
  value.textContent = `${percent.toFixed(0)}%${detail ? " · " + detail : ""}`;
  head.append(name, value);

  const meter = document.createElement("div");
  meter.className = "meter";
  const fill = document.createElement("i");
  fill.style.width = `${Math.min(100, Math.max(0, percent))}%`;
  if (percent >= 90) fill.classList.add("critical");
  else if (percent >= 75) fill.classList.add("warn");
  meter.append(fill);

  row.append(head, meter);
  return row;
}

function renderMetrics(stats) {
  const host = stats.host || {};
  // A VM publishes no board model and a container no DMI at all, so a row is
  // dropped rather than filled with "unknown".
  fillList(el("metrics-host"), [
    [t("Model"), host.model],
    [t("Operating system"), host.os],
    [t("Kernel"), [host.kernel, host.arch].filter(Boolean).join("  ")],
    [t("Processor"), host.cpuModel],
    [t("Host name"), host.hostname],
  ].filter(([, value]) => value));

  const cpu = el("metrics-cpu");
  cpu.replaceChildren(meterRow(t("Utilisation"), stats.cpuPercent || 0,
    t("%s cores", stats.cpuCount || "?")));
  cpu.append(kvList([
    [t("Load average"), (stats.load || []).map((v) => v.toFixed(2)).join("  ")],
    [t("Uptime"), formatUptime(stats.uptimeSeconds || 0)],
  ]));

  const memory = el("metrics-memory");
  memory.replaceChildren(meterRow(t("RAM"), stats.memory?.percent || 0,
    `${formatBytes(stats.memory?.used)} / ${formatBytes(stats.memory?.total)}`));
  if (stats.swap?.total) {
    memory.append(meterRow(t("Swap"), stats.swap.percent || 0,
      `${formatBytes(stats.swap.used)} / ${formatBytes(stats.swap.total)}`));
  }

  const storage = el("metrics-storage");
  storage.replaceChildren();
  for (const fs of stats.filesystems || []) {
    storage.append(meterRow(`${fs.mountpoint}  (${fs.device})`, fs.usage?.percent || 0,
      `${formatBytes(fs.usage?.used)} / ${formatBytes(fs.usage?.total)}`));
  }
  if (!(stats.filesystems || []).length) storage.append(mutedLine(t("No mounted volume reported.")));

  const temps = el("metrics-sensors");
  temps.replaceChildren();
  const groups = stats.sensors || [];
  if (!groups.length) {
    temps.append(mutedLine(t("This device exposes no sensor.")));
  }
  for (const group of groups) {
    // A chip with one probe already carries its name in the row label, so a
    // heading would just repeat it.
    if (group.sensors.length > 1) {
      const heading = document.createElement("h4");
      heading.className = "sensor-group";
      heading.textContent = group.name;
      temps.append(heading);
    }
    temps.append(sensorList(group.sensors));
  }

  el("metrics-age").textContent = t("updated %s", new Date(stats.at).toLocaleTimeString());
}

// Decimals follow the kind: an RPM reading with one decimal looks broken, a
// voltage without one is useless.
const sensorDecimals = { fan: 0, frequency: 0, charge: 0, power: 1, temperature: 1 };

function sensorList(sensors) {
  const list = document.createElement("dl");
  list.className = "status";

  for (const sensor of sensors) {
    const dt = document.createElement("dt");
    dt.textContent = sensor.label;

    const dd = document.createElement("dd");
    if (sensor.text) {
      dd.textContent = sensor.text;
    } else {
      const decimals = sensorDecimals[sensor.kind] ?? 2;
      dd.textContent = `${sensor.value.toFixed(decimals)} ${sensor.unit || ""}`.trim();
      if (sensor.critical && sensor.value >= sensor.critical) dd.className = "critical";
      else if (sensor.high && sensor.value >= sensor.high) dd.className = "warn";
    }
    list.append(dt, dd);
  }
  return list;
}

function kvList(rows) {
  const list = document.createElement("dl");
  list.className = "status";
  fillList(list, rows);
  return list;
}

function mutedLine(text) {
  const p = document.createElement("p");
  p.className = "muted small";
  p.textContent = text;
  return p;
}

async function loadMetrics() {
  renderMetrics((await api("/api/v1/system")).stats || {});
}

// Polling only runs while the panel is on screen; the readings are a live view,
// not something worth waking the agent for in the background.
function setMetricsPolling(on) {
  clearInterval(metricsTimer);
  metricsTimer = null;
  if (!on) return;

  withBusy(null, loadMetrics);
  metricsTimer = setInterval(() => loadMetrics().catch(() => {}), 5000);
}

/* ---------- remote access ---------- */

function renderSSH(status) {
  const rows = [];
  if (!status.available) {
    rows.push([t("SSH"), status.detail ? tm(status.detail) : t("unknown")]);
  } else {
    rows.push([t("SSH"), status.running ? t("running") : t("stopped")]);
    if (status.port) rows.push([t("Port"), String(status.port)]);
    rows.push([t("Starts at boot"), status.enabledAtBoot ? t("yes") : t("no")]);
    rows.push([t("Firewall"), status.firewall
      ? (status.firewallBlocks ? t("%s is active and blocks the port", status.firewall) : t("%s allows the port", status.firewall))
      : t("none")]);
  }
  fillList(el("ssh-info"), rows);

  // Without -allow-ssh the agent never looks at the SSH server, so claiming it
  // is closed would be a statement netcfg is in no position to make — and a
  // wrong one for the operator reading it over that very connection.
  const note = !status.available
    ? (status.detail ? tm(status.detail) : "")
    : status.stopsAt && new Date(status.stopsAt).getTime() > 0
      ? t("SSH is open. It closes automatically at %s.", new Date(status.stopsAt).toLocaleTimeString())
      : status.running
        ? (status.enabledAtBoot
          ? t("SSH is open and set to start at boot, so netcfgd will not close it.")
          : "")
        : t("SSH is closed.");
  notifyStatus(note, status.available && status.running ? "warn" : "ok");

  el("ssh-enable").disabled = !status.available;
  el("ssh-disable").disabled = !status.available || !status.running;
}

async function loadSSH() {
  renderSSH((await api("/api/v1/ssh")).status || {});
}

el("ssh-enable").addEventListener("click", (event) => withBusy(event.target, async () => {
  const result = await api("/api/v1/ssh/enable", { body: { windowMinutes: Number(el("ssh-window").value) || 30 } });
  renderSSH(result.status || {});
}));

el("ssh-disable").addEventListener("click", (event) => withBusy(event.target, async () => {
  const result = await api("/api/v1/ssh/disable", { body: {} });
  renderSSH(result.status || {});
}));

/* ---------- navigation ---------- */

// The header selects a section; the interface picker and its Wi-Fi / IP tabs
// only make sense inside the interface section, so they hide with it.
let currentTab = "network";
let currentSubTab = "wifi";

function applyVisibility() {
  for (const item of document.querySelectorAll(".navitem")) {
    item.classList.toggle("active", item.dataset.tab === currentTab);
  }
  for (const tab of document.querySelectorAll(".tab")) {
    tab.classList.toggle("active", tab.dataset.subtab === currentSubTab);
  }
  for (const panel of document.querySelectorAll(".tabpanel")) {
    const sub = panel.dataset.subpanel;
    panel.hidden = panel.dataset.panel !== currentTab || (sub !== undefined && sub !== currentSubTab);
  }
  el("interface-card").hidden = currentTab !== "network";
  // The access point banner belongs to the panel that can act on it; elsewhere
  // it is a card the operator cannot do anything about.
  el("hotspot").hidden = currentTab !== "tools" || !hotspotActive;
}

function selectTab(name) {
  currentTab = name;
  clearStatusNotice();
  applyVisibility();
  if (name === "failover") withBusy(null, loadFailover);
  if (name === "tools") withBusy(null, loadSSH);
  setMetricsPolling(name === "metrics");
}

function selectSubTab(name) {
  currentSubTab = name;
  clearStatusNotice();
  applyVisibility();
}

for (const item of document.querySelectorAll(".navitem")) {
  item.addEventListener("click", () => selectTab(item.dataset.tab));
}
for (const tab of document.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => selectSubTab(tab.dataset.subtab));
}

/* ---------- live events ---------- */

function connectEvents() {
  const source = new EventSource("/api/v1/events");
  const live = el("live");

  source.onopen = () => { live.textContent = t("online"); live.classList.add("on"); };
  source.onerror = () => { live.textContent = t("offline"); live.classList.remove("on"); };

  const reload = () => loadState(state.view?.link?.name).catch(() => {});

  source.addEventListener("apply_pending", (e) => renderPending(JSON.parse(e.data).data));
  source.addEventListener("apply_confirmed", () => { renderPending(null); reload(); });
  source.addEventListener("apply_reverted", (e) => {
    renderPending(null);
    notify(tm(JSON.parse(e.data).message), "warn");
    reload();
  });
  source.addEventListener("probe", (e) => {
    const evt = JSON.parse(e.data);
    if (!el("pending").hidden) el("pending-probe").textContent = t("Connectivity check: %s", tm(evt.message));
  });
  source.addEventListener("wifi_state", (e) => {
    const evt = JSON.parse(e.data);
    const wrongKey = evt.message?.format === "Wrong Wi-Fi password";
    if (evt.message) notify(tm(evt.message), wrongKey ? "error" : "ok");
    reload();
  });
  source.addEventListener("hotspot", (e) => {
    const evt = JSON.parse(e.data);
    renderHotspot(evt.data);
    if (evt.message) notify(tm(evt.message), "warn");
  });
  source.addEventListener("failover", (e) => {
    failoverStatus = JSON.parse(e.data).data || {};
    if (currentTab === "failover") renderFailover();
  });
  source.addEventListener("scan_results", () => {});
}

/* ---------- wiring ---------- */

el("link").addEventListener("change", (event) => {
  scanned = [];
  renderNetworks();
  withBusy(null, () => loadState(event.target.value));
});

el("refresh").addEventListener("click", (event) =>
  withBusy(event.target, () => loadState(state.view.link.name)));

el("scan").addEventListener("click", (event) => withBusy(event.target, async () => {
  const data = await api("/api/v1/scan", { body: { link: state.view.link.name } });
  scanned = data.networks || [];
  renderNetworks();
  notify(t("Found %s networks.", scanned.length));
}));

el("m-sec").addEventListener("change", () => {
  el("m-pass").disabled = el("m-sec").value === "open";
});

withBusy(null, async () => {
  await loadCatalog();
  restoreSystemPanel();
  applyVisibility();
  await loadState();
  connectEvents();
});
