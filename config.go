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
	Endpoints    []*Endpoint

	Masks    []string
	FileMode bool
	Err      string
}

type Config struct {
	Settings map[string]string
	Rules    []*Rule
	Warnings []string
}

func newRule(name string) *Rule {
	return &Rule{
		Name: name, Mode: "twoway", Conflict: "auto", Notify: true,
		TrashDays: 30, Scan: 60, Recursive: true, MaxDeletePct: 50,
		DeleteExtra: true, Enabled: true,
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
	case "notify", "recursive", "enabled", "deleteextra":
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
		}
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
		r.Scan = n
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
	text := strings.TrimPrefix(string(data), "\ufeff")
	cfg := &Config{Settings: map[string]string{}}
	var cur *Rule
	inSettings := false
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
			continue
		}
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
			cfg.Settings[k] = v
			continue
		}
		if cur == nil {
			warn("опция вне правила: %s", line)
			continue
		}
		cur.setOption(k, v, cfg)
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
