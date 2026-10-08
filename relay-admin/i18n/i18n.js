// Dashboard translations, the same scheme as the website (revivetendo-relay-guide):
// the pages carry the English text, every translatable element has a data-i18n key,
// and lang/<code>.js registers that language's strings for those keys (missing keys
// stay English). Loaded in <head>: the language is picked before the page shows
// (?lang=, saved choice, browser languages), and for other languages the page stays
// hidden until the strings are applied, so English never flashes.
//
// Attributes: data-i18n (innerHTML; {name} placeholders filled from data-i18n-args,
// a JSON object), data-i18n-placeholder, data-i18n-title (title attribute);
// <body data-i18n-page-title="key"> translates document.title.
// Scripts translate their own text with I18N.t(key, englishFallback) and can run
// code after the strings arrive with I18N.ready(fn).
(function () {
  var LANGS = {
    en: 'English', ar: 'العربية', cs: 'Čeština', de: 'Deutsch', el: 'Ελληνικά', es: 'Español',
    fa: 'فارسی', fr: 'Français', hi: 'हिन्दी', hu: 'Magyar', id: 'Bahasa Indonesia', it: 'Italiano',
    ja: '日本語', ko: '한국어', nl: 'Nederlands', pl: 'Polski', pt: 'Português', ro: 'Română',
    ru: 'Русский', sv: 'Svenska', th: 'ไทย', tr: 'Türkçe', uk: 'Українська', vi: 'Tiếng Việt',
    zh: '中文（简体）'
  };
  var RTL = { ar: true, fa: true };
  var STORAGE_KEY = 'revivetendo-lang';
  var BASE = '/inkay/i18n/';

  var readyFns = [];
  var I18N = window.I18N = {
    langs: LANGS,
    strings: {},
    register: function (code, strings) {
      this.strings[code] = strings;
      if (code === this.lang && document.readyState !== 'loading') apply();
    },
    // t returns the current language's string for key, or fallback (English).
    t: function (key, fallback) {
      var d = this.strings[this.lang];
      return (d && d[key]) || fallback;
    },
    // ready runs fn once the strings are available (right away for English).
    ready: function (fn) {
      if (applied) fn(); else readyFns.push(fn);
    }
  };

  function stored() {
    try { return localStorage.getItem(STORAGE_KEY); } catch (e) { return null; }
  }

  function pick() {
    var q = /[?&]lang=([a-z]{2})/.exec(location.search);
    if (q && LANGS[q[1]]) return q[1];
    var s = stored();
    if (s && LANGS[s]) return s;
    var prefs = navigator.languages || [navigator.language || 'en'];
    for (var i = 0; i < prefs.length; i++) {
      var code = String(prefs[i]).toLowerCase().split('-')[0];
      if (LANGS[code]) return code;
    }
    return 'en';
  }

  I18N.lang = pick();
  var root = document.documentElement;
  root.lang = I18N.lang;
  if (RTL[I18N.lang]) root.dir = 'rtl';

  var style = document.createElement('style');
  style.textContent =
    '.i18n-loading body{visibility:hidden}' +
    '.lang-select{position:fixed;top:.6rem;right:.6rem;z-index:1000;padding:.25rem .4rem;font:inherit;font-size:.8rem;' +
    'border:1px solid #d4d4d8;border-radius:6px;background:#fff;color:#222;cursor:pointer}' +
    '[dir=rtl] .lang-select{right:auto;left:.6rem}' +
    '.lang-select.in-slot{position:static;margin-left:.5rem}' +
    '[dir=rtl] code,[dir=rtl] pre,[dir=rtl] .mono{direction:ltr;unicode-bidi:isolate}' +
    '@media (prefers-color-scheme:dark){.lang-select{background:#18181b;color:#e4e4e7;border-color:#3f3f46}}';
  document.head.appendChild(style);

  if (I18N.lang !== 'en') {
    // Hidden until the strings are applied; shown anyway after 3 s if the
    // language file fails to load.
    root.classList.add('i18n-loading');
    setTimeout(function () { root.classList.remove('i18n-loading'); }, 3000);
    var script = document.createElement('script');
    script.src = BASE + 'lang/' + I18N.lang + '.js';
    script.onerror = function () { root.classList.remove('i18n-loading'); };
    document.head.appendChild(script);
  }

  // fill replaces {name} placeholders with values from a data-i18n-args JSON object
  // (values are inserted as text, never as HTML).
  function fill(s, argsJSON) {
    if (!argsJSON) return s;
    var args;
    try { args = JSON.parse(argsJSON); } catch (e) { return s; }
    return s.replace(/\{(\w+)\}/g, function (m, k) {
      if (!(k in args)) return m;
      var d = document.createElement('div');
      d.textContent = String(args[k]);
      return d.innerHTML;
    });
  }
  I18N.fill = function (s, args) { return fill(s, JSON.stringify(args || {})); };

  function translateIn(container) {
    if (!I18N.strings[I18N.lang]) return;
    var els = container.querySelectorAll('[data-i18n]');
    for (var i = 0; i < els.length; i++) {
      var s = I18N.t(els[i].getAttribute('data-i18n'), null);
      if (s !== null) els[i].innerHTML = fill(s, els[i].getAttribute('data-i18n-args'));
    }
    els = container.querySelectorAll('[data-i18n-placeholder]');
    for (i = 0; i < els.length; i++) {
      s = I18N.t(els[i].getAttribute('data-i18n-placeholder'), null);
      if (s !== null) els[i].setAttribute('placeholder', s);
    }
    els = container.querySelectorAll('[data-i18n-title]');
    for (i = 0; i < els.length; i++) {
      s = I18N.t(els[i].getAttribute('data-i18n-title'), null);
      if (s !== null) els[i].setAttribute('title', s);
    }
  }
  // translate re-applies the strings to elements added later (e.g. by a script).
  I18N.translate = function (container) { translateIn(container || document); };

  // apply runs once the page and (for other languages) the strings are both there.
  var applied = false;
  function apply() {
    if (applied) return;
    if (I18N.lang !== 'en' && !I18N.strings[I18N.lang]) {
      addSwitcher(); // strings still loading; register() calls apply again
      return;
    }
    applied = true;
    translateIn(document);
    var titleKey = document.body.getAttribute('data-i18n-page-title');
    if (titleKey) document.title = I18N.t(titleKey, document.title);
    addSwitcher();
    root.classList.remove('i18n-loading');
    for (var i = 0; i < readyFns.length; i++) readyFns[i]();
    readyFns = [];
  }

  function addSwitcher() {
    if (document.body.hasAttribute('data-no-lang-select') || document.querySelector('.lang-select')) return;
    var select = document.createElement('select');
    select.className = 'lang-select';
    select.setAttribute('aria-label', 'Language');
    for (var code in LANGS) {
      var opt = document.createElement('option');
      opt.value = code;
      opt.textContent = LANGS[code];
      if (code === I18N.lang) opt.selected = true;
      select.appendChild(opt);
    }
    select.addEventListener('change', function () {
      try {
        localStorage.setItem(STORAGE_KEY, select.value);
        if (location.search) {
          location.href = location.pathname + location.hash; // drop a ?lang= so the choice applies
        } else {
          location.reload();
        }
      } catch (e) {
        location.href = location.pathname + '?lang=' + select.value + location.hash; // no storage: keep it in the URL
      }
    });
    // A page can mark where the menu goes (data-lang-slot); otherwise it floats
    // in the top corner.
    var slot = document.querySelector('[data-lang-slot]');
    if (slot) {
      select.className += ' in-slot';
      slot.appendChild(select);
    } else {
      document.body.appendChild(select);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', apply);
  } else {
    apply();
  }
})();
