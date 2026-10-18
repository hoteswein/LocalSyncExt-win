package main

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Строка-путь: "C:\..." или "C:/..." или UNC "\\..." или "/..." (для тестов на Linux).
var pathLineRe = regexp.MustCompile(`^([A-Za-z]:[\\/]|\\\\|/)`)

// Игнорируется всегда (имена в нижнем регистре, маски path.Match).
var defaultIgnore = []string{
	"$recycle.bin", "system volume information", "desktop.ini", "thumbs.db",
	"~$*", "*.tmp", ".lsx-*", ".localsynctrash",
}

type Endpoint struct {
	Raw  string // как записано в конфиге
	Dir  string // папка (для файла — родительская)
	Mask string // маска файлов, если путь вида C:\Docs\*.pdf
	File string // имя файла, если правило в файловом режиме
}

type Rule struct {
	Name         string
	Mode         string // twoway | oneway
	Master       int    // для oneway, индекс (с 0)
	Conflict     string // auto | ask
	Notify       bool
	Ignore       []string
	TrashDays    int // -1 без корзины, 0 бессрочно, N дней
	Scan         int // секунд, 0 — выключено
	Recursive    bool
	MaxDeletePct int
	DeleteExtra  bool
	Enabled      bool
	Marker       bool         // маркер-файл в папках (по умолчанию да)
	NoMarker     map[int]bool // пути (с 0), для которых маркер отключён
	Watch        bool         // мгновенное слежение за изменениями
	ScanSet      bool         // scan задан явно
	Debounce     int          // мс, пауза накопления событий
	TrashPlace   string       // folder | work | абсолютный путь ("" — взять из [Settings])
	TempMode     string       // prefix | work ("" — из [Settings])
	DirectKB     int          // файлы не больше этого размера пишутся сразу, без временного файла (-1 — из [Settings])
	workDir      string
	Desc         string // аннотация (ключ description)
	Explicit     []KV   // явно заданные опции в порядке файла
	Endpoints    []*Endpoint

	Masks    []string
	FileMode bool
	Err      string
}

// KV — пара «ключ=значение» с сохранением порядка.
type KV struct {
	K string `json:"k"`
	V string `json:"v"`
}

type Config struct {
	WorkDir       string
	Header        []string // комментарии в начале файла
	SettingsOrder []string
	Settings      map[string]string
	Rules         []*Rule
	Warnings      []string
}

func newRule(name string) *Rule {
	return &Rule{
		Name: name, Mode: "twoway", Conflict: "auto", Notify: true,
		TrashDays: 30, Scan: 60, Recursive: true, MaxDeletePct: 50,
		DeleteExtra: true, Enabled: true, Marker: true, Watch: true, Debounce: 1500,
		NoMarker: map[int]bool{}, DirectKB: -1,
	}
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on", "да":
		return true, true
	case "0", "false", "no", "off", "нет":
		return false, true
	}
	return false, false
}

func (r *Rule) fail(format string, a ...any) {
	if r.Err == "" {
		r.Err = fmt.Sprintf(format, a...)
	}
}

func (r *Rule) setOption(k, v string, cfg *Config) {
	switch k {
	case "mode":
		v = strings.ToLower(v)
		if v != "twoway" && v != "oneway" {
			r.fail("mode=%q: допустимо twoway или oneway", v)
			return
		}
		r.Mode = v
	case "master":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			r.fail("master=%q: нужен номер пути в правиле, начиная с 1", v)
			return
		}
		r.Master = n - 1
	case "conflict":
		v = strings.ToLower(v)
		if v != "auto" && v != "ask" {
			r.fail("conflict=%q: допустимо auto или ask", v)
			return
		}
		r.Conflict = v
	case "notify", "recursive", "enabled", "deleteextra", "marker", "watch":
		b, ok := parseBool(v)
		if !ok {
			r.fail("%s=%q: нужно 1/0", k, v)
			return
		}
		switch k {
		case "notify":
			r.Notify = b
		case "recursive":
			r.Recursive = b
		case "enabled":
			r.Enabled = b
		case "deleteextra":
			r.DeleteExtra = b
		case "marker":
			r.Marker = b
		case "watch":
			r.Watch = b
		}
	case "nomarker":
		for _, p := range strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ';' || c == ' ' }) {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 {
				r.fail("nomarker=%q: нужны номера путей правила, начиная с 1 (например 2,3)", v)
				return
			}
			r.NoMarker[n-1] = true
		}
	case "trash":
		if v != "folder" && v != "work" && !pathLineRe.MatchString(v) {
			r.fail("trash=%q: допустимо folder, work или полный путь", v)
			return
		}
		r.TrashPlace = v
	case "temp":
		v = strings.ToLower(v)
		if v != "prefix" && v != "work" {
			r.fail("temp=%q: допустимо prefix или work", v)
			return
		}
		r.TempMode = v
	case "directbelow":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			r.fail("directbelow=%q: нужно число КБ >= 0 (0 — всегда через временный файл)", v)
			return
		}
		r.DirectKB = n
	case "description", "desc":
		r.Desc = v
	case "debounce":
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 60000 {
			r.fail("debounce=%q: нужно число миллисекунд 100..60000", v)
			return
		}
		r.Debounce = n
	case "ignore":
		for _, p := range strings.Split(v, ";") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				r.Ignore = append(r.Ignore, p)
			}
		}
	case "trashdays":
		if v == "" {
			r.TrashDays = -1
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			r.fail("TrashDays=%q: нужно число >= 0 или пусто", v)
			return
		}
		r.TrashDays = n
	case "scan":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			r.fail("scan=%q: нужно число секунд >= 0", v)
			return
		}
		r.Scan, r.ScanSet = n, true
	case "maxdeletepercent":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			r.fail("MaxDeletePercent=%q: нужно 1..100", v)
			return
		}
		r.MaxDeletePct = n
	default:
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("[%s] неизвестная опция %q", r.Name, k))
	}
}

func ParseConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfigText(string(data))
}

// ParseConfigText разбирает текст конфигурации (см. ParseConfig).
func ParseConfigText(text string) (*Config, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	cfg := &Config{Settings: map[string]string{}}
	var cur *Rule
	inSettings := false
	started := false // встретилась ли первая секция или строка настройки
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ln := 0
	warn := func(f string, a ...any) {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("строка %d: ", ln)+fmt.Sprintf(f, a...))
	}
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == ';' || line[0] == '#' {
			if !started && line != "" {
				cfg.Header = append(cfg.Header, line)
			}
			continue
		}
		started = true
		if line[0] == '[' && line[len(line)-1] == ']' {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if strings.EqualFold(name, "settings") {
				inSettings, cur = true, nil
				continue
			}
			inSettings = false
			cur = newRule(name)
			cfg.Rules = append(cfg.Rules, cur)
			continue
		}
		if pathLineRe.MatchString(line) {
			if cur == nil {
				warn("путь вне правила: %s", line)
				continue
			}
			cur.Endpoints = append(cur.Endpoints, &Endpoint{Raw: line})
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			warn("непонятная строка: %s", line)
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if inSettings {
			if _, dup := cfg.Settings[k]; !dup {
				cfg.SettingsOrder = append(cfg.SettingsOrder, k)
			}
			cfg.Settings[k] = v
			continue
		}
		if cur == nil {
			warn("опция вне правила: %s", line)
			continue
		}
		if k != "description" && k != "desc" {
			cur.record(k, v)
		}
		cur.setOption(k, v, cfg)
	}
	cfg.applyDefaults()
	for _, r := range cfg.Rules {
		if !r.ScanSet {
			// при наблюдателе полное сканирование нужно лишь как страховка
			if r.Watch {
				r.Scan = 900
			} else {
				r.Scan = 60
			}
		}
	}
	return cfg, sc.Err()
}

// prepare определяет по диску: папки / маски / отдельные файлы. Вызывается перед каждым проходом.
func (r *Rule) prepare() {
	var masks []string
	for _, e := range r.Endpoints {
		p := filepath.Clean(e.Raw)
		e.Dir, e.Mask, e.File = p, "", ""
		if base := filepath.Base(p); strings.ContainsAny(base, "*?") {
			e.Mask, e.Dir = base, filepath.Dir(p)
			masks = append(masks, strings.ToLower(base))
		}
	}
	sort.Strings(masks)
	r.Masks = r.Masks[:0]
	for i, m := range masks {
		if i == 0 || m != masks[i-1] {
			r.Masks = append(r.Masks, m)
		}
	}
	r.FileMode = false
	if len(r.Masks) > 0 {
		return
	}
	files, dirs := 0, 0
	for _, e := range r.Endpoints {
		if fi, err := os.Stat(e.Dir); err == nil {
			if fi.IsDir() {
				dirs++
			} else {
				files++
			}
		}
	}
	if files > 0 && dirs > 0 {
		r.fail("в одном правиле смешаны файлы и папки (без маски так нельзя)")
		return
	}
	if files > 0 {
		r.FileMode = true
		for _, e := range r.Endpoints {
			e.File, e.Dir = filepath.Base(e.Dir), filepath.Dir(e.Dir)
		}
	}
}

func lowerClean(p string) string { return strings.ToLower(filepath.Clean(p)) }

func under(child, parent string) bool {
	sep := string(filepath.Separator)
	return strings.HasPrefix(child+sep, parent+sep)
}

func fullPath(e *Endpoint) string {
	if e.File != "" {
		return lowerClean(filepath.Join(e.Dir, e.File))
	}
	return lowerClean(e.Dir)
}

func epOverlap(a, b *Endpoint) bool {
	pa, pb := fullPath(a), fullPath(b)
	return under(pa, pb) || under(pb, pa)
}

func disjointMasks(a, b *Rule) bool {
	if len(a.Masks) == 0 || len(b.Masks) == 0 {
		return false
	}
	for _, x := range a.Masks {
		for _, y := range b.Masks {
			if x == y {
				return false
			}
		}
	}
	return true
}

// Validate проверяет правила: минимум 2 пути, пересечения, дубликаты имён.
// Некорректные правила получают Err и не запускаются; остальные работают.
func (c *Config) Validate() {
	seen := map[string]bool{}
	for _, r := range c.Rules {
		key := strings.ToLower(r.Name)
		if seen[key] {
			r.fail("повторяющееся имя правила")
		}
		seen[key] = true
		if len(r.Endpoints) < 2 {
			r.fail("нужно минимум 2 пути")
		}
		if r.Mode == "oneway" && r.Master >= len(r.Endpoints) {
			r.fail("master=%d, но путей только %d", r.Master+1, len(r.Endpoints))
		}
		r.checkWorkPlaces(c)
		for i := range r.NoMarker {
			if i >= len(r.Endpoints) {
				r.fail("nomarker: пути №%d нет в правиле", i+1)
			}
		}
		r.prepare()
	}
	for i, ra := range c.Rules {
		if !ra.Enabled || ra.Err != "" {
			continue
		}
		for ia, ea := range ra.Endpoints {
			for ib := ia + 1; ib < len(ra.Endpoints); ib++ {
				if epOverlap(ea, ra.Endpoints[ib]) {
					ra.fail("пути %d и %d совпадают или вложены друг в друга", ia+1, ib+1)
				}
			}
		}
		for _, rb := range c.Rules[i+1:] {
			if !rb.Enabled || rb.Err != "" || ra.Err != "" {
				continue
			}
			for _, ea := range ra.Endpoints {
				for _, eb := range rb.Endpoints {
					if !epOverlap(ea, eb) {
						continue
					}
					if disjointMasks(ra, rb) {
						c.Warnings = append(c.Warnings, fmt.Sprintf("правила [%s] и [%s] работают в одной папке, но с разными масками", ra.Name, rb.Name))
						continue
					}
					rb.fail("пересекается с правилом [%s] (%s)", ra.Name, ea.Raw)
				}
			}
		}
	}
}

func safeName(name string) string {
	s := regexp.MustCompile(`[^\p{L}\p{N}_-]+`).ReplaceAllString(name, "_")
	return fmt.Sprintf("%s-%08x", s, crc32.ChecksumIEEE([]byte(strings.ToLower(name))))
}

// applyDefaults подставляет значения из [Settings] и встроенные умолчания.
func (c *Config) applyDefaults() {
	if wd := c.Settings["workdir"]; wd != "" {
		c.WorkDir = wd
	} else {
		exe, _ := os.Executable()
		c.WorkDir = filepath.Join(filepath.Dir(exe), "work")
	}
	gTrash, gTemp, gDirect := "folder", "prefix", 1024
	if v := c.Settings["trash"]; v != "" {
		if v == "folder" || v == "work" || pathLineRe.MatchString(v) {
			gTrash = v
		} else {
			c.Warnings = append(c.Warnings, fmt.Sprintf("[Settings] trash=%q не понят, использую folder", v))
		}
	}
	if v := strings.ToLower(c.Settings["temp"]); v != "" {
		if v == "prefix" || v == "work" {
			gTemp = v
		} else {
			c.Warnings = append(c.Warnings, fmt.Sprintf("[Settings] temp=%q не понят, использую prefix", v))
		}
	}
	if v := c.Settings["directbelow"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			gDirect = n
		} else {
			c.Warnings = append(c.Warnings, fmt.Sprintf("[Settings] directbelow=%q не понят, использую %d", v, gDirect))
		}
	}
	for _, r := range c.Rules {
		r.workDir = c.WorkDir
		if r.TrashPlace == "" {
			r.TrashPlace = gTrash
		}
		if r.TempMode == "" {
			r.TempMode = gTemp
		}
		if r.DirectKB < 0 {
			r.DirectKB = gDirect
		}
	}
}

// trashRoot — где лежит корзина для пути №i (с 0).
func (r *Rule) trashRoot(i int) string {
	switch r.TrashPlace {
	case "folder", "":
		return filepath.Join(r.Endpoints[i].Dir, trashDir)
	case "work":
		return filepath.Join(r.workDir, "trash", safeName(r.Name), fmt.Sprintf("ep%d", i+1))
	}
	return filepath.Join(r.TrashPlace, safeName(r.Name), fmt.Sprintf("ep%d", i+1))
}

// checkWorkPlaces: корзина/временные файлы вне папок не должны попадать внутрь синхронизируемых путей.
func (r *Rule) checkWorkPlaces(c *Config) {
	var roots []string
	if r.TrashPlace != "folder" && r.TrashPlace != "" {
		if r.TrashPlace == "work" {
			roots = append(roots, r.workDir)
		} else {
			roots = append(roots, r.TrashPlace)
		}
	}
	if r.TempMode == "work" {
		roots = append(roots, r.workDir)
	}
	for _, root := range roots {
		lr := lowerClean(root)
		for _, other := range c.Rules {
			for _, e := range other.Endpoints {
				ep := lowerClean(filepath.Dir(e.Raw))
				if !strings.ContainsAny(filepath.Base(e.Raw), "*?") {
					ep = lowerClean(e.Raw)
				}
				if under(lr, ep) {
					r.fail("рабочая папка %s находится внутри синхронизируемого пути %s — задайте другой WorkDir или trash", root, e.Raw)
					return
				}
			}
		}
	}
}

func (r *Rule) useMarker(i int) bool { return r.Marker && !r.NoMarker[i] }

// sig — отпечаток настроек правила: по нему при перечитывании конфига
// понимаем, какие правила изменились и требуют перезапуска.
func (r *Rule) sig() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s|%s|%d|%s|%t|%q|%d|%d|%t|%d|%t|%t|%t|%v|%d", r.Name, r.Mode, r.Master, r.Conflict,
		r.Notify, r.Ignore, r.TrashDays, r.Scan, r.Recursive, r.MaxDeletePct, r.DeleteExtra,
		r.Marker, r.Watch, r.NoMarker, r.Debounce)
	fmt.Fprintf(&sb, "|%s|%s|%d|%s", r.TrashPlace, r.TempMode, r.DirectKB, r.workDir)
	for _, e := range r.Endpoints {
		sb.WriteString("|" + e.Raw)
	}
	return sb.String()
}

// record запоминает явно заданную опцию (для сохранения конфига из веб-интерфейса).
func (r *Rule) record(k, v string) {
	for i := range r.Explicit {
		if r.Explicit[i].K == k {
			r.Explicit[i].V = v
			return
		}
	}
	r.Explicit = append(r.Explicit, KV{k, v})
}

var canonKeys = map[string]string{
	"trashdays": "TrashDays", "maxdeletepercent": "MaxDeletePercent", "deleteextra": "DeleteExtra",
	"directbelow": "DirectBelow", "nomarker": "NoMarker", "trayclick": "TrayClick",
	"traydoubleclick": "TrayDoubleClick", "workdir": "WorkDir", "logfile": "LogFile",
	"statedir": "StateDir", "webui": "WebUI", "webportopen": "WebPortOpen", "webportauth": "WebPortAuth",
	"webopenbind": "WebOpenBind", "webauthbind": "WebAuthBind", "webhosts": "WebHosts",
	"notifications": "Notifications", "trash": "Trash", "temp": "Temp",
}

func canonKey(k string) string {
	if c, ok := canonKeys[k]; ok {
		return c
	}
	return k
}

// Render записывает конфигурацию обратно в текст ini (комментарии внутри правил не сохраняются).
func (c *Config) Render() string {
	var b strings.Builder
	for _, h := range c.Header {
		b.WriteString(h + "\n")
	}
	if len(c.Header) > 0 {
		b.WriteString("\n")
	}
	if len(c.SettingsOrder) > 0 {
		b.WriteString("[Settings]\n")
		for _, k := range c.SettingsOrder {
			fmt.Fprintf(&b, "%s=%s\n", canonKey(k), c.Settings[k])
		}
		b.WriteString("\n")
	}
	for _, r := range c.Rules {
		fmt.Fprintf(&b, "[%s]\n", r.Name)
		if r.Desc != "" {
			fmt.Fprintf(&b, "description=%s\n", r.Desc)
		}
		for _, kv := range r.Explicit {
			fmt.Fprintf(&b, "%s=%s\n", canonKey(kv.K), kv.V)
		}
		for _, e := range r.Endpoints {
			b.WriteString(e.Raw + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
