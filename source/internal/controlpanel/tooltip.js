// Shared opt-in tooltip. Native top-layer popovers and external CSS anchor
// positioning keep descriptions outside scroll clipping without inline styles.
const instances = new WeakSet();
let sequence = 0;
let active = null;

export function initTooltips(root = document) {
  if (typeof HTMLElement.prototype.showPopover !== 'function') return;
  for (const wrapper of root.querySelectorAll('[data-tooltip]')) {
    if (instances.has(wrapper)) continue;
    const trigger = wrapper.querySelector('.asb-tooltip__trigger');
    const popup = wrapper.querySelector('[role="tooltip"]');
    if (!trigger || !popup) continue;
    instances.add(wrapper);
    if (!popup.id) popup.id = `asb-tooltip-${++sequence}`;
    trigger.setAttribute('aria-describedby', popup.id);
    let hover = false, focus = false, pinned = false, touch = false, timer;
    const isOpen = () => popup.matches(':popover-open');
    const close = () => {
      clearTimeout(timer);
      pinned = false;
      if (isOpen()) popup.hidePopover();
      if (active?.wrapper === wrapper) active = null;
    };
    const open = () => {
      clearTimeout(timer);
      if (active?.wrapper !== wrapper) active?.close();
      if (!isOpen()) popup.showPopover();
      active = { wrapper, close };
    };
    const maybeClose = () => {
      clearTimeout(timer);
      if (!hover && !focus && !pinned) timer = setTimeout(close, 100);
    };
    trigger.addEventListener('pointerenter', event => {
      if (event.pointerType === 'touch') return;
      hover = true; open();
    });
    trigger.addEventListener('pointerleave', () => { hover = false; maybeClose(); });
    popup.addEventListener('pointerenter', () => { hover = true; clearTimeout(timer); });
    popup.addEventListener('pointerleave', () => { hover = false; maybeClose(); });
    trigger.addEventListener('pointerdown', event => { touch = event.pointerType === 'touch'; });
    trigger.addEventListener('focus', () => { focus = true; if (!touch) open(); });
    trigger.addEventListener('blur', () => { focus = false; pinned = false; maybeClose(); });
    trigger.addEventListener('click', () => {
      if (pinned) close();
      else { pinned = true; open(); }
      touch = false;
    });
  }
}

document.addEventListener('keydown', event => {
  if (event.key === 'Escape' && active) { active.close(); event.preventDefault(); }
});
document.addEventListener('pointerdown', event => {
  if (active && !active.wrapper.contains(event.target)) active.close();
});
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => initTooltips());
else initTooltips();
