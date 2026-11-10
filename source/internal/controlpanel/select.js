// Shared select for the control panel.
//
// Opt in with <select data-select>; add data-select-search="true" for a local
// search box. The native select stays the source of truth: it keeps its name,
// value, disabled and required state and is submitted with the form. Picking
// an option sets its value and fires bubbling input and change events. After
// changing a value from code, fire "change" on the select or call
// initSelects(root) again (it is idempotent and re-syncs).
//
// The list is a manual popover inside the select's own form or dialog, so it
// sits in the top layer above table scroll areas and modal dialogs. Its
// position is set at runtime as numeric CSSOM properties (top, left,
// min-width, max-height) on the popover element only; this writes a style
// attribute in the DOM but injects no style strings, so the CSP stays
// style-src 'self'. Without JavaScript or the Popover API the native select
// is left as it is.

const instances = new WeakMap();
let sequence = 0;

export function initSelects(root = document) {
  if (typeof HTMLElement === "undefined" || typeof HTMLElement.prototype.showPopover !== "function") return;
  for (const select of root.querySelectorAll("select[data-select]")) {
    if (select.multiple) continue;
    const existing = instances.get(select);
    if (existing) existing.sync();
    else instances.set(select, enhance(select));
  }
}

function labelText(select) {
  const own = select.getAttribute("aria-label");
  if (own) return own;
  const labelledby = select.getAttribute("aria-labelledby");
  if (labelledby) {
    const text = labelledby.split(/\s+/).map((id) => document.getElementById(id)?.textContent || "").join(" ").trim();
    if (text) return text;
  }
  const label = select.closest("label") || (select.id && document.querySelector(`label[for="${CSS.escape(select.id)}"]`));
  if (!label) return "";
  let text = "";
  for (const node of label.childNodes) {
    if (node.nodeType === Node.TEXT_NODE) text += node.textContent;
    else if (node.nodeType === Node.ELEMENT_NODE && !node.contains(select) && !node.matches("select, input, button, textarea, small")) text += node.textContent;
  }
  return text.trim();
}

function element(tag, className, attributes = {}) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  for (const [name, value] of Object.entries(attributes)) el.setAttribute(name, value);
  return el;
}

function enhance(select) {
  const id = `asb-select-${++sequence}`;
  const searchable = select.dataset.selectSearch === "true";
  const name = labelText(select);

  const wrapper = element("span", "select");
  select.before(wrapper);
  wrapper.append(select);
  const trigger = element("button", "select__trigger", { type: "button", role: "combobox", "aria-haspopup": "listbox", "aria-expanded": "false", "aria-controls": `${id}-list` });
  if (name) trigger.setAttribute("aria-label", name);
  const value = element("span", "select__value");
  trigger.append(value, element("span", "select__chevron", { "aria-hidden": "true" }));
  const popup = element("div", "select__popup", { popover: "manual", id: `${id}-popup` });
  let search = null;
  if (searchable) {
    const box = element("div", "select__search");
    search = element("input", "select__search-input", { type: "text", role: "combobox", "aria-autocomplete": "list", "aria-expanded": "false", "aria-controls": `${id}-list`, autocomplete: "off", spellcheck: "false", "aria-label": name ? `${name} 검색` : "검색", placeholder: "검색" });
    box.append(search);
    popup.append(box);
  }
  const list = element("div", "select__list", { role: "listbox", id: `${id}-list` });
  if (name) list.setAttribute("aria-label", name);
  const empty = element("div", "select__empty", { role: "status", "aria-live": "polite" });
  empty.hidden = true;
  popup.append(list, empty);
  const error = element("span", "select__error", { id: `${id}-error`, "aria-live": "polite" });
  error.hidden = true;
  wrapper.append(trigger, popup, error);

  // The native select stays for the form but leaves the tab order and the
  // accessibility tree; its label now points the user at the trigger.
  select.classList.add("select__native");
  select.tabIndex = -1;
  select.setAttribute("aria-hidden", "true");
  select.addEventListener("focus", () => trigger.focus());

  let options = [];
  let active = -1;
  let typed = "";
  let typedAt = 0;
  let composing = false;
  const activeOwner = () => search || trigger;

  function sync() {
    const selected = select.options[select.selectedIndex];
    value.textContent = selected ? selected.textContent : "";
    trigger.disabled = select.matches(":disabled");
    if (select.required) trigger.setAttribute("aria-required", "true");
    else trigger.removeAttribute("aria-required");
    const descriptions = (select.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
    if (!error.hidden) descriptions.push(error.id);
    if (descriptions.length) trigger.setAttribute("aria-describedby", [...new Set(descriptions)].join(" "));
    else trigger.removeAttribute("aria-describedby");
    if (trigger.disabled && popup.matches(":popover-open")) close(false);
  }

  function build() {
    list.replaceChildren();
    options = [...select.options].map((option, index) => {
      const item = element("div", "select__option", { role: "option", id: `${id}-option-${index}`, "aria-selected": String(option.selected) });
      item.textContent = option.textContent;
      if (option.disabled) item.setAttribute("aria-disabled", "true");
      item.addEventListener("pointerdown", (event) => event.preventDefault());
      item.addEventListener("click", () => choose(index, true));
      item.addEventListener("pointermove", () => setActive(index));
      list.append(item);
      return item;
    });
  }

  const visible = () => options.map((item, index) => index).filter((index) => !options[index].hidden && !select.options[index].disabled);

  function setActive(index) {
    if (active >= 0 && options[active]) options[active].classList.remove("is-active");
    active = index;
    const owner = activeOwner();
    if (index >= 0 && options[index]) {
      options[index].classList.add("is-active");
      owner.setAttribute("aria-activedescendant", options[index].id);
      options[index].scrollIntoView({ block: "nearest" });
    } else {
      owner.removeAttribute("aria-activedescendant");
    }
  }

  function move(step) {
    const order = visible();
    if (!order.length) return;
    const at = order.indexOf(active);
    const next = at < 0 ? (step > 0 ? 0 : order.length - 1) : Math.min(order.length - 1, Math.max(0, at + step));
    setActive(order[next]);
  }

  function filter() {
    const query = search.value.trim().toLocaleLowerCase();
    options.forEach((item) => { item.hidden = query !== "" && !item.textContent.toLocaleLowerCase().includes(query); });
    const order = visible();
    empty.hidden = order.length > 0;
    empty.textContent = order.length ? "" : "일치하는 항목이 없습니다.";
    setActive(order.length ? order[0] : -1);
  }

  function place() {
    const anchor = trigger.getBoundingClientRect();
    if (anchor.bottom < 0 || anchor.top > innerHeight) { close(false); return; }
    const gap = 4;
    const margin = 8;
    const below = innerHeight - anchor.bottom - gap - margin;
    const above = anchor.top - gap - margin;
    popup.style.maxHeight = "";
    const natural = popup.scrollHeight;
    const downward = below >= Math.min(natural, 160) || below >= above;
    const room = Math.max(96, downward ? below : above);
    const height = Math.min(natural, 320, room);
    const width = Math.max(anchor.width, popup.offsetWidth);
    const left = Math.min(Math.max(margin, anchor.left), Math.max(margin, innerWidth - width - margin));
    const top = downward ? anchor.bottom + gap : anchor.top - gap - height;
    const px = (n) => (Number.isFinite(n) ? `${Math.round(n)}px` : "");
    popup.style.minWidth = px(anchor.width);
    popup.style.maxHeight = px(Math.min(320, room));
    popup.style.left = px(left);
    popup.style.top = px(Math.max(margin, top));
  }

  function onOutside(event) {
    if (!wrapper.contains(event.target)) close(false);
  }
  function onViewport(event) {
    if (event.type === "scroll" && popup.contains(event.target)) return;
    place();
  }

  function open(focusIndex) {
    if (popup.matches(":popover-open") || select.matches(":disabled")) return;
    build();
    if (search) { search.value = ""; filter(); }
    popup.showPopover();
    trigger.setAttribute("aria-expanded", "true");
    if (search) search.setAttribute("aria-expanded", "true");
    place();
    document.addEventListener("pointerdown", onOutside, true);
    addEventListener("scroll", onViewport, true);
    addEventListener("resize", onViewport);
    const start = focusIndex ?? select.selectedIndex;
    setActive(start >= 0 && !options[start].hidden && !select.options[start].disabled ? start : (visible()[0] ?? -1));
    if (search) search.focus();
  }

  function close(restoreFocus) {
    if (!popup.matches(":popover-open")) return;
    setActive(-1);
    popup.hidePopover();
    trigger.setAttribute("aria-expanded", "false");
    if (search) search.setAttribute("aria-expanded", "false");
    document.removeEventListener("pointerdown", onOutside, true);
    removeEventListener("scroll", onViewport, true);
    removeEventListener("resize", onViewport);
    if (restoreFocus) trigger.focus();
  }

  function choose(index, restoreFocus) {
    if (index < 0 || !select.options[index] || select.options[index].disabled) { close(restoreFocus); return; }
    const changed = select.selectedIndex !== index;
    select.selectedIndex = index;
    if (changed) {
      select.dispatchEvent(new Event("input", { bubbles: true }));
      select.dispatchEvent(new Event("change", { bubbles: true }));
    }
    sync();
    close(restoreFocus);
  }

  function typeahead(key) {
    const now = Date.now();
    typed = now - typedAt > 600 ? key : typed + key;
    typedAt = now;
    const query = typed.toLocaleLowerCase();
    const order = visible();
    const start = Math.max(0, order.indexOf(active) + (typed.length === 1 ? 1 : 0));
    const ring = [...order.slice(start), ...order.slice(0, start)];
    const found = ring.find((index) => options[index].textContent.trim().toLocaleLowerCase().startsWith(query));
    if (found !== undefined) setActive(found);
  }

  function keydown(event) {
    const isOpen = popup.matches(":popover-open");
    const fromSearch = event.target === search;
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp":
        event.preventDefault();
        if (!isOpen) { open(); return; }
        move(event.key === "ArrowDown" ? 1 : -1);
        return;
      case "Home":
      case "End":
        if (fromSearch) return;
        event.preventDefault();
        if (!isOpen) open();
        { const order = visible(); if (order.length) setActive(event.key === "Home" ? order[0] : order[order.length - 1]); }
        return;
      case "Enter":
        event.preventDefault();
        if (!isOpen) { open(); return; }
        if (!composing) choose(active, true);
        return;
      case " ":
        if (fromSearch) return;
        event.preventDefault();
        if (!isOpen) open(); else choose(active, true);
        return;
      case "Escape":
        if (!isOpen) return;
        // Close only the list: keep a surrounding modal dialog open.
        event.preventDefault();
        event.stopPropagation();
        close(true);
        return;
      case "Tab":
        if (isOpen) {
          choose(active, false);
          trigger.focus({ preventScroll: true });
        }
        return;
      default:
        if (!fromSearch && event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
          if (!isOpen) open();
          typeahead(event.key);
        }
    }
  }

  trigger.addEventListener("click", () => (popup.matches(":popover-open") ? close(true) : open()));
  trigger.addEventListener("keydown", keydown);
  wrapper.addEventListener("focusout", (event) => {
    if (event.relatedTarget && !wrapper.contains(event.relatedTarget)) close(false);
  });
  if (search) {
    search.addEventListener("keydown", keydown);
    search.addEventListener("compositionstart", () => { composing = true; });
    search.addEventListener("compositionend", () => { composing = false; filter(); place(); });
    search.addEventListener("input", (event) => {
      if (composing || event.isComposing) return;
      filter();
      place();
    });
  }

  // A modal dialog's Esc (cancel) must not close the dialog while the list is open.
  const dialog = select.closest("dialog");
  if (dialog) dialog.addEventListener("cancel", (event) => { if (popup.matches(":popover-open")) { event.preventDefault(); close(true); } });

  // Native constraint validation: point the user at the visible trigger.
  select.addEventListener("invalid", (event) => {
    event.preventDefault();
    trigger.setAttribute("aria-invalid", "true");
    error.textContent = select.validationMessage || "값을 선택하세요.";
    error.hidden = false;
    sync();
    trigger.focus();
  });
  function resync() {
    if (select.checkValidity()) { trigger.removeAttribute("aria-invalid"); error.hidden = true; error.textContent = ""; }
    sync();
  }
  select.addEventListener("change", resync);
  // Reset restores the native value without events; re-sync after it ran.
  select.form?.addEventListener("reset", () => setTimeout(resync));

  // Disabled may come from the select or a surrounding fieldset.
  const observer = new MutationObserver(sync);
  observer.observe(select, { attributes: true, attributeFilter: ["disabled"] });
  const fieldset = select.closest("fieldset");
  if (fieldset) observer.observe(fieldset, { attributes: true, attributeFilter: ["disabled"] });

  sync();
  return { sync: resync };
}

if (typeof document !== "undefined") initSelects(document);
