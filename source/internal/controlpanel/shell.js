"use strict";

// Page shell controls. Both buttons ship hidden and appear only here, so a
// page without JavaScript keeps the system theme and a static navigation.
const root = document.documentElement;

function effectiveTheme() {
  if (root.dataset.theme === "light" || root.dataset.theme === "dark") return root.dataset.theme;
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function syncThemeButtons() {
  const dark = effectiveTheme() === "dark";
  document.querySelectorAll("[data-theme-toggle]").forEach((button) => button.setAttribute("aria-pressed", String(dark)));
}

document.querySelectorAll("[data-theme-toggle]").forEach((button) => {
  button.hidden = false;
  button.addEventListener("click", () => {
    const next = effectiveTheme() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try {
      localStorage.setItem("asb-theme", next);
    } catch {}
    syncThemeButtons();
  });
});
syncThemeButtons();
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", syncThemeButtons);

const menuToggle = document.querySelector("[data-mobile-menu-toggle]");
const sidebar = menuToggle && document.getElementById(menuToggle.getAttribute("aria-controls"));
if (menuToggle && sidebar) {
  const setOpen = (open, restoreFocus) => {
    sidebar.classList.toggle("is-open", open);
    menuToggle.setAttribute("aria-expanded", String(open));
    if (open) sidebar.querySelector("a[href], button:not([hidden])")?.focus();
    else if (restoreFocus) menuToggle.focus();
  };
  menuToggle.hidden = false;
  menuToggle.addEventListener("click", () => setOpen(menuToggle.getAttribute("aria-expanded") !== "true", false));
  sidebar.querySelectorAll("[data-mobile-menu-close]").forEach((button) => {
    button.hidden = false;
    button.addEventListener("click", () => setOpen(false, true));
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && sidebar.classList.contains("is-open")) setOpen(false, true);
  });
}
