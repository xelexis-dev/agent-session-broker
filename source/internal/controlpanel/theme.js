"use strict";
// Loaded synchronously in <head> so a saved theme applies before first paint.
// Without a saved choice the page follows prefers-color-scheme. Storage may be
// unavailable (private mode, blocked site data); the default then applies.
(() => {
  const root = document.documentElement;
  root.classList.add("js");
  try {
    const saved = localStorage.getItem("asb-theme");
    if (saved === "light" || saved === "dark") root.dataset.theme = saved;
  } catch {}
})();
