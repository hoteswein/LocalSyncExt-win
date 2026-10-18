package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

const sessionCookie = "lsx_session"
const sessionTTL = 30 * 24 * time.Hour

// ---------- настройки веб-интерфейса ----------

type webCfg struct {
	Enabled            bool
	OpenPort, AuthPort int
	OpenBind, AuthBind string
	Hosts              []string
}

// parseWebCfg читает [Settings]: WebUI, WebPortOpen, WebOpenBind, WebPortAuth, WebAuthBind, WebHosts.
// По умолчанию включена только страница без авторизации на 127.0.0.1:20001.
func parseWebCfg(s map[string]string) (webCfg, []string) {
	c := webCfg{Enabled: true, OpenPort: 20001, OpenBind: "127.0.0.1", AuthPort: 0, AuthBind: "0.0.0.0"}
	var errs []string
	if v, ok := s["webui"]; ok && v != "" {
		if b, ok := parseBool(v); ok {
			c.Enabled = b
		} else {
			errs = append(errs, fmt.Sprintf("WebUI=%q: нужно 1 или 0", v))
		}
	}
	port := func(key, label string, dst *int) {
		if v, ok := s[key]; ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 65535 {
				errs = append(errs, fmt.Sprintf("%s=%q: нужен порт 0..65535 (0 — выключено)", label, v))
				return
			}
			*dst = n
		}
	}
	port("webportopen", "WebPortOpen", &c.OpenPort)
	port("webportauth", "WebPortAuth", &c.AuthPort)
	bind := func(key, label string, dst *string) {
		if v := strings.TrimSpace(s[key]); v != "" {
			if v != "localhost" && net.ParseIP(v) == nil {
				errs = append(errs, fmt.Sprintf("%s=%q: нужен IP-адрес (например 0.0.0.0 или 127.0.0.1)", label, v))
				return
			}
			*dst = v
		}
	}
	bind("webopenbind", "WebOpenBind", &c.OpenBind)
	bind("webauthbind", "WebAuthBind", &c.AuthBind)
	for _, h := range strings.FieldsFunc(s["webhosts"], func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		c.Hosts = append(c.Hosts, strings.ToLower(h))
	}
	if c.Enabled && c.OpenPort != 0 && c.OpenPort == c.AuthPort {
		errs = append(errs, "WebPortOpen и WebPortAuth не могут совпадать")
	}
	return c, errs
}

// hostAllowed защищает от DNS rebinding: разрешены IP-адреса, localhost и имена из WebHosts.
func hostAllowed(hostHeader string, allowed []string) bool {
	h := hostHeader
	if host, _, err := net.SplitHostPort(hostHeader); err == nil {
		h = host
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if net.ParseIP(h) != nil || h == "localhost" {
		return true
	}
	for _, a := range allowed {
		if a == h {
			return true
		}
	}
	return false
}

// ---------- сервер ----------

type WebServer struct {
	en       *Engine
	creds    *credStore
	iniPath  string
	logPath  string
	stateDir string
	version  string
	reload   func()
	limiter  *loginLimiter

	mu   sync.Mutex
	cur  webCfg
	on   bool
	srvs []*http.Server
	urls []string
}

func NewWebServer(en *Engine, iniPath, logPath, stateDir, version string, reload func()) *WebServer {
	return &WebServer{en: en, creds: loadCreds(stateDir), iniPath: iniPath, logPath: logPath,
		stateDir: stateDir, version: version, reload: reload, limiter: newLoginLimiter()}
}

func (w *WebServer) hosts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur.Hosts
}

// Apply запускает, останавливает или перезапускает слушающие порты при изменении настроек.
func (w *WebServer) Apply(c webCfg) {
	w.mu.Lock()
	if w.on && reflect.DeepEqual(c, w.cur) {
		w.mu.Unlock()
		return
	}
	old := w.srvs
	w.srvs, w.urls, w.on, w.cur = nil, nil, false, c
	w.mu.Unlock()
	for _, s := range old {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		_ = s.Close()
	}
	if !c.Enabled {
		w.en.logf("web", "INFO", "веб-интерфейс выключен (WebUI=0)")
		return
	}
	var srvs []*http.Server
	var urls []string
	start := func(port int, bind string, open bool) {
		if port == 0 {
			return
		}
		addr := net.JoinHostPort(bind, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			w.en.logf("web", "ERROR", "не удалось открыть порт %s: %v", addr, err)
			return
		}
		srv := &http.Server{Handler: w.handler(open), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go srv.Serve(ln)
		srvs = append(srvs, srv)
		host := bind
		if bind == "0.0.0.0" || bind == "::" {
			host = "127.0.0.1"
		}
		urls = append(urls, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/")
		kind := "с авторизацией"
		if open {
			kind = "БЕЗ авторизации"
		}
		w.en.logf("web", "INFO", "веб-интерфейс слушает %s (%s)", addr, kind)
	}
	start(c.OpenPort, c.OpenBind, true)
	start(c.AuthPort, c.AuthBind, false)
	w.mu.Lock()
	w.srvs, w.urls, w.on = srvs, urls, true
	w.mu.Unlock()
}

func (w *WebServer) Stop() {
	w.Apply(webCfg{})
}

// URL — адрес, который можно открыть на этом компьютере (для трея); "" если веб-интерфейс не работает.
func (w *WebServer) URL() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.urls) == 0 {
		return ""
	}
	return w.urls[0]
}

// ---------- обработчики ----------

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func jerr(rw http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(rw, code, map[string]any{"error": fmt.Sprintf(format, a...)})
}

func readJSON(rw http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(rw, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jerr(rw, 400, "некорректный запрос: %v", err)
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// secure: заголовки безопасности, проверка Host/Origin и обязательный заголовок для изменяющих запросов.
func (w *WebServer) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		if !hostAllowed(r.Host, w.hosts()) {
			jerr(rw, 403, "недопустимое имя узла в запросе (добавьте его в WebHosts)")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-LSX") != "1" {
				jerr(rw, 403, "нужен заголовок X-LSX")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
					jerr(rw, 403, "запрос с чужого источника")
					return
				}
			}
		}
		next.ServeHTTP(rw, r)
	})
}

func (w *WebServer) sessionUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return w.creds.CheckToken(c.Value)
}

func (w *WebServer) setCookie(rw http.ResponseWriter, user string) {
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: w.creds.Token(user, sessionTTL), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
}

func (w *WebServer) handler(open bool) http.Handler {
	mux := http.NewServeMux()
	static := func(name, ctype string) {
		data, _ := webFS.ReadFile("web/" + name)
		mux.HandleFunc("GET /"+name, func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", ctype)
			_, _ = rw.Write(data)
		})
		if name == "index.html" {
			mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Content-Type", ctype)
				_, _ = rw.Write(data)
			})
		}
	}
	static("index.html", "text/html; charset=utf-8")
	static("app.js", "text/javascript; charset=utf-8")
	static("style.css", "text/css; charset=utf-8")

	guard := func(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if !open {
				if _, ok := w.sessionUser(r); !ok {
					jerr(rw, 401, "требуется вход")
					return
				}
			}
			h(rw, r)
		}
	}

	mux.HandleFunc("GET /api/me", func(rw http.ResponseWriter, r *http.Request) {
		if open {
			writeJSON(rw, 200, map[string]any{"auth": false, "user": "", "version": w.version})
			return
		}
		if u, ok := w.sessionUser(r); ok {
			writeJSON(rw, 200, map[string]any{"auth": true, "user": u, "version": w.version})
			return
		}
		writeJSON(rw, 401, map[string]any{"error": "требуется вход", "auth": true, "needSetup": !w.creds.HasPassword(), "version": w.version})
	})
	mux.HandleFunc("POST /api/login", func(rw http.ResponseWriter, r *http.Request) {
		if open {
			jerr(rw, 400, "на этом порту вход не нужен")
			return
		}
		var in struct{ User, Password string }
		in.User = ""
		var raw struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if !readJSON(rw, r, &raw) {
			return
		}
		in.User, in.Password = raw.User, raw.Password
		if !w.creds.HasPassword() {
			jerr(rw, 403, "логин и пароль ещё не заданы: задайте их на странице настроек через порт без авторизации или командой -web-user/-web-pass")
			return
		}
		ip := clientIP(r)
		if wait := w.limiter.Wait(ip); wait > 0 {
			rw.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			jerr(rw, 429, "слишком много попыток, подождите %d с", int(wait.Seconds())+1)
			return
		}
		if !w.creds.Verify(strings.TrimSpace(in.User), in.Password) {
			w.limiter.Fail(ip)
			w.en.logf("web", "WARN", "неудачный вход с %s", ip)
			jerr(rw, 401, "неверный логин или пароль")
			return
		}
		w.limiter.OK(ip)
		w.setCookie(rw, w.creds.User())
		w.en.logf("web", "INFO", "вход выполнен с %s", ip)
		writeJSON(rw, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/logout", func(rw http.ResponseWriter, r *http.Request) {
		http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		writeJSON(rw, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/state", guard(w.apiState))
	mux.HandleFunc("POST /api/rule", guard(w.apiRuleAction))
	mux.HandleFunc("POST /api/all", guard(w.apiAllAction))
	mux.HandleFunc("GET /api/log", guard(w.apiLog))
	mux.HandleFunc("GET /api/logfile", guard(w.apiLogFile))
	mux.HandleFunc("GET /api/fs", guard(w.apiFS))
	mux.HandleFunc("GET /api/config", guard(w.apiConfigGet))
	mux.HandleFunc("POST /api/config", guard(w.apiConfigSave))
	mux.HandleFunc("POST /api/credentials", guard(func(rw http.ResponseWriter, r *http.Request) { w.apiCredentials(rw, r, open) }))
	return w.secure(mux)
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (w *WebServer) apiState(rw http.ResponseWriter, r *http.Request) {
	type ruleOut struct {
		Name       string   `json:"name"`
		Desc       string   `json:"desc"`
		Status     string   `json:"status"`
		Mode       string   `json:"mode"`
		Err        string   `json:"err"`
		Paths      []string `json:"paths"`
		Enabled    bool     `json:"enabled"`
		Paused     bool     `json:"paused"`
		LastRun    int64    `json:"lastRun"`
		LastChange int64    `json:"lastChange"`
		Copied     int64    `json:"copied"`
		Deleted    int64    `json:"deleted"`
		Conflicts  int64    `json:"conflicts"`
		Failed     int64    `json:"failed"`
	}
	var rules []ruleOut
	for _, s := range w.en.Statuses() {
		rules = append(rules, ruleOut{s.Name, s.Desc, s.Status, s.Mode, s.LastErr, s.Paths, s.Enabled, s.Paused,
			ms(s.LastRun), ms(s.LastChange), s.Copied, s.Deleted, s.Conflicts, s.Failed})
	}
	if rules == nil {
		rules = []ruleOut{}
	}
	writeJSON(rw, 200, map[string]any{"version": w.version, "now": time.Now().UnixMilli(), "rules": rules})
}

func (w *WebServer) apiRuleAction(rw http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Action string }
	var raw struct {
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if !readJSON(rw, r, &raw) {
		return
	}
	in.Name, in.Action = raw.Name, raw.Action
	found := false
	for _, s := range w.en.Statuses() {
		if s.Name == in.Name {
			found = true
		}
	}
	if !found {
		jerr(rw, 404, "правило не найдено или не запущено: %s", in.Name)
		return
	}
	switch in.Action {
	case "sync":
		w.en.Trigger(in.Name)
	case "pause":
		w.en.SetPaused(in.Name, true)
	case "resume":
		w.en.SetPaused(in.Name, false)
	default:
		jerr(rw, 400, "неизвестное действие %q", in.Action)
		return
	}
	writeJSON(rw, 200, map[string]any{"ok": true})
}

func (w *WebServer) apiAllAction(rw http.ResponseWriter, r *http.Request) {
	var raw struct {
		Action string `json:"action"`
	}
	if !readJSON(rw, r, &raw) {
		return
	}
	switch raw.Action {
	case "sync":
		w.en.TriggerAll()
	case "pause":
		w.en.PauseAll(true)
	case "resume":
		w.en.PauseAll(false)
	default:
		jerr(rw, 400, "неизвестное действие %q", raw.Action)
		return
	}
	writeJSON(rw, 200, map[string]any{"ok": true})
}

func levelRank(l string) int {
	switch l {
	case "ERROR":
		return 3
	case "WARN":
		return 2
	}
	return 1
}

func (w *WebServer) apiLog(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxEvents {
		limit = 300
	}
	min := levelRank(strings.ToUpper(q.Get("level")))
	rule := q.Get("rule")
	type ev struct {
		T     int64  `json:"t"`
		Rule  string `json:"rule"`
		Level string `json:"level"`
		Msg   string `json:"msg"`
	}
	all := w.en.Events()
	var out []ev
	for _, e := range all {
		if levelRank(e.Level) < min || (rule != "" && e.Rule != rule) {
			continue
		}
		out = append(out, ev{e.Time.UnixMilli(), e.Rule, e.Level, e.Msg})
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []ev{}
	}
	writeJSON(rw, 200, out)
}

func (w *WebServer) apiLogFile(rw http.ResponseWriter, r *http.Request) {
	kb, _ := strconv.Atoi(r.URL.Query().Get("kb"))
	if kb <= 0 || kb > 2048 {
		kb = 128
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	f, err := os.Open(w.logPath)
	if err != nil {
		rw.WriteHeader(404)
		fmt.Fprintf(rw, "файл журнала недоступен: %v", err)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	off := fi.Size() - int64(kb)*1024
	if off < 0 {
		off = 0
	}
	_, _ = f.Seek(off, io.SeekStart)
	data, _ := io.ReadAll(f)
	if off > 0 { // отбросить обрезанную первую строку
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	_, _ = rw.Write(data)
}

func (w *WebServer) apiFS(rw http.ResponseWriter, r *http.Request) {
	resp, err := fsList(r.URL.Query().Get("path"))
	if err != nil {
		jerr(rw, 400, "%v", err)
		return
	}
	if resp.Entries == nil {
		resp.Entries = []fsEntry{}
	}
	writeJSON(rw, 200, resp)
}

// ---------- конфигурация ----------

type apiRuleCfg struct {
	Name  string   `json:"name"`
	Desc  string   `json:"desc"`
	Opts  []KV     `json:"opts"`
	Paths []string `json:"paths"`
	Err   string   `json:"err,omitempty"`
}

type apiConfig struct {
	Rev      string       `json:"rev"`
	Settings []KV         `json:"settings"`
	Rules    []apiRuleCfg `json:"rules"`
	Warnings []string     `json:"warnings,omitempty"`
}

func revOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

func (w *WebServer) apiConfigGet(rw http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(w.iniPath)
	if err != nil {
		jerr(rw, 500, "не удалось прочитать конфиг: %v", err)
		return
	}
	cfg, err := ParseConfigText(string(data))
	if err != nil {
		jerr(rw, 500, "конфиг не разобран: %v", err)
		return
	}
	cfg.Validate()
	out := apiConfig{Rev: revOf(data), Settings: []KV{}, Rules: []apiRuleCfg{}, Warnings: cfg.Warnings}
	for _, k := range cfg.SettingsOrder {
		out.Settings = append(out.Settings, KV{k, cfg.Settings[k]})
	}
	for _, ru := range cfg.Rules {
		ar := apiRuleCfg{Name: ru.Name, Desc: ru.Desc, Opts: ru.Explicit, Paths: []string{}, Err: ru.Err}
		if ar.Opts == nil {
			ar.Opts = []KV{}
		}
		for _, e := range ru.Endpoints {
			ar.Paths = append(ar.Paths, e.Raw)
		}
		out.Rules = append(out.Rules, ar)
	}
	writeJSON(rw, 200, out)
}

var optKeyRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

func noNewline(s string) bool { return !strings.ContainsAny(s, "\r\n") }

func (w *WebServer) apiConfigSave(rw http.ResponseWriter, r *http.Request) {
	var in apiConfig
	if !readJSON(rw, r, &in) {
		return
	}
	cur, err := os.ReadFile(w.iniPath)
	if err != nil {
		jerr(rw, 500, "не удалось прочитать конфиг: %v", err)
		return
	}
	if in.Rev != "" && in.Rev != revOf(cur) {
		jerr(rw, 409, "конфиг был изменён (в другом окне или вручную). Обновите страницу и повторите правку")
		return
	}
	old, _ := ParseConfigText(string(cur))
	sk := &Config{Settings: map[string]string{}}
	if old != nil {
		sk.Header = old.Header
	}
	var errs []string
	for _, kv := range in.Settings {
		k := strings.ToLower(strings.TrimSpace(kv.K))
		if k == "" {
			continue
		}
		if !optKeyRe.MatchString(k) || !noNewline(kv.V) {
			errs = append(errs, fmt.Sprintf("настройка %q: недопустимый ключ или значение", kv.K))
			continue
		}
		if _, dup := sk.Settings[k]; !dup {
			sk.SettingsOrder = append(sk.SettingsOrder, k)
		}
		sk.Settings[k] = strings.TrimSpace(kv.V)
	}
	seen := map[string]bool{}
	for _, ar := range in.Rules {
		name := strings.TrimSpace(ar.Name)
		switch {
		case name == "":
			errs = append(errs, "у правила пустое название")
			continue
		case len(name) > 100 || strings.ContainsAny(name, "[]\r\n"):
			errs = append(errs, fmt.Sprintf("правило %q: название не должно содержать [ ] и переводов строк (до 100 символов)", name))
			continue
		case strings.EqualFold(name, "settings"):
			errs = append(errs, "название «Settings» зарезервировано")
			continue
		case seen[strings.ToLower(name)]:
			errs = append(errs, fmt.Sprintf("название правила повторяется: %s", name))
			continue
		}
		seen[strings.ToLower(name)] = true
		ru := &Rule{Name: name, Desc: strings.TrimSpace(ar.Desc)}
		if !noNewline(ru.Desc) {
			errs = append(errs, fmt.Sprintf("[%s]: аннотация не должна содержать переводов строк", name))
			continue
		}
		for _, kv := range ar.Opts {
			k := strings.ToLower(strings.TrimSpace(kv.K))
			if !optKeyRe.MatchString(k) || !noNewline(kv.V) {
				errs = append(errs, fmt.Sprintf("[%s]: недопустимая опция %q", name, kv.K))
				continue
			}
			ru.record(k, strings.TrimSpace(kv.V))
		}
		for _, p := range ar.Paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !noNewline(p) || !pathLineRe.MatchString(p) {
				errs = append(errs, fmt.Sprintf("[%s]: путь %q должен быть полным (C:\\..., \\\\сервер\\... или /...)", name, p))
				continue
			}
			ru.Endpoints = append(ru.Endpoints, &Endpoint{Raw: p})
		}
		sk.Rules = append(sk.Rules, ru)
	}
	if len(errs) > 0 {
		writeJSON(rw, 400, map[string]any{"error": "конфиг не сохранён", "errors": errs})
		return
	}
	text := sk.Render()
	cfg2, err := ParseConfigText(text)
	if err != nil {
		jerr(rw, 400, "конфиг не разобран: %v", err)
		return
	}
	cfg2.Validate()
	for _, ru := range cfg2.Rules {
		if ru.Err != "" {
			errs = append(errs, fmt.Sprintf("[%s]: %s", ru.Name, ru.Err))
		}
	}
	if _, werrs := parseWebCfg(cfg2.Settings); len(werrs) > 0 {
		errs = append(errs, werrs...)
	}
	if len(errs) > 0 {
		writeJSON(rw, 400, map[string]any{"error": "конфиг не сохранён", "errors": errs})
		return
	}
	if runtime.GOOS == "windows" || strings.Contains(string(cur), "\r\n") {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	_ = os.WriteFile(w.iniPath+".bak", cur, 0o644)
	tmp := w.iniPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		jerr(rw, 500, "не удалось записать конфиг: %v", err)
		return
	}
	if err := os.Rename(tmp, w.iniPath); err != nil {
		jerr(rw, 500, "не удалось заменить конфиг: %v", err)
		return
	}
	w.en.logf("web", "INFO", "конфиг сохранён через веб-интерфейс (правил: %d), копия прежнего — %s.bak", len(cfg2.Rules), w.iniPath)
	if w.reload != nil {
		go func() {
			time.Sleep(300 * time.Millisecond) // дать ответу уйти: возможна смена портов веб-интерфейса
			w.reload()
		}()
	}
	writeJSON(rw, 200, map[string]any{"ok": true, "rev": revOf([]byte(text)), "warnings": cfg2.Warnings})
}

func (w *WebServer) apiCredentials(rw http.ResponseWriter, r *http.Request, open bool) {
	var in struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if !readJSON(rw, r, &in) {
		return
	}
	if err := w.creds.Set(in.User, in.Password); err != nil {
		jerr(rw, 400, "%v", err)
		return
	}
	w.en.logf("web", "INFO", "логин и пароль веб-интерфейса изменены (пользователь %s)", strings.TrimSpace(in.User))
	if !open {
		w.setCookie(rw, w.creds.User()) // текущий сеанс продолжает работать
	}
	writeJSON(rw, 200, map[string]any{"ok": true})
}
