'use strict';
/* LocalSyncExt: веб-интерфейс. Без внешних библиотек; весь текст выводится через textContent. */

const app = document.getElementById('app');
const S = { me: null, version: '', timers: [] };

// ---------- мелкие помощники ----------
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'selected') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}
function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }
function setTimer(fn, ms) { const id = setInterval(fn, ms); S.timers.push(id); return id; }
function clearTimers() { S.timers.forEach(clearInterval); S.timers = []; }

async function api(path, opt = {}) {
  const init = { method: opt.method || 'GET', headers: { 'X-LSX': '1' }, credentials: 'same-origin' };
  if (opt.body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(opt.body); }
  const r = await fetch(path, init);
  const ct = r.headers.get('content-type') || '';
  const data = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) {
    if (r.status === 401 && S.me && S.me.auth && path !== '/api/login') { location.hash = '#/login'; }
    const e = new Error((data && data.error) || (typeof data === 'string' && data) || r.statusText);
    e.status = r.status; e.data = data;
    throw e;
  }
  return data;
}

const STATUS_RU = { ok: 'ок', syncing: 'синхронизация', error: 'ошибка', paused: 'пауза', disabled: 'выключено', 'config-error': 'ошибка настроек', idle: 'ожидание' };
function badge(st) { return h('span', { class: 'badge ' + st }, STATUS_RU[st] || st); }

function fmtAgo(ms, now) {
  if (!ms) return '—';
  const d = new Date(ms);
  const sec = Math.round(((now || Date.now()) - ms) / 1000);
  if (sec < 5) return 'только что';
  if (sec < 60) return sec + ' с назад';
  if (sec < 3600) return Math.floor(sec / 60) + ' мин назад';
  const t = d.toLocaleTimeString('ru-RU');
  if (sec < 86400) return 'сегодня ' + t;
  return d.toLocaleDateString('ru-RU') + ' ' + t;
}
function fmtFull(ms) { return ms ? new Date(ms).toLocaleString('ru-RU') : ''; }
function fmtSize(n) {
  if (n < 1024) return n + ' Б';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' КБ';
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' МБ';
  return (n / 1073741824).toFixed(2) + ' ГБ';
}
function errText(e) {
  if (e && e.data && Array.isArray(e.data.errors)) return e.data.error + ':\n• ' + e.data.errors.join('\n• ');
  return (e && e.message) || String(e);
}

// ---------- оболочка страницы ----------
function shell(active, ...content) {
  const link = (href, text, key) => h('a', { href, class: active === key ? 'active' : '' }, text);
  const nav = h('nav', {}, link('#/', 'Правила', 'rules'), link('#/status', 'Статус и журнал', 'status'), link('#/settings', 'Настройки', 'settings'), link('#/help', 'Справка', 'help'));
  const top = h('header', { class: 'top' }, h('span', { class: 'brand' }, 'LocalSyncExt'), nav);
  if (S.me && S.me.auth) {
    top.append(h('span', { class: 'who' }, S.me.user), h('button', { class: 'small', onclick: logout }, 'Выйти'));
  }
  clear(app);
  app.append(top, h('main', {}, ...content), h('footer', {}, 'LocalSyncExt ' + (S.version || (S.me && S.me.version) || '')));
}
async function logout() {
  try { await api('/api/logout', { method: 'POST', body: {} }); } catch (e) { /* ignore */ }
  location.hash = '#/login';
}
function errorPage(e) {
  shell('', h('div', { class: 'errbox' }, errText(e)), h('button', { onclick: () => route() }, 'Повторить'));
}

// ---------- вход ----------
function pageLogin(needSetup) {
  clear(app);
  const err = h('div');
  const u = h('input', { type: 'text', placeholder: 'Логин', autocomplete: 'username' });
  const p = h('input', { type: 'password', placeholder: 'Пароль', autocomplete: 'current-password' });
  async function go() {
    clear(err);
    try {
      await api('/api/login', { method: 'POST', body: { user: u.value, password: p.value } });
      S.me = await api('/api/me');
      location.hash = '#/';
      route();
    } catch (e) { err.append(h('div', { class: 'errbox' }, e.message)); }
  }
  p.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') go(); });
  u.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') p.focus(); });
  app.append(h('div', { class: 'login panel' },
    h('h2', {}, 'LocalSyncExt'),
    needSetup ? h('div', { class: 'warnbox' }, 'Логин и пароль ещё не заданы. Задайте их на странице настроек через порт без авторизации или командой LocalSyncExt-cli.exe -web-user ИМЯ -web-pass ПАРОЛЬ.') : null,
    h('div', { class: 'field', style: 'grid-template-columns:1fr' }, u), h('div', { class: 'field', style: 'grid-template-columns:1fr' }, p),
    err, h('button', { class: 'primary', onclick: go }, 'Войти'),
    h('footer', {}, 'LocalSyncExt ' + ((S.me && S.me.version) || ''))));
  u.focus();
}

// ---------- список правил ----------
async function pageRules() {
  const list = h('div', { class: 'cards' });
  const info = h('div');
  shell('rules',
    h('h2', {}, 'Правила синхронизации'),
    h('div', { class: 'toolbar' },
      h('button', { class: 'primary', onclick: () => { location.hash = '#/rule-new'; } }, '＋ Добавить правило'),
      h('span', { class: 'grow' }),
      h('button', { onclick: () => act('/api/all', { action: 'sync' }) }, 'Синхронизировать всё'),
      h('button', { onclick: () => act('/api/all', { action: 'pause' }) }, 'Пауза'),
      h('button', { onclick: () => act('/api/all', { action: 'resume' }) }, 'Продолжить')),
    info, list);
  async function act(path, body) {
    try { await api(path, { method: 'POST', body }); setTimeout(refresh, 400); } catch (e) { info.replaceChildren(h('div', { class: 'errbox' }, errText(e))); }
  }
  async function refresh() {
    let st;
    try { st = await api('/api/state'); } catch (e) { info.replaceChildren(h('div', { class: 'errbox' }, errText(e))); return; }
    S.version = st.version;
    info.replaceChildren();
    clear(list);
    if (!st.rules.length) list.append(h('div', { class: 'panel muted' }, 'Правил пока нет. Нажмите «Добавить правило».'));
    for (const r of st.rules) {
      const stop = (fn) => (ev) => { ev.stopPropagation(); fn(); };
      const usable = r.status !== 'config-error' && r.status !== 'disabled';
      const meta = r.status === 'config-error' || r.status === 'disabled' ? STATUS_RU[r.status]
        : 'проход: ' + fmtAgo(r.lastRun, st.now) + ' · изменения: ' + fmtAgo(r.lastChange, st.now);
      list.append(h('div', { class: 'card', onclick: () => { location.hash = '#/rule/' + encodeURIComponent(r.name); } },
        h('div', { class: 'head' }, h('span', { class: 'name' }, r.name), badge(r.status),
          h('span', { class: 'muted', style: 'font-size:12px' }, r.mode === 'oneway' ? 'односторонний' : 'двусторонний')),
        h('div', { class: 'meta', title: fmtFull(r.lastRun) }, meta),
        r.err ? h('div', { class: 'err' }, r.err) : null,
        h('div', { class: 'actions' },
          h('button', { class: 'small', disabled: !usable || r.paused, onclick: stop(() => act('/api/rule', { name: r.name, action: 'sync' })) }, 'Синхронизировать'),
          h('button', { class: 'small', disabled: !usable, onclick: stop(() => act('/api/rule', { name: r.name, action: r.paused ? 'resume' : 'pause' })) }, r.paused ? 'Продолжить' : 'Пауза')),
        r.desc ? h('div', { class: 'desc' }, r.desc) : null));
    }
  }
  await refresh();
  setTimer(refresh, 3000);
}

// ---------- обзор файлов ----------
function fileBrowser({ title, start, onDone }) {
  let cur = { path: '', sep: '\\' };
  const checked = new Set();
  const pathInput = h('input', { type: 'text', placeholder: 'Путь, например C:\\ или D:\\Docs' });
  const body = h('div', { class: 'mb' });
  const err = h('div');
  const maskInput = h('input', { type: 'text', placeholder: 'маска, например *.pdf', style: 'width:180px' });
  const countBtn = h('button', { class: 'primary' }, 'Выбрать отмеченное');
  const join = (n) => (cur.path === '' ? n : cur.path.endsWith(cur.sep) ? cur.path + n : cur.path + cur.sep + n);
  function updateCount() { countBtn.textContent = 'Выбрать отмеченное (' + checked.size + ')'; countBtn.disabled = checked.size === 0; }
  async function load(p) {
    clear(err);
    try {
      const d = await api('/api/fs?path=' + encodeURIComponent(p));
      cur = d; if (!cur.sep) cur.sep = '\\';
      pathInput.value = d.path;
      checked.clear(); updateCount();
      clear(body);
      if (d.path !== '') body.append(h('div', { class: 'frow' }, h('span', { class: 'fname', onclick: () => load(d.parent || '') }, '⬆ ..')));
      if (d.truncated) body.append(h('div', { class: 'warnbox' }, 'Показаны не все элементы (слишком много). Впишите путь вручную.'));
      for (const e of d.entries) {
        const full = d.path === '' ? e.name : join(e.name);
        const cb = h('input', { type: 'checkbox', onchange: (ev) => { if (ev.target.checked) checked.add(full); else checked.delete(full); updateCount(); } });
        body.append(h('div', { class: 'frow' }, d.path === '' ? null : cb,
          h('span', { class: 'fname', onclick: () => { if (e.dir) load(full); else { cb.checked = !cb.checked; cb.dispatchEvent(new Event('change')); } } }, (e.dir ? '📁 ' : '📄 ') + e.name),
          e.dir ? null : h('span', { class: 'fsize' }, fmtSize(e.size))));
      }
    } catch (e) { err.append(h('div', { class: 'errbox' }, e.message)); }
  }
  const bg = h('div', { class: 'modal-bg' });
  const close = () => bg.remove();
  countBtn.addEventListener('click', () => { const v = [...checked]; close(); onDone(v); });
  pathInput.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') load(pathInput.value); });
  const modal = h('div', { class: 'modal' },
    h('div', { class: 'mh' }, h('b', {}, title || 'Выбор файлов и папок'),
      h('div', { class: 'row' }, pathInput, h('button', { onclick: () => load(pathInput.value) }, 'Перейти'), h('button', { onclick: () => load('') }, 'Диски')),
      err),
    body,
    h('div', { class: 'mf' }, countBtn,
      h('button', { onclick: () => { if (cur.path) { const p = cur.path; close(); onDone([p]); } } }, 'Выбрать эту папку'),
      h('span', { class: 'row' }, maskInput, h('button', { onclick: () => { const m = maskInput.value.trim(); if (m && cur.path) { const p = join(m); close(); onDone([p]); } } }, 'Папка + маска')),
      h('span', { class: 'grow', style: 'flex:1' }), h('button', { onclick: close }, 'Отмена')));
  bg.append(modal);
  bg.addEventListener('mousedown', (ev) => { if (ev.target === bg) close(); });
  document.body.append(bg);
  updateCount();
  // начинаем с папки введённого пути (если это файл — его родитель)
  (async () => {
    let p = start || '';
    if (p) {
      try { await api('/api/fs?path=' + encodeURIComponent(p)); } catch (e) {
        const i = Math.max(p.lastIndexOf('\\'), p.lastIndexOf('/'));
        p = i > 2 ? p.slice(0, i) : (i >= 0 ? p.slice(0, i + 1) : '');
      }
    }
    load(p);
  })();
}

// ---------- редактор правила ----------
const OPT_SPECS = [
  { k: 'enabled', label: 'Правило включено', t: 'bool', def: 'Да' },
  { k: 'mode', label: 'Режим', t: 'sel', opts: [['twoway', 'Двусторонний (изменения идут во все стороны)'], ['oneway', 'Односторонний (остальные пути — копия главного)']], def: 'двусторонний' },
  { k: 'master', label: 'Главный путь', t: 'master' },
  { k: 'conflict', label: 'Конфликты', t: 'sel', opts: [['auto', 'Решать автоматически: побеждает новый, копия сохраняется'], ['ask', 'Не трогать файлы, только сообщить']], def: 'автоматически' },
  { k: 'trashdays', label: 'Корзина (удалённое и заменённое)', t: 'trash' },
  { k: 'recursive', label: 'Вложенные папки', t: 'bool', def: 'Да' },
  { k: 'watch', label: 'Мгновенное слежение', t: 'bool', def: 'Да', hint: 'Изменения подхватываются сразу, а не по таймеру.' },
  { k: 'scan', label: 'Полное сканирование, секунд', t: 'num', ph: '900 при слежении, 60 без него', hint: '0 — выключить. Страховка на случай пропущенных событий.' },
  { k: 'notify', label: 'Уведомления Windows', t: 'bool', def: 'Да' },
  { k: 'ignore', label: 'Игнорировать (маски через ;)', t: 'text', adv: true, ph: '*.bak;~$*' },
  { k: 'maxdeletepercent', label: 'Защита от массового удаления, %', t: 'num', adv: true, ph: '50' },
  { k: 'deleteextra', label: 'Удалять лишнее (для одностороннего)', t: 'bool', adv: true, def: 'Да' },
  { k: 'marker', label: 'Маркер в папках', t: 'bool', adv: true, def: 'Да', hint: 'Отметки «без маркера» ставятся у конкретных путей выше.' },
  { k: 'mtimetol', label: 'Допуск времени изменения, секунд', t: 'num', adv: true, ph: '2', hint: 'Файлы с разницей во времени не больше этого считаются одинаковыми.' },
  { k: 'timeout', label: 'Ждать ответа удалённого диска, секунд', t: 'num', adv: true, ph: '60 для сетевого, 180 для облака', hint: 'Если диск не отвечает дольше, правило приостанавливается, ничего не удаляя.' },
  { k: 'debounce', label: 'Пауза накопления событий, мс', t: 'num', adv: true, ph: '1500' },
  { k: 'trash', label: 'Где хранить корзину', t: 'text', adv: true, ph: 'folder | work | полный путь' },
  { k: 'temp', label: 'Временные файлы больших файлов', t: 'sel', adv: true, opts: [['prefix', 'Рядом, с приставкой LocalSyncExt-…'], ['work', 'В рабочей папке (только на том же диске)']], def: 'рядом' },
  { k: 'directbelow', label: 'Писать сразу без временного файла до, КБ', t: 'num', adv: true, ph: '1024' },
];

function optControl(spec, R, rerender) {
  const o = R.opts;
  const row = (ctrl) => h('div', { class: 'field' }, h('label', {}, spec.label), ctrl, spec.hint ? h('div', { class: 'hint' }, spec.hint) : null);
  if (spec.t === 'bool') {
    const sel = h('select', { onchange: (e) => { if (e.target.value === '') delete o[spec.k]; else o[spec.k] = e.target.value; } },
      h('option', { value: '', selected: !(spec.k in o) }, 'По умолчанию (' + spec.def + ')'),
      h('option', { value: '1', selected: o[spec.k] === '1' }, 'Да'), h('option', { value: '0', selected: o[spec.k] === '0' }, 'Нет'));
    return row(sel);
  }
  if (spec.t === 'sel') {
    const sel = h('select', { onchange: (e) => { if (e.target.value === '') delete o[spec.k]; else o[spec.k] = e.target.value; if (spec.k === 'mode') rerender(); } },
      h('option', { value: '', selected: !(spec.k in o) }, 'По умолчанию (' + spec.def + ')'),
      ...spec.opts.map(([v, l]) => h('option', { value: v, selected: o[spec.k] === v }, l)));
    return row(sel);
  }
  if (spec.t === 'master') {
    if (o.mode !== 'oneway') return null;
    const sel = h('select', { onchange: (e) => { if (e.target.value === '') delete o.master; else o.master = e.target.value; } },
      h('option', { value: '', selected: !('master' in o) }, 'Первый путь (по умолчанию)'),
      ...R.paths.map((p, i) => h('option', { value: String(i + 1), selected: o.master === String(i + 1) }, 'Путь №' + (i + 1) + (p.p ? ': ' + p.p : ''))));
    return row(sel);
  }
  if (spec.t === 'trash') {
    const mode = !('trashdays' in o) ? 'default' : o.trashdays === '' ? 'none' : o.trashdays === '0' ? 'forever' : 'days';
    const days = h('input', { type: 'number', min: '1', value: mode === 'days' ? o.trashdays : '30', style: 'width:90px', onchange: (e) => { if (sel.value === 'days') o.trashdays = String(Math.max(1, parseInt(e.target.value || '30', 10))); } });
    days.style.display = mode === 'days' ? '' : 'none';
    const sel = h('select', { onchange: (e) => {
      const v = e.target.value;
      days.style.display = v === 'days' ? '' : 'none';
      if (v === 'default') delete o.trashdays; else if (v === 'none') o.trashdays = ''; else if (v === 'forever') o.trashdays = '0'; else o.trashdays = String(Math.max(1, parseInt(days.value || '30', 10)));
    } },
      h('option', { value: 'default', selected: mode === 'default' }, 'По умолчанию (30 дней)'),
      h('option', { value: 'none', selected: mode === 'none' }, 'Без корзины (удалять сразу)'),
      h('option', { value: 'forever', selected: mode === 'forever' }, 'Хранить бессрочно'),
      h('option', { value: 'days', selected: mode === 'days' }, 'Хранить N дней'));
    return row(h('div', { class: 'row' }, sel, days));
  }
  // text / num
  const inp = h('input', { type: spec.t === 'num' ? 'number' : 'text', placeholder: spec.ph || '', value: o[spec.k] || '',
    oninput: (e) => { const v = e.target.value.trim(); if (v === '') delete o[spec.k]; else o[spec.k] = v; } });
  if (spec.t === 'num') inp.style.width = '160px';
  return row(inp);
}

async function pageRule(name) {
  const cfg = await api('/api/config');
  S.version = S.version || '';
  const isNew = name === null;
  let R;
  if (isNew) R = { name: '', desc: '', opts: {}, paths: [{ p: '', nm: false, kind: 'local' }, { p: '', nm: false, kind: 'local' }] };
  else {
    const src = cfg.rules.find((r) => r.name === name);
    if (!src) { errorPage(new Error('Правило «' + name + '» не найдено в конфиге')); return; }
    const opts = {}; for (const kv of src.opts) opts[kv.k] = kv.v;
    const nm = new Set((opts.nomarker || '').split(/[,; ]+/).filter(Boolean).map((x) => parseInt(x, 10) - 1));
    delete opts.nomarker;
    const idxSet = (v) => new Set((v || '').split(/[,; ]+/).filter(Boolean).map((x) => parseInt(x, 10) - 1));
    const rem = idxSet(opts.remote), cld = idxSet(opts.cloud);
    delete opts.remote; delete opts.cloud;
    R = { name: src.name, desc: src.desc, opts, paths: src.paths.map((p, i) => ({ p, nm: nm.has(i), kind: cld.has(i) ? 'cloud' : rem.has(i) ? 'remote' : 'local' })) };
    if (src.err) R.configErr = src.err;
  }
  const errBox = h('div');
  const pathsBox = h('div');
  const optsBox = h('div');
  const advBox = h('div');

  function renderPaths() {
    clear(pathsBox);
    R.paths.forEach((pr, i) => {
      const inp = h('input', { type: 'text', value: pr.p, placeholder: 'C:\\папка, C:\\файл.txt или C:\\папка\\*.pdf', oninput: (e) => { pr.p = e.target.value; } });
      pathsBox.append(h('div', { class: 'pathrow' },
        h('span', { class: 'num' }, '№' + (i + 1)), inp,
        h('select', { title: 'Тип диска: от него зависят слежение, таймауты и сравнение времени изменения', onchange: (e) => { pr.kind = e.target.value; } },
          h('option', { value: 'local', selected: pr.kind === 'local' }, 'Локальный'),
          h('option', { value: 'remote', selected: pr.kind === 'remote' }, 'Сетевой (sshfs, SMB)'),
          h('option', { value: 'cloud', selected: pr.kind === 'cloud' }, 'Облако (rclone)')),
        h('button', { class: 'small', onclick: () => fileBrowser({ title: 'Путь №' + (i + 1), start: pr.p, onDone: (v) => {
          if (!v.length) return;
          pr.p = v[0];
          R.paths.splice(i + 1, 0, ...v.slice(1).map((p) => ({ p, nm: false, kind: pr.kind })));
          renderPaths(); renderOpts();
        } }) }, 'Обзор…'),
        h('button', { class: 'small', disabled: i === 0, onclick: () => { [R.paths[i - 1], R.paths[i]] = [R.paths[i], R.paths[i - 1]]; renderPaths(); renderOpts(); } }, '↑'),
        h('button', { class: 'small', disabled: i === R.paths.length - 1, onclick: () => { [R.paths[i + 1], R.paths[i]] = [R.paths[i], R.paths[i + 1]]; renderPaths(); renderOpts(); } }, '↓'),
        h('button', { class: 'small danger', onclick: () => { R.paths.splice(i, 1); renderPaths(); renderOpts(); } }, '✕'),
        h('label', { title: 'Не создавать служебный файл-маркер в этой папке' }, h('input', { type: 'checkbox', checked: pr.nm, onchange: (e) => { pr.nm = e.target.checked; } }), 'без маркера')));
    });
  }
  function renderOpts() {
    clear(optsBox); clear(advBox);
    for (const sp of OPT_SPECS) {
      const c = optControl(sp, R, renderOpts);
      if (!c) continue;
      (sp.adv ? advBox : optsBox).append(c);
    }
  }
  renderPaths(); renderOpts();

  const nameInp = h('input', { type: 'text', value: R.name, placeholder: 'Название правила', oninput: (e) => { R.name = e.target.value; } });
  const descInp = h('input', { type: 'text', value: R.desc, placeholder: 'Краткая аннотация: что и зачем синхронизируется', oninput: (e) => { R.desc = e.target.value; } });

  function buildRule() {
    const opts = [];
    for (const [k, v] of Object.entries(R.opts)) opts.push({ k, v });
    // «без маркера» нумеруется по непустым путям — так же, как строки путей в ini
    const nonEmpty = R.paths.filter((pr) => pr.p.trim());
    const nm2 = nonEmpty.map((pr, i) => (pr.nm ? i + 1 : 0)).filter(Boolean);
    if (nm2.length) opts.push({ k: 'nomarker', v: nm2.join(',') });
    const idx = (kind) => nonEmpty.map((pr, i) => (pr.kind === kind ? i + 1 : 0)).filter(Boolean).join(',');
    if (idx('remote')) opts.push({ k: 'remote', v: idx('remote') });
    if (idx('cloud')) opts.push({ k: 'cloud', v: idx('cloud') });
    return { name: R.name.trim(), desc: R.desc.trim(), opts, paths: nonEmpty.map((pr) => pr.p.trim()) };
  }
  async function save() {
    clear(errBox);
    const rule = buildRule();
    if (!rule.name) { errBox.append(h('div', { class: 'errbox' }, 'Укажите название правила')); return; }
    if (rule.paths.length < 2) { errBox.append(h('div', { class: 'errbox' }, 'Нужно минимум два пути')); return; }
    try {
      const latest = await api('/api/config');
      const idx = isNew ? -1 : latest.rules.findIndex((r) => r.name === name);
      if (!isNew && idx < 0) throw new Error('Правило уже изменено или удалено в другом месте. Обновите страницу.');
      const clash = latest.rules.findIndex((r, i) => r.name.toLowerCase() === rule.name.toLowerCase() && i !== idx);
      if (clash >= 0) throw new Error('Правило с таким названием уже есть');
      if (idx >= 0) latest.rules[idx] = rule; else latest.rules.push(rule);
      const res = await api('/api/config', { method: 'POST', body: { rev: latest.rev, settings: latest.settings, rules: latest.rules } });
      if (!isNew && rule.name !== name) alert('Правило переименовано. Для программы это новое правило: прежняя история синхронизации не переносится, лишнего удалено не будет.');
      if (res.warnings && res.warnings.length) alert('Сохранено. Замечания:\n' + res.warnings.join('\n'));
      location.hash = '#/';
    } catch (e) { errBox.replaceChildren(h('div', { class: 'errbox' }, errText(e))); }
  }
  async function del() {
    if (!confirm('Удалить правило «' + name + '»? Файлы в папках не удаляются, правило просто перестанет работать.')) return;
    try {
      const latest = await api('/api/config');
      latest.rules = latest.rules.filter((r) => r.name !== name);
      await api('/api/config', { method: 'POST', body: { rev: latest.rev, settings: latest.settings, rules: latest.rules } });
      location.hash = '#/';
    } catch (e) { errBox.replaceChildren(h('div', { class: 'errbox' }, errText(e))); }
  }

  shell('rules',
    h('h2', {}, isNew ? 'Новое правило' : 'Правило: ' + name),
    R.configErr ? h('div', { class: 'errbox' }, 'Ошибка в настройках правила: ' + R.configErr) : null,
    h('div', { class: 'panel' },
      h('div', { class: 'field' }, h('label', {}, 'Название'), nameInp),
      h('div', { class: 'field' }, h('label', {}, 'Аннотация'), descInp, h('div', { class: 'hint' }, 'Показывается мелким шрифтом под названием в списке правил.'))),
    h('div', { class: 'panel' },
      h('h3', { style: 'margin-top:0' }, 'Пути'),
      h('div', { class: 'muted', style: 'font-size:13px;margin-bottom:6px' }, 'Папка, отдельный файл или маска (D:\\Docs\\*.pdf). Содержимое всех путей правила приводится к одному виду. Пути вводятся вручную, вставляются или выбираются кнопкой «Обзор…» — путь на компьютере, где работает программа.'),
      pathsBox,
      h('div', { class: 'row', style: 'margin-top:8px' },
        h('button', { onclick: () => { R.paths.push({ p: '', nm: false, kind: 'local' }); renderPaths(); renderOpts(); } }, '＋ Путь'),
        h('button', { onclick: () => fileBrowser({ title: 'Добавить файлы и папки', start: '', onDone: (v) => {
          for (const p of v) { const empty = R.paths.find((x) => !x.p.trim()); if (empty) empty.p = p; else R.paths.push({ p, nm: false, kind: 'local' }); }
          renderPaths(); renderOpts();
        } }) }, '＋ Обзор (можно несколько)…'))),
    h('div', { class: 'panel' }, h('h3', { style: 'margin-top:0' }, 'Параметры'), optsBox,
      h('details', {}, h('summary', {}, 'Дополнительно'), advBox)),
    errBox,
    h('div', { class: 'toolbar' },
      h('button', { class: 'primary', onclick: save }, 'Сохранить'),
      h('button', { onclick: () => { location.hash = '#/'; } }, 'Отмена'),
      h('span', { class: 'grow' }),
      isNew ? null : h('button', { class: 'danger', onclick: del }, 'Удалить правило')));
}

// ---------- статус и журнал ----------
async function pageStatus() {
  const tbl = h('div', { class: 'tablewrap' });
  const logBox = h('pre', { class: 'log' });
  const info = h('div');
  const levelSel = h('select', {}, h('option', { value: '' }, 'все события'), h('option', { value: 'WARN' }, 'предупреждения и ошибки'), h('option', { value: 'ERROR' }, 'только ошибки'));
  const ruleSel = h('select', {}, h('option', { value: '' }, 'все правила'));
  const srcSel = h('select', {}, h('option', { value: 'events' }, 'события (с запуска)'), h('option', { value: 'file' }, 'файл журнала (хвост)'));
  const auto = h('input', { type: 'checkbox', checked: true });
  shell('status',
    h('h2', {}, 'Статус и журнал'),
    h('div', { class: 'toolbar' },
      h('button', { onclick: () => post('/api/all', { action: 'sync' }) }, 'Синхронизировать всё'),
      h('button', { onclick: () => post('/api/all', { action: 'pause' }) }, 'Пауза всех'),
      h('button', { onclick: () => post('/api/all', { action: 'resume' }) }, 'Продолжить все')),
    info, tbl,
    h('h3', {}, 'Журнал'),
    h('div', { class: 'toolbar' }, srcSel, levelSel, ruleSel, h('label', { class: 'row' }, auto, 'обновлять сам')),
    logBox);
  async function post(path, body) {
    try { await api(path, { method: 'POST', body }); setTimeout(refresh, 400); } catch (e) { info.replaceChildren(h('div', { class: 'errbox' }, errText(e))); }
  }
  let known = '';
  async function refresh() {
    try {
      const st = await api('/api/state');
      S.version = st.version;
      info.replaceChildren();
      const names = st.rules.map((r) => r.name).join('\n');
      if (names !== known) { known = names; const v = ruleSel.value; clear(ruleSel); ruleSel.append(h('option', { value: '' }, 'все правила'), ...st.rules.map((r) => h('option', { value: r.name }, r.name))); ruleSel.value = v; }
      const rows = st.rules.map((r) => h('tr', {},
        h('td', {}, h('a', { href: '#/rule/' + encodeURIComponent(r.name) }, r.name)), h('td', {}, badge(r.status)),
        h('td', { title: fmtFull(r.lastRun) }, fmtAgo(r.lastRun, st.now)), h('td', { title: fmtFull(r.lastChange) }, fmtAgo(r.lastChange, st.now)),
        h('td', {}, r.copied), h('td', {}, r.deleted), h('td', {}, r.conflicts), h('td', {}, r.failed),
        h('td', { style: 'color:var(--err);word-break:break-word' }, r.err)));
      tbl.replaceChildren(h('table', {}, h('thead', {}, h('tr', {}, ...['Правило', 'Состояние', 'Последний проход', 'Последнее изменение', 'Скопировано', 'Удалено', 'Конфликтов', 'Ошибок', 'Последняя ошибка'].map((t) => h('th', {}, t)))), h('tbody', {}, ...rows)));
      await refreshLog();
    } catch (e) { info.replaceChildren(h('div', { class: 'errbox' }, errText(e))); }
  }
  async function refreshLog() {
    const atBottom = logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight < 40;
    if (srcSel.value === 'file') {
      try { logBox.textContent = await api('/api/logfile?kb=128'); } catch (e) { logBox.textContent = e.message; }
    } else {
      const q = new URLSearchParams({ limit: '400' });
      if (levelSel.value) q.set('level', levelSel.value);
      if (ruleSel.value) q.set('rule', ruleSel.value);
      const ev = await api('/api/log?' + q);
      clear(logBox);
      for (const e of ev) {
        logBox.append(h('span', { class: 't' }, new Date(e.t).toLocaleTimeString('ru-RU') + ' '), h('span', { class: e.level }, '[' + e.rule + '] ' + e.level + ': ' + e.msg + '\n'));
      }
      if (!ev.length) logBox.textContent = 'Событий нет';
    }
    if (atBottom) logBox.scrollTop = logBox.scrollHeight;
  }
  [srcSel, levelSel, ruleSel].forEach((s) => s.addEventListener('change', () => { refreshLog().then(() => { logBox.scrollTop = logBox.scrollHeight; }); }));
  await refresh();
  logBox.scrollTop = logBox.scrollHeight;
  setTimer(() => { if (auto.checked) refresh(); }, 3000);
}

// ---------- настройки ----------
const ACTIONS = [['status', 'Показать сводку'], ['syncnow', 'Синхронизировать всё'], ['pause', 'Пауза / продолжить'], ['ui', 'Открыть веб-интерфейс'], ['log', 'Открыть журнал'], ['config', 'Открыть ini'], ['notify', 'Уведомления вкл/выкл'], ['none', 'Ничего']];
const SETTING_GROUPS = [
  { g: 'Общие', items: [
    { k: 'notifications', label: 'Уведомления Windows', t: 'bool', def: 'Да' },
    { k: 'trayclick', label: 'Левый щелчок по значку', t: 'sel', opts: ACTIONS, def: 'сводка' },
    { k: 'traydoubleclick', label: 'Двойной щелчок по значку', t: 'sel', opts: ACTIONS, def: 'открыть веб-интерфейс' },
    { k: 'trayblinkms', label: 'Мигание значка при синхронизации, мс', t: 'num', ph: '200', hint: 'Период смены значков «зелёный/жёлтый» во время синхронизации (50–5000).' },
    { k: 'workdir', label: 'Рабочая папка', t: 'text', ph: 'work рядом с программой', hint: 'Корзина и временные файлы, если вынесены из папок. Не должна лежать внутри синхронизируемых путей.' },
    { k: 'logfile', label: 'Файл журнала', t: 'text', ph: 'LocalSyncExt.log рядом с программой', hint: 'Применяется после перезапуска программы.' },
    { k: 'statedir', label: 'Папка состояния', t: 'text', ph: 'state рядом с программой', hint: 'Применяется после перезапуска программы.' }] },
  { g: 'Умолчания для правил', items: [
    { k: 'trash', label: 'Корзина', t: 'text', ph: 'folder | work | полный путь' },
    { k: 'temp', label: 'Временные файлы', t: 'sel', opts: [['prefix', 'Рядом, с приставкой LocalSyncExt-…'], ['work', 'В рабочей папке (только на том же диске)']], def: 'рядом' },
    { k: 'directbelow', label: 'Писать без временного файла до, КБ', t: 'num', ph: '1024' }] },
  { g: 'Веб-интерфейс', items: [
    { k: 'webui', label: 'Веб-интерфейс включён', t: 'bool', def: 'Да' },
    { k: 'webportopen', label: 'Порт БЕЗ авторизации', t: 'num', ph: '20001', hint: '0 — выключить. Для доверенных сетей (ZeroTier, локальная сеть). Закройте внешний доступ брандмауэром.' },
    { k: 'webopenbind', label: 'Адрес для порта без авторизации', t: 'text', ph: '127.0.0.1', hint: '127.0.0.1 — только этот компьютер; 0.0.0.0 — все сетевые интерфейсы.' },
    { k: 'webportauth', label: 'Порт с логином и паролем', t: 'num', ph: '0 (выключен)', hint: 'Вход по логину/паролю, затем cookie на 30 дней.' },
    { k: 'webauthbind', label: 'Адрес для порта с авторизацией', t: 'text', ph: '0.0.0.0' },
    { k: 'webhosts', label: 'Разрешённые имена узлов', t: 'text', ph: 'через запятую', hint: 'По IP-адресу и localhost доступ разрешён всегда; имена (например mypc.zt) добавьте сюда.' }] },
];

async function pageSettings() {
  const cfg = await api('/api/config');
  const vals = {}; for (const kv of cfg.settings) vals[kv.k] = kv.v;
  const errBox = h('div');
  const groups = SETTING_GROUPS.map((grp) => h('div', { class: 'panel' }, h('h3', { style: 'margin-top:0' }, grp.g),
    ...grp.items.map((it) => {
      let ctrl;
      if (it.t === 'bool') ctrl = h('select', { onchange: (e) => { if (e.target.value === '') delete vals[it.k]; else vals[it.k] = e.target.value; } },
        h('option', { value: '', selected: !(it.k in vals) }, 'По умолчанию (' + it.def + ')'), h('option', { value: '1', selected: vals[it.k] === '1' }, 'Да'), h('option', { value: '0', selected: vals[it.k] === '0' }, 'Нет'));
      else if (it.t === 'sel') ctrl = h('select', { onchange: (e) => { if (e.target.value === '') delete vals[it.k]; else vals[it.k] = e.target.value; } },
        h('option', { value: '', selected: !(it.k in vals) }, 'По умолчанию (' + it.def + ')'), ...it.opts.map(([v, l]) => h('option', { value: v, selected: vals[it.k] === v }, l)));
      else ctrl = h('input', { type: it.t === 'num' ? 'number' : 'text', placeholder: it.ph || '', value: vals[it.k] || '', style: it.t === 'num' ? 'width:160px' : null,
        oninput: (e) => { const v = e.target.value.trim(); if (v === '') delete vals[it.k]; else vals[it.k] = v; } });
      return h('div', { class: 'field' }, h('label', {}, it.label), ctrl, it.hint ? h('div', { class: 'hint' }, it.hint) : null);
    })));
  async function save() {
    clear(errBox);
    try {
      const latest = await api('/api/config');
      const known = new Set(SETTING_GROUPS.flatMap((g) => g.items.map((i) => i.k)));
      const settings = latest.settings.filter((kv) => !known.has(kv.k) || (kv.k in vals));
      for (const kv of settings) if (known.has(kv.k)) kv.v = vals[kv.k];
      for (const k of Object.keys(vals)) if (!settings.find((kv) => kv.k === k)) settings.push({ k, v: vals[k] });
      const res = await api('/api/config', { method: 'POST', body: { rev: latest.rev, settings, rules: latest.rules } });
      errBox.append(h('div', { class: 'okbox' }, 'Сохранено и применено.' + (res.warnings && res.warnings.length ? ' Замечания: ' + res.warnings.join('; ') : '')));
    } catch (e) { errBox.append(h('div', { class: 'errbox' }, errText(e))); }
  }
  const cuser = h('input', { type: 'text', placeholder: 'Логин', autocomplete: 'off' });
  const cp1 = h('input', { type: 'password', placeholder: 'Новый пароль (не короче 6 символов)', autocomplete: 'new-password' });
  const cp2 = h('input', { type: 'password', placeholder: 'Повторите пароль', autocomplete: 'new-password' });
  const credBox = h('div');
  async function saveCred() {
    clear(credBox);
    if (cp1.value !== cp2.value) { credBox.append(h('div', { class: 'errbox' }, 'Пароли не совпадают')); return; }
    try { await api('/api/credentials', { method: 'POST', body: { user: cuser.value, password: cp1.value } }); cp1.value = cp2.value = ''; credBox.append(h('div', { class: 'okbox' }, 'Логин и пароль сохранены. Прежние входы на порту с авторизацией перестали действовать.')); }
    catch (e) { credBox.append(h('div', { class: 'errbox' }, errText(e))); }
  }
  shell('settings', h('h2', {}, 'Настройки'),
    h('div', { class: 'warnbox' }, 'Изменение настроек веб-интерфейса (порты, адреса) может сделать эту страницу недоступной по текущему адресу.'),
    ...groups, errBox,
    h('div', { class: 'toolbar' }, h('button', { class: 'primary', onclick: save }, 'Сохранить настройки')),
    h('div', { class: 'panel' }, h('h3', { style: 'margin-top:0' }, 'Логин и пароль для порта с авторизацией'),
      h('div', { class: 'muted', style: 'font-size:13px' }, 'Хранятся в виде хэша в папке состояния, не в ini.'),
      h('div', { class: 'field' }, h('label', {}, 'Логин'), cuser), h('div', { class: 'field' }, h('label', {}, 'Пароль'), cp1), h('div', { class: 'field' }, h('label', {}, 'Ещё раз'), cp2),
      credBox, h('button', { onclick: saveCred }, 'Сохранить логин и пароль')));
}

// ---------- справка ----------
const HELP = [
  { t: 'Как это работает', p: [
    'Правило объединяет два и больше путей: папки, отдельные файлы или маски (D:\\Docs\\*.pdf). В двустороннем режиме изменение в любом пути доходит до остальных, в одностороннем остальные пути становятся копией главного.',
    'Удалённые и заменённые файлы попадают в корзину (по умолчанию .LocalSyncTrash в папке, хранится 30 дней). Конфликты (файл изменён сразу в нескольких местах) решаются автоматически: побеждает более новая версия, проигравшая сохраняется рядом как имя.sync-conflict-ДАТА-ВРЕМЯ-ДИСК.расширение и тоже синхронизируется.'] },
  { t: 'Типы путей', p: ['Тип выбирается у каждого пути правила. От него зависит, как программа следит за изменениями и каким данным диска можно доверять.'],
    l: [
      'Локальный — обычный диск. Изменения подхватываются сразу (слежение Windows), время изменения надёжно.',
      'Сетевой (sshfs-win, SMB) — события об изменениях, сделанных с другой стороны, обычно не приходят, поэтому путь проверяется по таймеру (по умолчанию раз в 2 минуты). Включён контроль зависания: если диск не отвечает дольше 60 с, правило приостанавливается, ничего не удаляя, и повторяет попытки с нарастающей паузой (до 10 минут).',
      'Облако (rclone mount, bclone) — как сетевой, но время изменения в облаке не используется: изменения определяются по размеру файла (по документации rclone, например Mega не позволяет задать время изменения). Проверка раз в 10 минут, ожидание ответа 180 с. При конфликте побеждает версия с надёжным временем (локальная), облачная сохраняется копией. Ограничение: правка облачного файла без изменения размера не будет замечена.'] },
  { t: 'Советы для rclone mount и sshfs-win', l: [
    'У rclone каталог кэшируется (--dir-cache-time, по умолчанию 5 минут), поэтому чужие изменения видны с задержкой; период сканирования правила (scan) не делайте короче. Для облаков, умеющих уведомлять об изменениях, помогает --poll-interval.',
    '--vfs-write-back (по умолчанию 5 с) откладывает загрузку записанных файлов в облако: файл локально виден сразу, в облаке появится позже. Это нормально.',
    '--network-mode показывает диск как сетевой. Определение типа в программе от этого не зависит: тип задаётся вручную у пути.',
    'Облако Mega (по документации rclone) различает регистр имён и допускает несколько файлов с одинаковым именем; дубликаты лечатся командой rclone dedupe. Свойства Terabox (bclone) документированы хуже, поэтому для него тоже выбирайте «Облако».',
    'sshfs-win: права и владелец файлов через Windows недоступны, поэтому сохранение прав Linux возможно только при прямом SFTP (планируется).',
    'Маркер (служебный файл в корне пути, защита от подмены диска) можно отключить отметкой «без маркера» у пути. Для облаков корзину удобно выносить из облака: Trash=work.'] },
  { t: 'Защита данных', l: [
    'Если путь недоступен, не отвечает, потерял маркер или у диска изменился серийный номер, правило приостанавливается и ничего не удаляет.',
    'Если за один проход нужно удалить больше MaxDeletePercent файлов (по умолчанию 50%), проход отменяется.',
    'Свежие файлы (изменённые менее 2 секунд назад) пропускаются до следующего прохода. Копирование идёт через временный файл с проверкой, что исходник не менялся.'] },
  { t: 'Имена, отличающиеся только регистром', p: [
    'Сейчас: если в одной папке лежат файлы, которые различаются только регистром (на Linux, в облаке Mega), программа их пропускает и пишет предупреждение: Windows не различает такие имена при открытии, и можно перезаписать не тот файл. Остальные файлы синхронизируются.',
    'Планируется (при прямом SFTP, где доступ к файлам точный). Схема меток для копирования Linux → Windows, программа будет понимать её и в обратную сторону, и для других Linux/Android-устройств:'],
    l: ['Файл, отличающийся регистром от другого: file.ext, File.ext, FILE.ext, fIlE.ext → на Windows file.ext, File[linux_case].ext, FILE[linux_case].ext, fIlE[linux_case].ext.',
      'Отличается только расширение: another_file.ext, another_file.EXT, another_file.Ext → another_file.ext, another_file.EXT.ext, another_file.Ext.ext.',
      'Метка нужна только при совпадении имён без учёта регистра; обычные файлы (README.md) метки не получают. При обратной синхронизации Windows → Linux метки распознаются и убираются.'] },
  { t: 'Права и владелец на Linux (планируется)', p: ['При прямом SFTP обновлённый файл будет получать те же права и владельца, что были у заменяемого (например, исполняемый останется исполняемым).'] },
  { t: 'Параметры правила', l: [
    'mode (twoway/oneway), master — режим и главный путь; conflict (auto/ask); TrashDays (число дней, 0 — бессрочно, пусто — без корзины).',
    'scan — период полного сканирования, секунд (0 — выключено); watch — мгновенное слежение; debounce — пауза накопления событий, мс; recursive — вложенные папки.',
    'remote, cloud — номера путей сетевых/облачных дисков (например cloud=1,3); nomarker — пути без маркера; mtimetol — допуск времени, секунд; timeout — ожидание ответа удалённого диска, секунд.',
    'ignore — маски игнорируемых файлов через «;»; MaxDeletePercent; DeleteExtra; Trash (folder, work или путь); Temp (prefix, work); DirectBelow (КБ); notify; enabled.'] },
  { t: 'Веб-интерфейс', l: [
    'Порт без авторизации (WebPortOpen, по умолчанию 20001, слушает 127.0.0.1) предназначен для доверенных сетей. Любой, кто до него дотянется, может менять правила и просматривать файлы компьютера: закройте его брандмауэром от посторонних.',
    'Порт с логином и паролем (WebPortAuth) выдаёт cookie на 30 дней. Логин и пароль задаются на странице настроек или командой LocalSyncExt-cli.exe -web-user ИМЯ -web-pass ПАРОЛЬ.',
    'Все изменения через веб проверяются и применяются без перезапуска; предыдущий ini сохраняется как .ini.bak.'] },
  { t: 'Значок в трее', l: [
    'Зелёный — работа, жёлтый/зелёный мигают — идёт синхронизация, красный — пауза, зелёный с красной точкой — есть ошибки.',
    'Правая кнопка — меню; действия левого щелчка и двойного настраиваются (TrayClick, TrayDoubleClick); период мигания — TrayBlinkMs.'] },
];
function pageHelp() {
  const sections = HELP.map((sec) => h('div', { class: 'panel' }, h('h3', { style: 'margin-top:0' }, sec.t),
    ...(sec.p || []).map((p) => h('p', { style: 'margin:6px 0' }, p)),
    sec.l ? h('ul', { style: 'margin:6px 0;padding-left:20px' }, ...sec.l.map((x) => h('li', { style: 'margin:4px 0' }, x))) : null));
  shell('help', h('h2', {}, 'Справка'), ...sections);
}

// ---------- маршрутизация ----------
async function route() {
  clearTimers();
  document.querySelectorAll('.modal-bg').forEach((m) => m.remove());
  const hash = decodeURIComponent((location.hash || '#/').slice(1));
  try {
    if (!S.me) {
      try { S.me = await api('/api/me'); } catch (e) {
        if (e.status === 401 && e.data) { S.me = { auth: true, version: e.data.version, needLogin: true, needSetup: e.data.needSetup }; }
        else throw e;
      }
    }
    if (S.me.needLogin && hash !== '/login') { location.hash = '#/login'; return; }
    S.version = S.me.version || S.version;
    if (hash === '/login') {
      try { S.me = await api('/api/me'); location.hash = '#/'; return; }
      catch (e) {
        if (e.status !== 401) throw e;
        S.me = { auth: true, version: e.data && e.data.version, needLogin: true, needSetup: e.data && e.data.needSetup };
        pageLogin(S.me.needSetup);
        return;
      }
    }
    if (hash.startsWith('/rule/')) await pageRule(hash.slice(6));
    else if (hash === '/rule-new') await pageRule(null);
    else if (hash === '/status') await pageStatus();
    else if (hash === '/settings') await pageSettings();
    else if (hash === '/help') pageHelp();
    else await pageRules();
  } catch (e) {
    if (e.status === 401) { S.me = null; location.hash = '#/login'; return; }
    errorPage(e);
  }
}
window.addEventListener('hashchange', route);
route();
