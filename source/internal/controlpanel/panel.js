"use strict";

// Messages come from the page's JSON data block (html/template encodes it; no inline script runs). The other panel
// scripts load after this deferred script and use window.ASB.t. Replacement is a single pass, so values are never
// read as placeholders. A missing key shows the key, which the catalog tests keep out of releases.
const ASB = window.ASB = (() => {
  let messages = {};
  try { messages = JSON.parse(document.getElementById("asb-i18n")?.textContent || "{}"); } catch { messages = {}; }
  const lang = document.documentElement.lang || "en";
  function t(key, vars = {}) {
    let text = messages[key];
    if (text && typeof text === "object") text = vars.n === 1 && text.one ? text.one : text.other;
    if (typeof text !== "string") return key;
    return text.replace(/\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (match, name) => (Object.hasOwn(vars, name) ? String(vars[name]) : match));
  }
  return Object.freeze({ lang, t });
})();

// Keep the native submit path (including validation and submitter name/value).
// Window bubbling runs after confirmation handlers on the form or document.
const pendingSubmissions = new Map();

function restoreSubmission(form, pending) {
  clearTimeout(pending.timer);
  pending.buttons.forEach((button) => { button.disabled = false; });
  if (pending.busy === null) form.removeAttribute("aria-busy");
  else form.setAttribute("aria-busy", pending.busy);
  pendingSubmissions.delete(form);
}

window.addEventListener("submit", (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || event.defaultPrevented) return;
  const method = event.submitter?.getAttribute("formmethod") || form.method;
  if (method.toLowerCase() !== "post") return;
  if (pendingSubmissions.has(form)) {
    event.preventDefault();
    return;
  }
  const pending = { busy: form.getAttribute("aria-busy"), buttons: [], timer: null };
  pendingSubmissions.set(form, pending);
  form.setAttribute("aria-busy", "true");
  // Disabling in the submit event would omit the chosen button from POST data.
  // The lock above already blocks another submission, even in the same task.
  pending.timer = setTimeout(() => {
    if (event.defaultPrevented) {
      restoreSubmission(form, pending);
      return;
    }
    pending.buttons = Array.from(form.elements).filter((control) =>
      (control.type === "submit" || control.type === "image") && !control.disabled);
    pending.buttons.forEach((button) => { button.disabled = true; });
  }, 0);
});

window.addEventListener("pageshow", () => {
  pendingSubmissions.forEach((pending, form) => restoreSubmission(form, pending));
});

document.querySelectorAll("[data-dialog-open]").forEach((button) => {
  const dialog = document.getElementById(button.dataset.dialogOpen);
  if (!dialog) return;
  button.addEventListener("click", () => dialog.showModal());
  dialog.addEventListener("close", () => button.focus());
});
document.querySelectorAll("[data-dialog-close]").forEach((button) => {
  button.addEventListener("click", () => button.closest("dialog").close());
});
document.querySelectorAll("[data-dialog-reopen]").forEach((dialog) => {
  dialog.showModal();
});

// Confirm only actions with destructive effects. Submit on the form phase so a
// global submission guard sees defaultPrevented while the dialog is open.
const confirmationDialog = document.getElementById("confirm-action");
if (confirmationDialog) {
  let pendingConfirmation;
  const confirmedForms = new WeakSet();
  const roleRank = { viewer: 1, developer: 2, owner: 3 };
  const title = document.getElementById("confirm-action-title");
  const description = document.getElementById("confirm-action-description");
  const confirmButton = confirmationDialog.querySelector("[data-confirm-submit]");

  function confirmationFor(form) {
    if (form.hasAttribute("data-api-confirm")) return [form.dataset.confirmTitle, form.dataset.confirmDescription, form.dataset.confirmButton];
    const target = form.dataset.confirmTarget;
    switch (form.dataset.confirmAction) {
      case "attachments":
        return form.elements.namedItem("attachments").value === "true"
          ? [ASB.t("js.confirm.attachments_on_title"), ASB.t("js.confirm.attachments_on_body"), ASB.t("js.confirm.attachments_on_ok")]
          : [ASB.t("js.confirm.attachments_off_title"), ASB.t("js.confirm.attachments_off_body"), ASB.t("js.confirm.attachments_off_ok")];

      case "scope-issuance":
        if (form.elements.namedItem("enabled").value !== "true") return null;
        return [ASB.t("js.confirm.scope_issuance_title"), ASB.t("js.confirm.scope_issuance_body")];
      case "http-policy":
        if (form.elements.namedItem("allowHTTP").value !== "true") return null;
        return [ASB.t("js.confirm.http_title"), ASB.t("js.confirm.http_body")];
      case "broker-announcement": {
        const project = form.elements.namedItem("project");
        const scope = project.selectedOptions[0]?.textContent || ASB.t("js.all_projects");
        const body = form.elements.namedItem("body").value;
        return [ASB.t("js.confirm.announcement_title"), ASB.t("js.confirm.announcement_body", { scope, body })];
      }
      case "session-suspend":
        return [ASB.t("js.confirm.suspend_title"), ASB.t("js.confirm.suspend_body", { target })];
      case "token-revoke":
        return [ASB.t("js.confirm.token_revoke_title"), ASB.t("js.confirm.token_revoke_body", { target })];
      case "project-token-revoke":
        return [ASB.t("js.confirm.project_token_revoke_title"), ASB.t("js.confirm.project_token_revoke_body", { target })];
      case "session-grant-release":
        return [ASB.t("js.confirm.reservation_release_title"), ASB.t("js.confirm.reservation_release_body", { target })];
      case "session-detach":
        return [ASB.t("js.confirm.detach_title"), ASB.t("js.confirm.detach_body", { target })];
      case "member-remove":
        return [ASB.t("js.confirm.member_remove_title"), ASB.t("js.confirm.member_remove_body", { target })];
      case "member-role": {
        const role = form.elements.namedItem("role").value;
        if (roleRank[role] < roleRank[form.dataset.currentRole]) {
          return [ASB.t("js.confirm.role_demote_title"), ASB.t("js.confirm.role_demote_body", { target, from: form.dataset.currentRole, to: role })];
        }
        return null;
      }
      default:
        return null;
    }
  }

  document.querySelectorAll("form[data-confirm-action]").forEach((form) => {
    if (form.dataset.confirmAction === "member-role") {
      const role = form.elements.namedItem("role");
      const submit = form.querySelector("button:not([type=button])");
      const refresh = () => { submit.disabled = role.value === form.dataset.currentRole; };
      role.addEventListener("change", refresh);
      refresh();
    }
    form.addEventListener("submit", (event) => {
      if (event.defaultPrevented || confirmedForms.has(form)) return;
      const confirmation = confirmationFor(form);
      if (!confirmation) return;
      event.preventDefault();
      if (pendingConfirmation && confirmationDialog.open) return;
      pendingConfirmation = { form, submitter: event.submitter, trigger: event.submitter || document.activeElement };
      title.textContent = confirmation[0];
      description.textContent = confirmation[1];
      confirmButton.textContent = confirmation[2] || confirmation[0];
      confirmationDialog.showModal();
      confirmationDialog.querySelector("[data-dialog-close][autofocus]")?.focus();
    });
  });

  confirmationDialog.addEventListener("close", () => {
    // Native close events are queued; a new confirmation may already be open.
    if (confirmationDialog.open) return;
    const pending = pendingConfirmation;
    pendingConfirmation = null;
    pending?.trigger?.focus();
  });
  confirmButton.addEventListener("click", () => {
    const pending = pendingConfirmation;
    if (!pending) return;
    pendingConfirmation = null;
    confirmationDialog.close();
    pending.trigger?.focus();
    confirmedForms.add(pending.form);
    try {
      // Keep native validation, submitter data and the global double-submit guard.
      pending.form.requestSubmit(pending.submitter || undefined);
    } finally {
      // Validation can fail without dispatching submit; never leave a bypass armed.
      confirmedForms.delete(pending.form);
    }
  });
}

// Success feedback is rendered once after a completed save, including without JS.
document.querySelectorAll("[data-toast]").forEach((toast) => {
  const dismiss = () => toast.remove();
  const timer = setTimeout(dismiss, 6000);
  toast.querySelector("[data-toast-dismiss]")?.addEventListener("click", dismiss);
  const pause = () => clearTimeout(timer);
  toast.addEventListener("mouseenter", pause);
  toast.addEventListener("focusin", pause);
});

document.querySelectorAll("[data-member-autocomplete]").forEach((widget) => {
  const input = widget.querySelector('[role="combobox"]');
  const list = widget.querySelector('[role="listbox"]');
  const status = widget.querySelector('[role="status"]');
  let sequence = 0;
  let timer;
  let controller;
  let active = -1;
  let composing = false;

  function dismiss() {
    sequence += 1;
    clearTimeout(timer);
    if (controller) controller.abort();
    list.replaceChildren();
    list.hidden = true;
    active = -1;
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
  }

  function choose(option) {
    input.value = option.textContent;
    dismiss();
    status.textContent = ASB.t("js.member.selected", { name: input.value });
    input.focus();
  }

  function highlight(index) {
    active = index;
    Array.from(list.children).forEach((option, i) => {
      option.setAttribute("aria-selected", String(i === active));
    });
    const option = list.children[active];
    input.setAttribute("aria-activedescendant", option.id);
    option.scrollIntoView({ block: "nearest" });
  }

  function search() {
    dismiss();
    const query = input.value.trim();
    if (!query || composing) {
      status.textContent = ASB.t("js.member.type_more");
      return;
    }
    const requestSequence = sequence;
    status.textContent = ASB.t("js.member.searching");
    timer = setTimeout(async () => {
      controller = new AbortController();
      const params = new URLSearchParams({ project: widget.dataset.project, q: query });
      try {
        const response = await fetch(`/projects/members/suggestions?${params}`, {
          signal: controller.signal,
          credentials: "same-origin",
          headers: { Accept: "application/json" },
        });
        if (!response.ok || response.redirected) throw new Error("lookup failed");
        const data = await response.json();
        if (requestSequence !== sequence) return;
        if (!Array.isArray(data.usernames)) throw new Error("invalid lookup");
        const usernames = data.usernames.filter((name) => typeof name === "string").slice(0, 10);
        usernames.forEach((username, index) => {
          const option = document.createElement("li");
          option.id = `${list.id}-${index}`;
          option.className = "member-suggestion";
          option.setAttribute("role", "option");
          option.setAttribute("aria-selected", "false");
          option.textContent = username;
          // Keep input focus during pointer selection so blur cannot clear it.
          option.addEventListener("pointerdown", (event) => event.preventDefault());
          option.addEventListener("click", () => choose(option));
          list.append(option);
        });
        list.hidden = usernames.length === 0;
        input.setAttribute("aria-expanded", String(usernames.length > 0));
        status.textContent = usernames.length
          ? ASB.t("js.member.candidates", { n: usernames.length })
          : ASB.t("js.member.none");
      } catch (error) {
        if (requestSequence !== sequence || error.name === "AbortError") return;
        dismiss();
        status.textContent = ASB.t("js.member.load_failed");
      }
    }, 180);
  }

  input.addEventListener("input", search);
  input.addEventListener("compositionstart", () => { composing = true; dismiss(); });
  input.addEventListener("compositionend", () => { composing = false; search(); });
  input.addEventListener("focus", search);
  input.addEventListener("blur", dismiss);
  input.addEventListener("keydown", (event) => {
    if (composing || event.isComposing || list.hidden) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const count = list.children.length;
      highlight(event.key === "ArrowDown" ? (active + 1) % count : (active < 0 ? count - 1 : (active + count - 1) % count));
    } else if (event.key === "Enter" && active >= 0) {
      event.preventDefault();
      choose(list.children[active]);
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      dismiss();
      status.textContent = ASB.t("js.member.closed");
    } else if (event.key === "Tab") {
      dismiss();
    }
  });
  const dialog = widget.closest("dialog");
  if (dialog) dialog.addEventListener("close", dismiss);
  if (document.activeElement === input && input.value) search();
});

// Sidebar user menu: a disclosure that behaves like a menu button. Items are
// plain links and a form, so the page still works without this script.
document.querySelectorAll("[data-user-menu]").forEach((menu) => {
  const toggle = menu.querySelector("[data-user-menu-toggle]");
  const items = menu.querySelector('[role="menu"]');
  if (!toggle || !items) return;
  const entries = () => Array.from(items.querySelectorAll('[role="menuitem"]'));
  const close = (focusToggle) => {
    if (items.hidden) return;
    items.hidden = true;
    toggle.setAttribute("aria-expanded", "false");
    if (focusToggle) toggle.focus();
  };
  const open = (index) => {
    items.hidden = false;
    toggle.setAttribute("aria-expanded", "true");
    entries().at(index)?.focus();
  };
  toggle.addEventListener("click", () => (items.hidden ? open(0) : close(false)));
  toggle.addEventListener("keydown", (event) => {
    if (event.key === "ArrowDown") { event.preventDefault(); open(0); }
    if (event.key === "ArrowUp") { event.preventDefault(); open(-1); }
  });
  menu.addEventListener("keydown", (event) => {
    if (items.hidden) return;
    if (event.key === "Escape") { event.preventDefault(); close(true); return; }
    // The toggle's own arrow keys already opened and focused an item.
    if (!items.contains(event.target)) return;
    const list = entries();
    const current = list.indexOf(document.activeElement);
    if (event.key === "ArrowDown") { event.preventDefault(); list[(current + 1) % list.length]?.focus(); }
    else if (event.key === "ArrowUp") { event.preventDefault(); list[(current - 1 + list.length) % list.length]?.focus(); }
    else if (event.key === "Tab") close(false);
  });
  document.addEventListener("pointerdown", (event) => { if (!menu.contains(event.target)) close(false); });
});

// Client-side table filter for small server-rendered lists.
document.querySelectorAll("[data-table-filter]").forEach((input) => {
  const table = document.getElementById(input.dataset.tableFilter);
  if (!table) return;
  const status = input.closest(".table-tools")?.querySelector("[data-table-filter-status]");
  const rows = Array.from(table.tBodies[0]?.rows ?? []);
  const apply = () => {
    const query = input.value.trim().toLowerCase();
    let shown = 0;
    rows.forEach((row) => {
      const text = (row.dataset.filterText ?? row.textContent).toLowerCase();
      const match = !query || text.includes(query);
      row.hidden = !match;
      if (match) shown += 1;
    });
    if (status) status.textContent = query ? ASB.t("js.filter.count", { n: shown }) : "";
  };
  input.addEventListener("input", apply);
  apply();
});

// Whole-row navigation delegates to the row's real link, so keyboard users and
// scripts keep an ordinary anchor and controls inside the row keep working.
document.querySelectorAll("[data-row-links] tr.row-link").forEach((row) => {
  const link = row.querySelector("a.row-link-target");
  if (!link) return;
  row.addEventListener("click", (event) => {
    if (event.defaultPrevented || event.target.closest("a, button, input, select, form, label")) return;
    if (window.getSelection()?.toString()) return;
    link.click();
  });
});

// Automatic admission limits submit only when the Owner selects auto scope.
// The server independently checks the gate, issuer role, and explicit unlimited choices.
document.querySelectorAll("[data-auto-scope]").forEach((checkbox) => {
  const limits = checkbox.form?.querySelector("[data-auto-limits]");
  if (!limits) return;
  const syncLimits = () => { limits.disabled = checkbox.matches(":disabled") || !checkbox.checked; };
  checkbox.addEventListener("change", syncLimits);
  window.addEventListener("pageshow", syncLimits);
  syncLimits();
});

// Approval labels and codes accept pasted surrounding whitespace only.
document.querySelectorAll('form[action="/auth/code"]').forEach((form) => {
  const inputs = Array.from(form.querySelectorAll('input[name="code"], input[name="nickname"]'));
  const trim = (input) => { input.value = input.value.trim(); };
  inputs.forEach((input) => input.addEventListener("blur", () => trim(input)));
  form.addEventListener("submit", () => inputs.forEach(trim));
});

// Keep machine timestamps for accessibility; display audit times in the user's
// local timezone, with whole seconds and an explicit date.
for (const node of document.querySelectorAll('time[data-local-time]')) {
  const date = new Date(node.getAttribute('datetime'));
  if (!Number.isFinite(date.getTime())) { node.textContent = ASB.t('js.time.unknown'); continue; }
  const pad = value => String(value).padStart(2,'0');
  node.textContent = `${date.getFullYear()}-${pad(date.getMonth()+1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

// Scope review/back use the real server validation without adding history entries.
function initializeScopeEditor(dialog, savedScroll = 0, backgroundScroll) {
  if (!dialog) return;
  const form = dialog.querySelector('[data-scope-form]');
  const heading = dialog.querySelector('#scope-editor-title');
  const next = form.querySelector('[data-scope-next]');
  const apply = form.querySelector('[data-scope-apply]');
  const boxes = [...form.querySelectorAll('input[type="checkbox"][name="scope"]')];
  let pending = false;
  let replacing = false;
  let requestController;
  const isReview = dialog.dataset.step === 'review';
  const update = () => {
    const added = boxes.filter(x => x.checked && x.dataset.scopeBaseline !== 'true').map(x => x.value).sort();
    const removed = boxes.filter(x => !x.checked && x.dataset.scopeBaseline === 'true').map(x => x.value).sort();
    boxes.forEach(box => {
      const delta = box.closest('.check').querySelector('[data-scope-delta]');
      const changed = box.checked !== (box.dataset.scopeBaseline === 'true');
      delta.textContent = changed ? (box.checked ? ASB.t('js.scope.added') : ASB.t('js.scope.removed')) : '';
      delta.classList.toggle('is-removed', changed && !box.checked);
    });
    form.querySelector('[data-scope-added]').textContent = ASB.t('js.scope.added_list', { count: added.length, list: added.join(', ') || ASB.t('js.scope.none') });
    form.querySelector('[data-scope-removed]').textContent = ASB.t('js.scope.removed_list', { count: removed.length, list: removed.join(', ') || ASB.t('js.scope.none') });
    form.querySelector('[data-scope-receive-added]').hidden = !added.includes('message:receive');
    form.querySelector('[data-scope-receive-removed]').hidden = !removed.includes('message:receive');
    const noop = added.length + removed.length === 0;
    form.querySelector('[data-scope-summary]').classList.toggle('is-noop', noop);
    form.querySelector('[data-scope-noop]').hidden = !noop;
    form.querySelector('[data-scope-added]').hidden = noop;
    form.querySelector('[data-scope-removed]').hidden = noop;
    form.querySelector('[data-scope-impact]').hidden = noop;
    next.disabled = pending || added.length + removed.length === 0;
    apply.disabled = !isReview;
  };
  boxes.forEach(box => box.addEventListener('change', update));
  dialog.addEventListener('keydown', event => {
    if (event.key !== 'Tab') return;
    const targets = [...dialog.querySelectorAll('a[href], button, input, select, textarea, [tabindex="0"]')]
      .filter(node => !node.disabled && node.getClientRects().length);
    const first = targets[0], last = targets.at(-1);
    if (event.shiftKey && (document.activeElement === first || document.activeElement === heading)) {
      event.preventDefault(); last?.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault(); first?.focus();
    }
  });
  dialog.querySelectorAll('[data-scope-close]').forEach(link => link.addEventListener('click', event => {
    event.preventDefault(); dialog.close();
  }));
  dialog.addEventListener('close', () => {
    if (replacing) return;
    requestController?.abort();
    document.querySelectorAll(".scope-editor-selected").forEach(row => row.classList.remove("scope-editor-selected"));
    history.replaceState(null, '', dialog.dataset.closeUrl);
    const origin = document.querySelector(`[data-scope-edit="${CSS.escape(dialog.dataset.sourceToken)}"]`) || document.getElementById('project-token-title');
    window.scrollTo({top:backgroundScroll, behavior:'instant'});
    origin?.focus({preventScroll:window.innerWidth > 600});
  });
  form.addEventListener('submit', async event => {
    let intent = event.submitter?.value || 'review';
    if (!isReview) intent = 'review';
    if (pending) { event.preventDefault(); return; }
    if (isReview && intent === 'apply') return;
    event.preventDefault();
    pending = true;
    const body = new URLSearchParams(new FormData(form));
    body.set('intent', intent);
    if (!isReview) savedScroll = dialog.scrollTop;
    form.setAttribute('aria-busy', 'true');
    const controls = [...form.elements].map(control => ({control, disabled:control.disabled}));
    controls.forEach(({control}) => { control.disabled = true; });
    requestController = new AbortController();
    try {
      const response = await fetch(form.action, {method:'POST', body, credentials:'same-origin', signal:requestController.signal});
      const parsed = new DOMParser().parseFromString(await response.text(), 'text/html');
      const fresh = parsed.querySelector('[data-scope-editor]');
      if (!fresh) {
        dialog.close();
        location.assign(dialog.dataset.closeUrl);
        return;
      }
      const imported = document.importNode(fresh, true);
      replacing = true;
      dialog.close();
      dialog.replaceWith(imported);
      initializeScopeEditor(imported, savedScroll, backgroundScroll);
    } catch {
      if (!dialog.open) return;
      controls.forEach(({control, disabled}) => { control.disabled = disabled; });
      let notice = dialog.querySelector('[data-scope-network-error]');
      if (!notice) {
        notice = document.createElement('div');
        notice.className = 'notice error'; notice.setAttribute('role', 'alert');
        notice.dataset.scopeNetworkError = '';
        heading.closest('.section-heading').after(notice);
      }
      notice.textContent = ASB.t('js.scope.review_failed');
      pending = false;
      form.removeAttribute('aria-busy');
      update();
    }
  });
  update();
  if (backgroundScroll === undefined) {
    if (window.innerWidth > 600) {
      document.querySelector(`[data-token-row="${CSS.escape(dialog.dataset.sourceToken)}"]`)?.scrollIntoView({block:'center', behavior:'instant'});
    } else {
      window.scrollTo({top:0, behavior:'instant'});
    }
    backgroundScroll = window.scrollY;
  }
  dialog.removeAttribute('open');
  dialog.showModal();
  const errorNotice = dialog.querySelector('.notice.error[role="alert"]');
  (errorNotice || heading).focus({preventScroll:true});
  window.scrollTo({top:backgroundScroll, behavior:'instant'});
  const drawerScroll = isReview || errorNotice ? 0 : savedScroll;
  dialog.scrollTop = drawerScroll;
  requestAnimationFrame(() => { dialog.scrollTop = drawerScroll; });
}
initializeScopeEditor(document.querySelector('[data-scope-editor]'));

const issuedAPI = document.getElementById("api-issued");
if (issuedAPI) {
  document.getElementById("api-issued-title")?.focus();
  issuedAPI.querySelector("[data-copy-token]")?.addEventListener("click", async () => {
    const token = issuedAPI.querySelector("#issued-token");
    const status = issuedAPI.querySelector("[data-copy-status]");
    try {
      await navigator.clipboard.writeText(token.textContent);
      status.textContent = ASB.t("js.copy.copied");
    } catch {
      const range = document.createRange();
      range.selectNodeContents(token);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      token.focus();
      status.textContent = ASB.t("js.copy.selected");
    }
  });
}
