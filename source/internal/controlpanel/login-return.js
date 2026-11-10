"use strict";

// Fragments never enter next or the POST body. Each tab keeps one short-lived
// return anchor; failed logins retain it and a matching page consumes it once.
(() => {
  const key = "asb-login-return";
  const lifetime = 10 * 60 * 1000;
  const form = document.querySelector("form[data-login-return]");
  try {
    if (form) {
      const next = form.elements.namedItem("next")?.value;
      if (!next) { sessionStorage.removeItem(key); return; }
      const url = new URL(next, location.origin);
      if (!next.startsWith("/") || next.startsWith("//") || next.includes("\\") || url.origin !== location.origin || url.hash) {
        sessionStorage.removeItem(key);
        return;
      }
      const target = url.pathname + url.search;
      if (location.hash) {
        sessionStorage.setItem(key, JSON.stringify({ target, fragment: location.hash, expires: Date.now() + lifetime }));
        history.replaceState(null, "", location.pathname + location.search);
      } else {
        const saved = JSON.parse(sessionStorage.getItem(key) || "null");
        if (saved && (saved.target !== target || !Number.isFinite(saved.expires) || saved.expires <= Date.now())) sessionStorage.removeItem(key);
      }
      return;
    }
    const saved = JSON.parse(sessionStorage.getItem(key) || "null");
    if (!saved) return;
    if (!Number.isFinite(saved.expires) || saved.expires <= Date.now() ||
        typeof saved.fragment !== "string" || !saved.fragment.startsWith("#") || saved.fragment.length > 4096) {
      sessionStorage.removeItem(key);
      return;
    }
    if (saved.target !== location.pathname + location.search) return;
    sessionStorage.removeItem(key);
    if (!location.hash) location.replace(saved.fragment);
  } catch {
    // Storage denial/corruption must not interfere with native login.
  }
})();
