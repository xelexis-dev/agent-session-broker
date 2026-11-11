"use strict";

(() => {
  const table = document.querySelector('[data-session-live]');
  if (!table) return;
  const endpoint = new URL(table.dataset.sessionLive, location.href);
  if (endpoint.origin !== location.origin || endpoint.pathname !== '/projects/sessions/live') return;
  const project = endpoint.searchParams.get('project');
  // Labels come back in this page's language, so a page never mixes languages after an account change.
  if (document.documentElement.lang === 'en' || document.documentElement.lang === 'ko') endpoint.searchParams.set('lang', document.documentElement.lang);
  const notice = document.querySelector('[data-session-live-status]');
  const rows = new Map(Array.from(table.querySelectorAll('[data-session-row]'), row => [row.dataset.sessionRow, row]));
  const states = new Set(['ready','waiting','unreachable','suspended','detached','unregistered']);
  const labels = {working:ASB.t('js.sessions.activity.working'),idle:ASB.t('js.sessions.activity.idle'),unknown:ASB.t('js.sessions.activity.unknown'),none:ASB.t('js.sessions.activity.none')};
  const expiredReason = ASB.t('js.sessions.activity.expired_reason');
  const activityAria = state => ASB.t('js.sessions.activity_aria',{label:labels[state] ?? labels.unknown});
  const evidence = new Map();
  const runtimeEvidence = new Map();
  const runtimeUnknown = ASB.t('js.sessions.runtime.unknown');
  const safeRuntime = (value,cap) => typeof value === "string" && value.length <= cap && /^[A-Za-z0-9._-]*$/.test(value);
  function runtimeLabel(info) {
    if (!info || !safeRuntime(info.model,96) || !safeRuntime(info.effort,16) || !info.model && !info.effort) return runtimeUnknown;
    return `${info.model || ASB.t('js.sessions.runtime.unknown_part')} / ${info.effort || ASB.t('js.sessions.runtime.unknown_part')}`;
  }
  let clock = {server:Date.parse(table.dataset.liveServerTime),base:0};
  const freshnessNow = () => Number.isFinite(clock.server) ? clock.server + performance.now() - clock.base : Date.now();
  const pad = value => String(value).padStart(2,'0');
  const clockTime = date => `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
  function duration(seconds) {
    if (seconds < 60) return ASB.t('js.sessions.duration.seconds',{n:seconds});
    if (seconds < 3600) return ASB.t('js.sessions.duration.minutes',{n:Math.floor(seconds/60)});
    if (seconds < 86400) return ASB.t('js.sessions.duration.hours',{n:Math.floor(seconds/3600)});
    return ASB.t('js.sessions.duration.days',{n:Math.floor(seconds/86400)});
  }
  function formatTimes(root = document) {
    const reference = freshnessNow(), today = new Date();
    for (const node of root.querySelectorAll('time[data-activity-time]')) {
      const date = new Date(node.getAttribute('datetime')), value = date.getTime();
      if (!Number.isFinite(value)) { node.textContent = ASB.t('js.time.unknown'); continue; }
      const sameDay = date.getFullYear() === today.getFullYear() && date.getMonth() === today.getMonth() && date.getDate() === today.getDate();
      const day = sameDay ? '' : `${date.getFullYear()}-${pad(date.getMonth()+1)}-${pad(date.getDate())} `;
      const local = `${day}${clockTime(date)}`;
      const delta = (value-reference)/1000;
      const relative = node.dataset.activityTime === 'expiresAt' ? (delta > 0 ? ASB.t('js.sessions.relative.expires_in',{duration:duration(Math.ceil(delta))}) : ASB.t('js.sessions.relative.expired')) :
        (delta > 0 ? ASB.t('js.sessions.relative.in',{duration:duration(Math.ceil(delta))}) : ASB.t('js.sessions.relative.ago',{duration:duration(Math.floor(-delta))}));
      node.textContent = `${local} · ${relative}`;
    }
  }

  let timer, expiryTimer, controller, sequence = 0, failures = 0, stopped = false, suspended = false;
  let pending, changed = false, lastSuccess = '';
  const visible = () => !document.hidden && !suspended;
  const busy = () => document.querySelector('dialog[open], form[aria-busy="true"]') ||
    table.querySelector('.cell-status:focus-within, [data-session-tooltip]:popover-open');
  const message = text => { if (notice) notice.textContent = text; };
  const disable = row => row.querySelectorAll('.cell-actions button, form button').forEach(button => { button.disabled = true; });
  const deadline = (expiresAt, serverTime, base = 0) => base + Math.max(0, Date.parse(expiresAt) - Date.parse(serverTime));
  for (const [key,row] of rows) {
    const badge = row.querySelector('[data-activity-state]');
    if (badge) evidence.set(key, {activity:{state:badge.dataset.activityState}, expires:deadline(badge.dataset.activityExpires, table.dataset.liveServerTime)});
  }
  for (const [key,row] of rows) {
    const node = row.querySelector("[data-runtime-info]");
    if (node?.dataset.runtimeExpires) runtimeEvidence.set(key,deadline(node.dataset.runtimeExpires,table.dataset.liveServerTime));
  }
  function tooltip(row, data) {
    const popup = row.querySelector('[data-session-tooltip]');
    if (!popup) return;
    const code = document.createElement('code'); code.textContent = data.status;
    const a = data.activity;
    const line = document.createElement('span'); line.dataset.activityLine = '';
    line.textContent = a.state === 'none' ? ASB.t('js.sessions.tooltip.activity_none') : ASB.t('js.sessions.tooltip.activity',{label:labels[a.state]});
    const lines = [data.reason, data.probePending ? ASB.t('js.sessions.tooltip.probe_pending') : '', line];
    if (a.state !== 'none') {
      if (a.source) lines.push(ASB.t('js.sessions.tooltip.source',{value:a.source}));
      if (a.reason) lines.push(ASB.t('js.sessions.tooltip.reason',{value:a.reason}));
    }
    popup.replaceChildren(code);
    for (const item of lines.filter(Boolean)) popup.append(document.createElement('br'), typeof item === 'string' ? document.createTextNode(item) : item);
    if (a.state !== 'none') for (const [label,kind] of [[ASB.t('js.sessions.tooltip.observed_at'),'observedAt'],[ASB.t('js.sessions.tooltip.changed_at'),'changedAt'],[ASB.t('js.sessions.tooltip.received_at'),'receivedAt'],[ASB.t('js.sessions.tooltip.expires_at'),'expiresAt']]) {
      const value = a[kind];
      if (!value || !Number.isFinite(Date.parse(value))) continue;
      const node = document.createElement('time'); node.setAttribute('datetime',value); node.dataset.activityTime = kind;
      popup.append(document.createElement('br'),document.createTextNode(`${label}: `),node);
    }
    formatTimes(popup);

  }
  function expire() {
    for (const [key,expires] of runtimeEvidence) if (performance.now() >= expires) {
      const node = rows.get(key)?.querySelector("[data-runtime-info]");
      if (node) node.textContent = runtimeUnknown;
      runtimeEvidence.delete(key);
    }
    for (const [key,item] of evidence) {
      if (!['working','idle'].includes(item.activity.state) || !(performance.now() >= item.expires)) continue;
      item.activity = {...item.activity,state:'unknown',label:labels.unknown,reason:expiredReason};
      const row = rows.get(key), badge = row?.querySelector('[data-activity-state]');
      if (badge) { badge.dataset.activityState = 'unknown'; badge.textContent = ''; badge.setAttribute('aria-label',activityAria('unknown')); }
      // Never disturb an open/focused tooltip. The badge still expires on time.
      if (item.data) item.data.activity = item.activity;
      item.tooltipDirty = true;
    }
    if (!busy()) for (const [key,item] of evidence) {
      if (!item.tooltipDirty) continue;
      const row = rows.get(key);
      if (item.data) tooltip(row,item.data);
      else {
        const popup = row?.querySelector('[data-session-tooltip]');
        if (popup) {
          const line = popup.querySelector('[data-activity-line]');
          if (line) line.textContent = ASB.t('js.sessions.tooltip.activity',{label:labels.unknown});
          popup.append(document.createElement('br'),document.createTextNode(ASB.t('js.sessions.tooltip.reason',{value:expiredReason})));
        }
      }
      item.tooltipDirty = false;
    }
  }
  function apply() {
    if (!pending || busy() || !visible()) return;
    const {data, base} = pending; pending = null;
    const keys = new Set(data.rows.map(item => item.key));
    if (keys.size !== rows.size || [...rows.keys()].some(key => !keys.has(key))) changed = true;
    for (const [key,row] of rows) if (!keys.has(key)) disable(row);
    for (const item of data.rows) {
      const row = rows.get(item.key);
      if (!row) continue;
      if (row.dataset.rowShape !== item.shape) { changed = true; disable(row); }
      const badge = row.querySelector('[data-connection-state]');
      if (badge) {
        badge.className = `badge state-badge state-badge--${item.displayState}`;
        const dot = document.createElement('span'); dot.className = 'badge__dot'; dot.setAttribute('aria-hidden','true');
        badge.replaceChildren(dot,document.createTextNode(item.connectionLabel));
      }
      const dot = row.querySelector('[data-session-dot]');
      if (dot) dot.className = `state-dot state-dot--${item.displayState}`;
      const activity = {...item.activity};
      const expires = deadline(activity.expiresAt,data.serverTime,base);
      if (['working','idle'].includes(activity.state) && (!Number.isFinite(expires) || performance.now() >= expires)) {
        activity.state = 'unknown'; activity.reason = expiredReason;
      }
      const activityBadge = row.querySelector('[data-activity-state]');
      if (activityBadge) {
        activityBadge.dataset.activityState = activity.state; activityBadge.dataset.activityExpires = activity.expiresAt || '';
        activityBadge.textContent = ''; activityBadge.setAttribute('aria-label',activityAria(activity.state));
      }
      const runtimeNode = row.querySelector("[data-runtime-info]");
      if (runtimeNode) {
        const expires = deadline(item.runtimeInfo?.expiresAt,data.serverTime,base);
        runtimeNode.textContent = item.activity.state === "none" ? "—" : Number.isFinite(expires) && performance.now() < expires ? runtimeLabel(item.runtimeInfo) : runtimeUnknown;
        runtimeNode.dataset.runtimeExpires = item.runtimeInfo?.expiresAt || "";
        if (Number.isFinite(expires)) runtimeEvidence.set(item.key,expires); else runtimeEvidence.delete(item.key);
      }
      const latest = {...item,activity};
      evidence.set(item.key,{activity,expires,data:latest}); tooltip(row,latest);
    }
    for (const node of document.querySelectorAll('[data-session-summary]')) {
      const count = data.summary?.[node.dataset.sessionSummary];
      if (Number.isSafeInteger(count) && count >= 0) node.textContent = String(count);
    }
    table.closest('.panel')?.querySelector('[data-table-filter]')?.dispatchEvent(new Event('input'));
    // A fixture/search input can sit outside the panel card too.
    if (!table.closest('.panel')) document.querySelector('[data-table-filter="session-table"]')?.dispatchEvent(new Event('input'));
    message(changed ? ASB.t('js.sessions.status.changed') : ASB.t('js.sessions.status.ok',{time:lastSuccess}));
  }
  function valid(data) {
    return data?.project === project && Number.isFinite(Date.parse(data.serverTime)) && Array.isArray(data.rows) && data.rows.length <= 4096 &&
      new Set(data.rows.map(row => row?.key)).size === data.rows.length && data.rows.every(row => typeof row.key === 'string' && typeof row.shape === 'string' &&
        states.has(row.displayState) && typeof row.status === 'string' && typeof row.connectionLabel === 'string' && typeof row.reason === 'string' &&
        Object.hasOwn(labels,row.activity?.state));
  }
  function schedule(delay) { clearTimeout(timer); if (visible() && !stopped) timer = setTimeout(poll,delay); }
  async function poll() {
    if (!visible() || stopped || controller) return;
    const own = new AbortController(), version = sequence, base = performance.now(); controller = own;
    const timeout = setTimeout(() => own.abort(),10000);
    try {
      const response = await fetch(endpoint,{signal:own.signal,credentials:'same-origin',cache:'no-store',redirect:'error',headers:{Accept:'application/json'}});
      if (version !== sequence || !visible()) return;
      if ([401,403,404].includes(response.status)) {
        stopped = true; pending = null; rows.forEach(disable);
        message(ASB.t('js.sessions.status.denied')); return;
      }
      if (!response.ok || response.redirected || !response.headers.get('Content-Type')?.startsWith('application/json')) throw new Error('status-unavailable');
      const data = await response.json();
      if (version !== sequence || !visible()) return;
      if (!valid(data)) throw new Error('invalid-status');
      clock = {server:Date.parse(data.serverTime),base};
      failures = 0; lastSuccess = clockTime(new Date()); pending = {data,base}; apply();
    } catch (_) {
      if (version !== sequence || !visible()) return;
      failures += 1;
      message(ASB.t('js.sessions.status.failed',{time:lastSuccess || ASB.t('js.sessions.status.never')}));
    } finally {
      clearTimeout(timeout);
      if (controller === own) controller = null;
      if (version === sequence) schedule(failures === 0 ? 5000 : [5000,10000,30000][Math.min(failures-1,2)]);
    }
  }
  function pause() { sequence += 1; clearTimeout(timer); clearInterval(expiryTimer); controller?.abort(); controller = null; pending = null; }
  function tick() { expire(); apply(); formatTimes(); }
  function resume() { if (!visible()) return; tick(); expiryTimer = setInterval(tick,1000); schedule(0); }
  document.addEventListener('visibilitychange',() => { pause(); if (!document.hidden) resume(); });
  window.addEventListener('pagehide',() => { suspended = true; pause(); });
  window.addEventListener('pageshow',event => { if (event.persisted || suspended) { suspended = false; pause(); resume(); } });
  document.addEventListener('close',() => setTimeout(apply,0),true);
  table.addEventListener('focusout',() => setTimeout(apply,150));
  table.addEventListener('pointerleave',() => setTimeout(apply,150));
  formatTimes();
  if (visible()) { expiryTimer = setInterval(tick,1000); schedule(5000); }
})();
