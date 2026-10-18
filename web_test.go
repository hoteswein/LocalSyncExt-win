package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPBKDF2Vector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32))
	if got != "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b" {
		t.Fatalf("PBKDF2: %s", got)
	}
	got = hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 4096, 32))
	if got != "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a" {
		t.Fatalf("PBKDF2 4096: %s", got)
	}
}

func TestPasswordAndTokens(t *testing.T) {
	h := hashPassword("секрет123")
	if !checkPassword(h, "секрет123") || checkPassword(h, "секрет124") || checkPassword("мусор", "x") {
		t.Fatal("проверка пароля")
	}
	cs := loadCreds(t.TempDir())
	if cs.HasPassword() || cs.Verify("a", "b") {
		t.Fatal("пустое хранилище")
	}
	if cs.Set("", "123456") == nil || cs.Set("u", "123") == nil || cs.Set("a|b", "123456") == nil {
		t.Fatal("некорректные логин/пароль должны отклоняться")
	}
	if err := cs.Set("admin", "123456"); err != nil {
		t.Fatal(err)
	}
	if !cs.Verify("admin", "123456") || cs.Verify("admin", "x23456") || cs.Verify("root", "123456") {
		t.Fatal("Verify")
	}
	tok := cs.Token("admin", time.Hour)
	if u, ok := cs.CheckToken(tok); !ok || u != "admin" {
		t.Fatal("токен должен быть действителен")
	}
	if _, ok := cs.CheckToken(tok + "x"); ok {
		t.Fatal("испорченный токен принят")
	}
	if _, ok := cs.CheckToken(cs.Token("admin", -time.Second)); ok {
		t.Fatal("просроченный токен принят")
	}
	// после смены пароля старые токены недействительны, а состояние переживает перезапуск
	cs.Set("admin", "654321")
	if _, ok := cs.CheckToken(tok); ok {
		t.Fatal("старый токен должен перестать действовать")
	}
	cs2 := loadCreds(filepath.Dir(cs.path))
	if !cs2.Verify("admin", "654321") {
		t.Fatal("логин не сохранился")
	}
	if u, ok := cs2.CheckToken(cs.Token("admin", time.Hour)); !ok || u != "admin" {
		t.Fatal("токен должен проверяться после перезагрузки хранилища")
	}
}

func TestHostAllowedAndWebCfg(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:20001": true, "192.168.1.5": true, "[::1]:20002": true, "localhost:20001": true,
		"evil.example.com": false, "evil.com:20001": false, "mypc.zt": true,
	} {
		if got := hostAllowed(host, []string{"mypc.zt"}); got != want {
			t.Errorf("hostAllowed(%q)=%v", host, got)
		}
	}
	c, errs := parseWebCfg(map[string]string{})
	if len(errs) != 0 || !c.Enabled || c.OpenPort != 20001 || c.OpenBind != "127.0.0.1" || c.AuthPort != 0 {
		t.Fatalf("умолчания: %+v %v", c, errs)
	}
	c, errs = parseWebCfg(map[string]string{"webportopen": "20005", "webopenbind": "0.0.0.0", "webportauth": "20006", "webhosts": "a.b, c.d"})
	if len(errs) != 0 || c.OpenPort != 20005 || c.AuthPort != 20006 || len(c.Hosts) != 2 {
		t.Fatalf("настройки: %+v %v", c, errs)
	}
	_, errs = parseWebCfg(map[string]string{"webportopen": "99999", "webopenbind": "не-ip", "webui": "может"})
	if len(errs) != 3 {
		t.Fatalf("ожидалось 3 ошибки: %v", errs)
	}
	_, errs = parseWebCfg(map[string]string{"webportopen": "20001", "webportauth": "20001"})
	if len(errs) != 1 {
		t.Fatalf("одинаковые порты: %v", errs)
	}
}

type webEnv struct {
	root, ini string
	en        *Engine
	ws        *WebServer
	reloaded  atomic.Int32
}

func newWebEnv(t *testing.T) *webEnv {
	t.Helper()
	root := t.TempDir()
	r := filepath.ToSlash(root)
	ini := "; мой комментарий\n\n[Settings]\nnotifications=1\n\n[Rule 1]\ndescription=Тестовое правило\nscan=0\nwatch=0\nTrashDays=\n" + r + "/a\n" + r + "/b\n"
	p := filepath.Join(root, "t.ini")
	os.WriteFile(p, []byte(ini), 0o644)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e := &webEnv{root: root, ini: p}
	e.en = NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard)
	e.ws = NewWebServer(e.en, p, filepath.Join(root, "t.log"), filepath.Join(root, "state"), "v-test", func() { e.reloaded.Add(1) })
	return e
}

func (e *webEnv) srv(t *testing.T, open bool) *httptest.Server {
	s := httptest.NewServer(e.ws.handler(open))
	t.Cleanup(s.Close)
	return s
}

func do(t *testing.T, c *http.Client, method, url string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if method != "GET" {
		req.Header.Set("X-LSX", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestOpenPortAPI(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv(t, true)
	c := s.Client()
	if code, b := do(t, c, "GET", s.URL+"/", nil, nil); code != 200 || !strings.Contains(string(b), "app.js") {
		t.Fatalf("главная страница: %d", code)
	}
	if code, _ := do(t, c, "GET", s.URL+"/app.js", nil, nil); code != 200 {
		t.Fatal("app.js")
	}
	code, b := do(t, c, "GET", s.URL+"/api/me", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"auth":false`) {
		t.Fatalf("me: %d %s", code, b)
	}
	code, b = do(t, c, "GET", s.URL+"/api/state", nil, nil)
	var st struct {
		Version string
		Rules   []struct {
			Name, Desc string
			Paths      []string
		}
	}
	json.Unmarshal(b, &st)
	if code != 200 || st.Version != "v-test" || len(st.Rules) != 1 || st.Rules[0].Desc != "Тестовое правило" || len(st.Rules[0].Paths) != 2 {
		t.Fatalf("state: %d %s", code, b)
	}
	// защита от CSRF и DNS rebinding
	if code, _ := do(t, c, "POST", s.URL+"/api/all", map[string]string{"action": "pause"}, map[string]string{"X-LSX": ""}); code != 403 {
		t.Fatalf("без X-LSX должно быть 403, а не %d", code)
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/all", map[string]string{"action": "pause"}, map[string]string{"Origin": "http://evil.example.com"}); code != 403 {
		t.Fatalf("чужой Origin должен давать 403, а не %d", code)
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/state", nil, map[string]string{"Host": "evil.example.com"}); code != 403 {
		t.Fatalf("чужое имя узла должно давать 403, а не %d", code)
	}
	if e.en.AllPaused() {
		t.Fatal("отклонённые запросы не должны менять состояние")
	}
	// действия
	e.en.rt["Rule 1"] = newRT()
	if code, _ := do(t, c, "POST", s.URL+"/api/rule", map[string]string{"name": "Rule 1", "action": "pause"}, nil); code != 200 || !e.en.IsPaused("Rule 1") {
		t.Fatalf("пауза не применилась: %d", code)
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/rule", map[string]string{"name": "Rule 1", "action": "resume"}, nil); code != 200 || e.en.IsPaused("Rule 1") {
		t.Fatal("продолжение не применилось")
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/rule", map[string]string{"name": "нет такого", "action": "sync"}, nil); code != 404 {
		t.Fatal("несуществующее правило должно давать 404")
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/rule", map[string]string{"name": "Rule 1", "action": "взорвать"}, nil); code != 400 {
		t.Fatal("неизвестное действие должно давать 400")
	}
}

func TestAuthPort(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv(t, false)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if code, b := do(t, c, "GET", s.URL+"/api/state", nil, nil); code != 401 {
		t.Fatalf("без входа должно быть 401: %d %s", code, b)
	}
	code, b := do(t, c, "GET", s.URL+"/api/me", nil, nil)
	if code != 401 || !strings.Contains(string(b), `"needSetup":true`) {
		t.Fatalf("me без пароля: %d %s", code, b)
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/login", map[string]string{"user": "a", "password": "b"}, nil); code != 403 {
		t.Fatal("вход без заданного пароля должен давать 403")
	}
	if err := e.ws.creds.Set("admin", "pass1234"); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, c, "POST", s.URL+"/api/login", map[string]string{"user": "admin", "password": "pass1234"}, nil); code != 200 {
		t.Fatal("верный вход отклонён")
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/state", nil, nil); code != 200 {
		t.Fatal("после входа cookie должна открывать API")
	}
	if code, b := do(t, c, "GET", s.URL+"/api/me", nil, nil); code != 200 || !strings.Contains(string(b), `"user":"admin"`) {
		t.Fatalf("me после входа: %d %s", code, b)
	}
	// смена пароля на порту с авторизацией не выбрасывает текущий сеанс
	if code, _ := do(t, c, "POST", s.URL+"/api/credentials", map[string]string{"user": "admin", "password": "newpass99"}, nil); code != 200 {
		t.Fatal("смена пароля")
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/state", nil, nil); code != 200 {
		t.Fatal("текущий сеанс должен продолжать работать после смены пароля")
	}
	// выход
	do(t, c, "POST", s.URL+"/api/logout", map[string]string{}, nil)
	if code, _ := do(t, c, "GET", s.URL+"/api/state", nil, nil); code != 401 {
		t.Fatal("после выхода доступ должен закрыться")
	}
	// подбор пароля ограничивается
	var last int
	for i := 0; i < 6; i++ {
		last, _ = do(t, &http.Client{}, "POST", s.URL+"/api/login", map[string]string{"user": "admin", "password": "wrong" + string(rune('a'+i))}, nil)
	}
	if last != 429 {
		t.Fatalf("после серии неудач ожидался 429, получен %d", last)
	}
	if code, _ := do(t, &http.Client{}, "POST", s.URL+"/api/login", map[string]string{"user": "admin", "password": "newpass99"}, nil); code != 429 {
		t.Fatal("блокировка действует и на верный пароль")
	}
	// поддельный токен
	req, _ := http.NewRequest("GET", s.URL+"/api/state", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "YWRtaW58OTk5OTk5OTk5OQ.fake"})
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("поддельная cookie принята: %d", resp.StatusCode)
	}
}

func TestConfigAPI(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv(t, true)
	c := s.Client()
	var cfg apiConfig
	code, b := do(t, c, "GET", s.URL+"/api/config", nil, nil)
	if code != 200 {
		t.Fatalf("GET config: %d %s", code, b)
	}
	json.Unmarshal(b, &cfg)
	if len(cfg.Rules) != 1 || cfg.Rules[0].Desc != "Тестовое правило" || len(cfg.Rules[0].Paths) != 2 || cfg.Rev == "" {
		t.Fatalf("конфиг: %+v", cfg)
	}
	hasEmptyTrash := false
	for _, kv := range cfg.Rules[0].Opts {
		if kv.K == "trashdays" && kv.V == "" {
			hasEmptyTrash = true
		}
	}
	if !hasEmptyTrash {
		t.Fatalf("явно пустой TrashDays должен сохраняться: %+v", cfg.Rules[0].Opts)
	}
	r := filepath.ToSlash(e.root)
	// добавить правило, изменить аннотацию и настройку
	cfg.Rules[0].Desc = "Новая аннотация"
	cfg.Rules = append(cfg.Rules, apiRuleCfg{Name: "Второе", Desc: "для проверки", Opts: []KV{{"mode", "oneway"}, {"nomarker", "2"}},
		Paths: []string{r + "/c", r + "/d"}})
	cfg.Settings = append(cfg.Settings, KV{"trayclick", "ui"})
	code, b = do(t, c, "POST", s.URL+"/api/config", cfg, nil)
	if code != 200 {
		t.Fatalf("POST config: %d %s", code, b)
	}
	saved, _ := os.ReadFile(e.ini)
	txt := string(saved)
	for _, want := range []string{"; мой комментарий", "[Settings]", "TrayClick=ui", "[Второе]", "description=Для проверки", "NoMarker=2", "TrashDays=\n"} {
		if want == "description=Для проверки" {
			want = "description=для проверки"
		}
		if !strings.Contains(strings.ReplaceAll(txt, "\r\n", "\n"), want) {
			t.Errorf("в сохранённом ini нет %q:\n%s", want, txt)
		}
	}
	if _, err := os.Stat(e.ini + ".bak"); err != nil {
		t.Fatal("должна создаваться копия .bak")
	}
	time.Sleep(600 * time.Millisecond)
	if e.reloaded.Load() != 1 {
		t.Fatalf("перезагрузка конфига должна быть вызвана один раз, вызвана %d", e.reloaded.Load())
	}
	// конфиг после сохранения читается обратно
	cf2, err := loadConfig(e.ini)
	if err != nil || len(cf2.Rules) != 2 || cf2.Rules[1].Mode != "oneway" || !cf2.Rules[1].NoMarker[1] {
		t.Fatalf("после сохранения: %v %+v", err, cf2)
	}
	// устаревшая версия
	code, _ = do(t, c, "POST", s.URL+"/api/config", cfg, nil)
	if code != 409 {
		t.Fatalf("старая ревизия должна давать 409, получено %d", code)
	}
	// ошибки не записываются
	before, _ := os.ReadFile(e.ini)
	_, b = do(t, c, "GET", s.URL+"/api/config", nil, nil)
	json.Unmarshal(b, &cfg)
	structural := cfg
	structural.Rules = append([]apiRuleCfg(nil), cfg.Rules...)
	structural.Rules = append(structural.Rules,
		apiRuleCfg{Name: "Ещё", Paths: []string{"относительный", r + "/y"}}, // не полный путь
		apiRuleCfg{Name: "Rule 1", Paths: []string{r + "/p", r + "/q"}},     // дубль названия
		apiRuleCfg{Name: "[x]", Paths: []string{r + "/p", r + "/q"}})        // недопустимое название
	code, b = do(t, c, "POST", s.URL+"/api/config", structural, nil)
	var er struct {
		Error  string
		Errors []string
	}
	json.Unmarshal(b, &er)
	if code != 400 || len(er.Errors) != 3 {
		t.Fatalf("ожидались 3 структурные ошибки: %d %s", code, b)
	}
	semantic := cfg
	semantic.Rules = append([]apiRuleCfg(nil), cfg.Rules...)
	semantic.Rules = append(semantic.Rules,
		apiRuleCfg{Name: "Плохое", Paths: []string{r + "/x"}},                                        // один путь
		apiRuleCfg{Name: "Перекр", Paths: []string{r + "/a/inner", r + "/z"}},                        // пересечение с Rule 1
		apiRuleCfg{Name: "Опция", Opts: []KV{{"mode", "бред"}}, Paths: []string{r + "/m", r + "/n"}}) // неверное значение
	code, b = do(t, c, "POST", s.URL+"/api/config", semantic, nil)
	er.Errors = nil
	json.Unmarshal(b, &er)
	if code != 400 || len(er.Errors) != 3 {
		t.Fatalf("ожидались 3 ошибки проверки правил: %d %s", code, b)
	}
	after, _ := os.ReadFile(e.ini)
	if !bytes.Equal(before, after) {
		t.Fatal("при ошибках конфиг изменяться не должен")
	}
	// неверные настройки веб-интерфейса тоже блокируют сохранение
	badWeb := cfg
	badWeb.Settings = append(append([]KV(nil), cfg.Settings...), KV{"webportopen", "70000"})
	if code, _ := do(t, c, "POST", s.URL+"/api/config", badWeb, nil); code != 400 {
		t.Fatal("неверный порт должен блокировать сохранение")
	}
}

func TestFSAPIAndLogs(t *testing.T) {
	e := newWebEnv(t)
	s := e.srv(t, true)
	c := s.Client()
	d := filepath.Join(e.root, "browse")
	os.MkdirAll(filepath.Join(d, "zdir"), 0o755)
	os.MkdirAll(filepath.Join(d, "Adir"), 0o755)
	os.WriteFile(filepath.Join(d, "b.txt"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("1"), 0o644)
	code, b := do(t, c, "GET", s.URL+"/api/fs?path="+d, nil, nil)
	var fr fsResp
	json.Unmarshal(b, &fr)
	if code != 200 || len(fr.Entries) != 4 || !fr.Entries[0].Dir || fr.Entries[0].Name != "Adir" || fr.Entries[1].Name != "zdir" || fr.Entries[2].Name != "a.txt" || fr.Entries[3].Size != 5 {
		t.Fatalf("fs: %d %+v", code, fr)
	}
	if fr.Parent != e.root {
		t.Fatalf("родитель: %q", fr.Parent)
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/fs?path="+filepath.Join(d, "нет"), nil, nil); code != 400 {
		t.Fatal("несуществующий путь должен давать 400")
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/fs?path=относительный", nil, nil); code != 400 {
		t.Fatal("относительный путь должен давать 400")
	}
	if code, b := do(t, c, "GET", s.URL+"/api/fs?path=", nil, nil); code != 200 || !strings.Contains(string(b), `"entries"`) {
		t.Fatalf("корни: %d", code)
	}
	// журнал
	e.en.logf("Rule 1", "INFO", "обычное")
	e.en.logf("Rule 1", "WARN", "предупреждение")
	e.en.logf("Другое", "ERROR", "ошибка")
	_, b = do(t, c, "GET", s.URL+"/api/log?level=WARN", nil, nil)
	if strings.Contains(string(b), "обычное") || !strings.Contains(string(b), "предупреждение") || !strings.Contains(string(b), "ошибка") {
		t.Fatalf("фильтр уровня: %s", b)
	}
	_, b = do(t, c, "GET", s.URL+"/api/log?rule=Другое", nil, nil)
	if strings.Contains(string(b), "предупреждение") || !strings.Contains(string(b), "ошибка") {
		t.Fatalf("фильтр правила: %s", b)
	}
	if code, _ := do(t, c, "GET", s.URL+"/api/logfile", nil, nil); code != 404 {
		t.Fatal("нет файла журнала — 404")
	}
	big := strings.Repeat("строка журнала номер 123456\n", 10000)
	os.WriteFile(e.ws.logPath, []byte(big), 0o644)
	code, b = do(t, c, "GET", s.URL+"/api/logfile?kb=4", nil, nil)
	if code != 200 || len(b) > 4096 || !strings.HasPrefix(string(b), "строка журнала") {
		t.Fatalf("хвост журнала: %d, %d байт", code, len(b))
	}
}

func TestWebServerListeners(t *testing.T) {
	e := newWebEnv(t)
	e.ws.Apply(webCfg{Enabled: true, OpenPort: 0, AuthPort: 0})
	if e.ws.URL() != "" {
		t.Fatal("без портов адреса быть не должно")
	}
	// выбрать свободный порт
	e.ws.Apply(webCfg{Enabled: true, OpenPort: freePort(t), OpenBind: "127.0.0.1"})
	u := e.ws.URL()
	if u == "" {
		t.Fatal("сервер не запустился")
	}
	resp, err := http.Get(u + "api/me")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("запрос к запущенному серверу: %v", err)
	}
	e.ws.Apply(webCfg{Enabled: false})
	time.Sleep(100 * time.Millisecond)
	if _, err := http.Get(u + "api/me"); err == nil {
		t.Fatal("после WebUI=0 порт должен закрыться")
	}
	e.ws.Stop()
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
