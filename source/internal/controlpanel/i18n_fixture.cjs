// Builds the page's JSON message block from the real catalogs, the way the server does (i18n.JS), so script tests
// run panel.js/select.js with the shipped messages. Korean keeps the existing assertions; English covers the default.
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, '..', 'i18n');

function catalog(lang) {
  const out = {};
  for (const name of fs.readdirSync(path.join(root, lang)).filter(n => n.endsWith('.json')).sort()) {
    Object.assign(out, JSON.parse(fs.readFileSync(path.join(root, lang, name), 'utf8')));
  }
  return out;
}

function jsMessages(lang) {
  const english = catalog('en'), own = catalog(lang), out = {};
  for (const [key, value] of Object.entries(english)) if (key.startsWith('js.')) out[key] = key in own ? own[key] : value;
  return out;
}

// The <script type="application/json" id="asb-i18n"> element html/template writes (with < escaped).
function dataBlock(lang = 'ko') {
  return `<script type="application/json" id="asb-i18n">${JSON.stringify(jsMessages(lang)).replace(/</g, '\\u003c')}</script>`;
}

// Puts the block and <html lang> into an already loaded page, before panel.js is added.
async function useCatalog(page, lang = 'ko') {
  await page.evaluate(([html, language]) => {
    document.documentElement.lang = language;
    document.body.insertAdjacentHTML('afterbegin', html);
  }, [dataBlock(lang), lang]);
}

// The expected text for a key, with the same single-pass substitution as ASB.t.
function msg(lang, key, vars = {}) {
  let text = jsMessages(lang)[key];
  if (text && typeof text === 'object') text = vars.n === 1 && text.one ? text.one : text.other;
  return String(text).replace(/\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (match, name) => (Object.hasOwn(vars, name) ? String(vars[name]) : match));
}

module.exports = { catalog, jsMessages, dataBlock, useCatalog, msg };
