"use strict";

// el, t, api, notify and withBusy come from common.js.

el("password-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.target;
  withBusy(form.querySelector('button[type="submit"]'), async () => {
    const result = await api("/api/v1/password", {
      body: {
        current: el("pw-current").value,
        new: el("pw-new").value,
        confirm: el("pw-confirm").value,
      },
    });
    form.reset();
    notify(result.revokedSessions
      ? t("Password changed. %d other session(s) were signed out.", result.revokedSessions)
      : t("Password changed."));
  });
});

loadCatalog();
