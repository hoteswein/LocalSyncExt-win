// Проверка веб-интерфейса в jsdom на настоящем сервере: node ui.test.cjs <http://127.0.0.1:порт> <ini> [auth-url user pass]
const { JSDOM } = require('jsdom');
const fs = require('fs');
const base = process.argv[2], ini = process.argv[3];
const authBase = process.argv[4], authUser = process.argv[5], authPass = process.argv[6];
let failed = 0;
const ok = (c, m) => { if (!c) { failed++; console.log('FAIL:', m); } else console.log('ok  :', m); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function open(url) {
  const jar = {};
  const dom = await JSDOM.fromURL(url, {
    runScripts: 'dangerously', resources: 'usable', pretendToBeVisual: true,
    beforeParse(w) {
      w.fetch = async (u, init = {}) => {
        const full = new URL(u, url).href;
        init = { ...init, headers: { ...(init.headers || {}), cookie: Object.entries(jar).map(([k, v]) => k + '=' + v).join('; ') } };
        const r = await fetch(full, init);
        for (const c of r.headers.getSetCookie ? r.headers.getSetCookie() : []) { const [kv] = c.split(';'); const [k, v] = kv.split('='); if (/Max-Age=0|Max-Age=-1/i.test(c)) delete jar[k]; else jar[k] = v; }
        return r;
      };
      w.alert = (m) => { w.__alerts = (w.__alerts || []).concat(m); };
      w.confirm = () => true;
    },
  });
  return dom;
}
async function until(w, fn, what, ms = 6000) {
  const end = Date.now() + ms;
  while (Date.now() < end) { try { const v = fn(); if (v) return v; } catch (e) { /* ждём */ } await sleep(50); }
  ok(false, 'таймаут: ' + what); return null;
}
const $ = (w, sel) => w.document.querySelector(sel);
const $$ = (w, sel) => [...w.document.querySelectorAll(sel)];
const byText = (w, sel, text) => $$(w, sel).find((e) => e.textContent.includes(text));
const type = (w, el, v) => { el.value = v; el.dispatchEvent(new w.Event('input', { bubbles: true })); el.dispatchEvent(new w.Event('change', { bubbles: true })); };

(async () => {
  const dom = await open(base);
  const w = dom.window;
  const errors = []; w.addEventListener('error', (e) => errors.push(e.message));
  await until(w, () => $(w, '.card'), 'карточки правил');
  ok($$(w, '.card').length >= 2, 'в списке правил есть карточки');
  ok(/Первое правило/.test($(w, '.card .desc').textContent), 'аннотация правила видна внизу карточки');
  ok(/LocalSyncExt\s+\S+/.test($(w, 'footer').textContent), 'внизу страницы указана версия: ' + $(w, 'footer').textContent);

  // редактор: открыть правило кликом
  $(w, '.card').click();
  await until(w, () => $(w, '.pathrow'), 'редактор правила');
  ok($$(w, '.pathrow').length === 2, 'в редакторе два пути');
  ok($(w, '.pathrow input[type=text]').value.length > 3, 'путь подставлен в поле');

  // обзор: добавить пути через модальное окно
  const addBrowse = byText(w, 'button', 'Обзор (можно несколько)');
  addBrowse.click();
  await until(w, () => $(w, '.modal .frow'), 'окно обзора');
  const rows = $$(w, '.modal .frow');
  ok(rows.length >= 1, 'обзор показывает содержимое');
  const dirRow = $$(w, '.modal .fname').find((e) => e.textContent.includes('📁'));
  ok(!!dirRow, 'в обзоре есть папки');
  dirRow.click();
  await until(w, () => $(w, '.modal input[type=text]').value.length > 0, 'переход в папку');
  const cbs = $$(w, '.modal .frow input[type=checkbox]');
  if (cbs.length) { cbs[0].checked = true; cbs[0].dispatchEvent(new w.Event('change')); }
  const pick = byText(w, '.modal button', 'Выбрать отмеченное');
  ok(!pick.disabled || cbs.length === 0, 'кнопка выбора доступна после отметки');
  if (cbs.length) { pick.click(); await sleep(100); ok($$(w, '.pathrow').length >= 2, 'выбранный путь добавлен'); }
  else byText(w, '.modal button', 'Отмена').click();

  // изменить аннотацию и сохранить
  const desc = $$(w, 'input[type=text]').find((i) => /аннотация/i.test(i.placeholder));
  type(w, desc, 'Изменено из теста интерфейса');
  byText(w, 'button', 'Сохранить').click();
  await until(w, () => w.location.hash === '#/', 'возврат к списку после сохранения');
  const saved = fs.readFileSync(ini, 'utf8');
  ok(saved.includes('Изменено из теста интерфейса'), 'аннотация записана в ini');
  ok(/\[Settings\]/i.test(saved), 'раздел [Settings] сохранён');

  // новое правило с ошибкой (один путь) → сообщение
  w.location.hash = '#/rule-new';
  await until(w, () => $(w, '.pathrow'), 'форма нового правила');
  type(w, $$(w, 'input[type=text]').find((i) => /Название/.test(i.placeholder)), 'Тестовое');
  type(w, $$(w, '.pathrow input[type=text]')[0], process.argv[7] + '/n1');
  byText(w, 'button', 'Сохранить').click();
  await until(w, () => $(w, '.errbox'), 'сообщение об ошибке');
  ok(/минимум два пути/.test($(w, '.errbox').textContent), 'ошибка про два пути показана');
  type(w, $$(w, '.pathrow input[type=text]')[1], process.argv[7] + '/n2');
  const modeSel = $$(w, 'select').find((s) => [...s.options].some((o) => o.value === 'oneway'));
  modeSel.value = 'oneway'; modeSel.dispatchEvent(new w.Event('change'));
  await until(w, () => byText(w, 'label', 'Главный путь'), 'поле «Главный путь» для одностороннего режима');
  $$(w, '.pathrow input[type=checkbox]')[1].checked = true; $$(w, '.pathrow input[type=checkbox]')[1].dispatchEvent(new w.Event('change'));
  const kindSel = $$(w, '.pathrow select')[1];
  kindSel.value = 'cloud'; kindSel.dispatchEvent(new w.Event('change'));
  const trash = $$(w, 'select').find((s) => [...s.options].some((o) => o.value === 'forever'));
  trash.value = 'none'; trash.dispatchEvent(new w.Event('change'));
  byText(w, 'button', 'Сохранить').click();
  await until(w, () => fs.readFileSync(ini, 'utf8').includes('[Тестовое]'), 'новое правило в ini', 8000);
  const t2 = fs.readFileSync(ini, 'utf8');
  ok(/mode=oneway/.test(t2) && /NoMarker=2/.test(t2) && /TrashDays=\r?\n/.test(t2) && /Cloud=2/.test(t2), 'опции нового правила записаны (oneway, NoMarker=2, TrashDays=пусто, Cloud=2)');
  // справка
  w.location.hash = '#/help';
  await until(w, () => byText(w, 'h3', 'Типы путей'), 'страница справки');
  ok(!!byText(w, 'h3', 'Имена, отличающиеся только регистром') && /\[linux_case\]/.test($(w, 'main').textContent), 'в справке описана схема меток [linux_case]');
  // повторно открыть правило: тип пути «облако» читается из ini
  w.location.hash = '#/rule/' + encodeURIComponent('Тестовое');
  await until(w, () => $$(w, '.pathrow select')[1], 'редактор сохранённого правила');
  ok($$(w, '.pathrow select')[1].value === 'cloud', 'тип пути «облако» восстановлен из ini');

  // статус и журнал
  w.location.hash = '#/status';
  await until(w, () => $(w, 'table'), 'таблица статусов');
  ok($$(w, 'tbody tr').length >= 2, 'в таблице статусов есть правила');
  await until(w, () => /\[/.test($(w, 'pre.log').textContent), 'записи журнала');
  ok(/web|main|Rule/.test($(w, 'pre.log').textContent), 'журнал содержит события');
  ok(/LocalSyncExt\s+\S+/.test($(w, 'footer').textContent), 'версия на странице статуса');

  // настройки
  w.location.hash = '#/settings';
  await until(w, () => byText(w, 'h3', 'Веб-интерфейс'), 'страница настроек');
  const tc = $$(w, 'select').find((s) => [...s.options].some((o) => o.value === 'syncnow') && s.previousSibling);
  tc.value = 'pause'; tc.dispatchEvent(new w.Event('change'));
  byText(w, 'button', 'Сохранить настройки').click();
  await until(w, () => $(w, '.okbox'), 'подтверждение сохранения настроек');
  ok(/TrayClick=pause/i.test(fs.readFileSync(ini, 'utf8')), 'настройка записана в ini');
  ok(errors.length === 0, 'ошибок JavaScript нет ' + errors.join('|'));
  w.close();

  if (authBase) {
    const d2 = await open(authBase);
    const w2 = d2.window;
    await until(w2, () => $(w2, '.login'), 'страница входа на порту с авторизацией');
    const [u, p] = $$(w2, '.login input');
    type(w2, u, authUser); type(w2, p, 'неверный');
    byText(w2, 'button', 'Войти').click();
    await until(w2, () => $(w2, '.errbox'), 'ошибка неверного пароля');
    ok(/неверн/i.test($(w2, '.errbox').textContent), 'неверный пароль отклонён');
    type(w2, p, authPass);
    byText(w2, 'button', 'Войти').click();
    await until(w2, () => $(w2, '.card'), 'список правил после входа');
    ok(!!byText(w2, 'header .who', authUser) || !!byText(w2, 'header', authUser), 'имя пользователя показано, есть «Выйти»');
    byText(w2, 'button', 'Выйти').click();
    await until(w2, () => $(w2, '.login'), 'возврат на вход после выхода');
    w2.close();
  }
  console.log(failed ? `\n${failed} проверок не прошло` : '\nвсе проверки интерфейса прошли');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.log('СБОЙ ТЕСТА:', e); process.exit(2); });
