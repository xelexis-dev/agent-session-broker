"use strict";

(() => {
  const table = document.querySelector('[data-session-live]');
  if (!table) return;
  const endpoint = new URL(table.dataset.sessionLive, location.href);
  if (endpoint.origin !== location.origin || endpoint.pathname !== '/projects/sessions/live') return;
  const project = endpoint.searchParams.get('project');
  const notice = document.querySelector('[data-session-live-status]');
  const rows = new Map(Array.from(table.querySelectorAll('[data-session-row]'), row => [row.dataset.sessionRow, row]));
  const states = new Set(['ready','waiting','unreachable','suspended','detached','unregistered']);
  const labels = {working:'턴 진행',idle:'대기',unknown:'활동 확인 불가',none:'—'};
  const evidence = new Map();
  const runtimeEvidence = new Map();
  const runtimeUnknown = "모델·effort 알 수 없음";
  const safeRuntime = (value,cap) => typeof value === "string" && value.length <= cap && /^[A-Za-z0-9._-]*$/.test(value);
  function runtimeLabel(info) {
    if (!info || !safeRuntime(info.model,96) || !safeRuntime(info.effort,16) || !info.model && !info.effort) return runtimeUnknown;
    return `${info.model || "알 수 없음"} / ${info.effort || "알 수 없음"}`;
  }
  let clock = {server:Date.parse(table.dataset.liveServerTime),base:0};
  const freshnessNow = () => Number.isFinite(clock.server) ? clock.server + performance.now() - clock.base : Date.now();
  const pad = value => String(value).padStart(2,'0');
  function duration(seconds) {
    if (seconds < 60) return `${seconds}초`;
    if (seconds < 3600) return `${Math.floor(seconds/60)}분`;
    if (seconds < 86400) return `${Math.floor(seconds/3600)}시간`;
    return `${Math.floor(seconds/86400)}일`;
  }
  function formatTimes(root = document) {
    const reference = freshnessNow(), today = new Date();
    for (const node of root.querySelectorAll('time[data-activity-time]')) {
      const date = new Date(node.getAttribute('datetime')), value = date.getTime();
      if (!Number.isFinite(value)) { node.textContent = '시각 확인 불가'; continue; }
      const sameDay = date.getFullYear() === today.getFullYear() && date.getMonth() === today.getMonth() && date.getDate() === today.getDate();
      const day = sameDay ? '' : `${date.getFullYear()}-${pad(date.getMonth()+1)}-${pad(date.getDate())} `;
      const local = `${day}${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
      const delta = (value-reference)/1000;
      const relative = node.dataset.activityTime === 'expiresAt' ? (delta > 0 ? `만료까지 ${duration(Math.ceil(delta))}` : '만료됨') :
        (delta > 0 ? `${duration(Math.ceil(delta))} 후` : `${duration(Math.floor(-delta))} 전`);
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
    const lines = [data.reason, data.probePending ? '상태 응답 대기' : ''];
    const a = data.activity;
    lines.push(a.state === 'none' ? '활동: — · 등록 없음' : `활동: ${labels[a.state]} · 연결 상태와 다름`);
    if (a.state !== 'none') {
      for (const [label,value] of [['근거',a.source],['사유',a.reason]]) {
        if (value) lines.push(`${label}: ${value}`);
      }
    }
    popup.replaceChildren(code);
    for (const line of lines.filter(Boolean)) popup.append(document.createElement('br'), document.createTextNode(line));
    if (a.state !== 'none') for (const [label,kind] of [['관측 시각','observedAt'],['변경 시각','changedAt'],['서버 수신','receivedAt'],['신선도 만료','expiresAt']]) {
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
      item.activity = {...item.activity,state:'unknown',label:labels.unknown,reason:'활동 관측 만료'};
      const row = rows.get(key), badge = row?.querySelector('[data-activity-state]');
      if (badge) { badge.dataset.activityState = 'unknown'; badge.textContent = ''; badge.setAttribute('aria-label','활동: unknown'); }
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
          for (const node of popup.childNodes) if (node.nodeType === Node.TEXT_NODE && node.textContent.startsWith('활동:')) node.textContent = `활동: ${labels.unknown} · 연결 상태와 다름`;
          popup.append(document.createElement('br'),document.createTextNode('사유: 활동 관측 만료'));
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
        activity.state = 'unknown'; activity.reason = '활동 관측 만료';
      }
      const activityBadge = row.querySelector('[data-activity-state]');
      if (activityBadge) {
        activityBadge.dataset.activityState = activity.state; activityBadge.dataset.activityExpires = activity.expiresAt || '';
        activityBadge.textContent = ''; activityBadge.setAttribute('aria-label',`활동: ${activity.state}`);
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
    message(changed ? '목록 또는 관리 권한 변경 · 새로고침 필요' : `자동 갱신 · 마지막 성공 ${lastSuccess} · 활동은 연결 상태와 다릅니다.`);
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
        message('세션 조회 권한 또는 프로젝트를 확인할 수 없습니다. 새로고침해 주세요.'); return;
      }
      if (!response.ok || response.redirected || !response.headers.get('Content-Type')?.startsWith('application/json')) throw new Error('status-unavailable');
      const data = await response.json();
      if (version !== sequence || !visible()) return;
      if (!valid(data)) throw new Error('invalid-status');
      clock = {server:Date.parse(data.serverTime),base};
      failures = 0; lastSuccess = new Date().toLocaleTimeString('ko-KR'); pending = {data,base}; apply();
    } catch (_) {
      if (version !== sequence || !visible()) return;
      failures += 1;
      message(`자동 갱신 실패 · 마지막 성공 ${lastSuccess || '없음'} · 다시 확인 중입니다.`);
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
