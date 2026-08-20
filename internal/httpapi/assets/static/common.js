"use strict";

// Helpers shared by every authenticated page: translation, the JSON transport
// and the toast. Loaded before the page specific script.

const csrf = document.querySelector('meta[name="csrf-token"]').content;
const el = (id) => document.getElementById(id);

let catalog = {};

/* ---------- i18n ---------- */

// Catalog keys are the English source strings, so an untranslated string still
// renders correctly in English.
function t(source, ...args) {
  return expand(catalog[source] || source, args);
}

// tm translates a {format, args} message produced by the backend.
function tm(message) {
  if (!message || !message.format) return "";
  return expand(catalog[message.format] || message.format, message.args || []);
}

function expand(format, args) {
  let i = 0;
  return format.replace(/%[sqdv]/g, (verb) => {
    const value = args[i++];
    if (value === undefined) return "";
    return verb === "%q" ? `"${value}"` : String(value);
  });
}

async function loadCatalog() {
  try {
    const res = await fetch("/i18n.json");
    const data = await res.json();
    catalog = data.messages || {};
  } catch {
    catalog = {};
  }
}

/* ---------- transport ---------- */

async function api(path, options = {}) {
  const init = { headers: { "X-CSRF-Token": csrf }, ...options };
  if (options.body !== undefined) {
    init.method = options.method || "POST";
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(options.body);
  }

  const res = await fetch(path, init);
  if (res.status === 401) {
    window.location.href = "/login";
    throw new Error("unauthenticated");
  }
  if (res.status === 204) return {};

  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const message = data.messageFormat
      ? tm({ format: data.messageFormat, args: data.messageArgs })
      : data.detail || data.title || t("HTTP error %s", res.status);
    throw new Error(message);
  }
  return data;
}

function notify(message, kind = "ok") {
  const toast = el("toast");
  toast.textContent = message;
  toast.className = `alert ${kind}`;
  toast.hidden = !message;
  if (message && kind === "ok") setTimeout(() => { toast.hidden = true; }, 5000);
}

async function withBusy(button, fn) {
  const previous = button?.textContent;
  if (button) { button.disabled = true; button.textContent = t("Working…"); }
  try {
    await fn();
  } catch (err) {
    if (err.message !== "unauthenticated") notify(err.message, "error");
  } finally {
    if (button) { button.disabled = false; button.textContent = previous; }
  }
}
