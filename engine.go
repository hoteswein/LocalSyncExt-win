package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tolNS      = int64(2 * time.Second) // допуск времени (FAT/exFAT хранят с точностью 2 с)
	settle     = 2 * time.Second        // только что изменённый файл пропускаем до следующего прохода
	fileKey    = "<file>"
	trashDir   = ".LocalSyncTrash"
	maxEvents  = 300
	stateFlush = 5 * time.Second
)

var (
	errSourceChanged    = errors.New("файл изменился во время копирования")
	errWatchUnsupported = errors.New("слежение за папками не поддерживается на этой ОС")
)

type FInfo struct {
	Rel   string
	Size  int64
	MTime int64
}

// epCache — известное содержимое пути (в памяти), чтобы по событию проверять только изменённое.
type epCache struct {
	files map[string]FInfo  // ключ (нижний регистр, '/') -> файл
	dirs  map[string]string // ключ папки -> относительный путь
}

type rawEv struct {
	ep  int
	rel string
}

type Event struct {
	Time  time.Time
	Rule  string
	Level string
	Msg   string
}

type ruleRT struct {
	mu      sync.Mutex // один проход за раз
	trigger chan struct{}

	smu     sync.Mutex // поля статуса
	status  string
	lastErr string
	lastRun time.Time

	lastCleanup time.Time
	lastSave    time.Time
	notified    map[string]bool
	st          *State
	cache       []*epCache
	pend        map[string]string // ключи для повторной проверки (файл ещё пишется / ошибка)
}

func newRT() *ruleRT {
	return &ruleRT{trigger: make(chan struct{}, 1), status: "idle", notified: map[string]bool{}, pend: map[string]string{}}
}

func (rt *ruleRT) setStatus(status, errMsg string) {
	rt.smu.Lock()
	rt.status, rt.lastErr = status, errMsg
	if status != "syncing" {
		rt.lastRun = time.Now()
	}
	rt.smu.Unlock()
}

type RuleStatus struct {
	Name, Status, LastErr string
	LastRun               time.Time
}

type running struct {
	sig    string
	cancel context.CancelFunc
	done   chan struct{}
}

type Engine struct {
	stateDir string
	dry      bool
	out      *log.Logger
	evMu     sync.Mutex
	events   []Event
	// OnNotify вызывается для уведомлений (трей подключим позже).
	OnNotify func(rule, title, msg string)

	mu  sync.Mutex
	ctx context.Context
	cfg *Config
	rt  map[string]*ruleRT
	run map[string]*running
}

func NewEngine(cfg *Config, stateDir string, dry bool, w io.Writer) *Engine {
	return &Engine{cfg: cfg, stateDir: stateDir, dry: dry,
		out: log.New(w, "", log.Ldate|log.Ltime), rt: map[string]*ruleRT{}, run: map[string]*running{}}
}

func (en *Engine) logf(rule, level, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	en.out.Printf("[%s] %s: %s", rule, level, msg)
	en.evMu.Lock()
	en.events = append(en.events, Event{time.Now(), rule, level, msg})
	if len(en.events) > maxEvents {
		en.events = en.events[len(en.events)-maxEvents:]
	}
	en.evMu.Unlock()
}

func (en *Engine) Events() []Event {
	en.evMu.Lock()
	defer en.evMu.Unlock()
	return append([]Event(nil), en.events...)
}

func (en *Engine) rtFor(r *Rule) *ruleRT {
	en.mu.Lock()
	defer en.mu.Unlock()
	rt := en.rt[r.Name]
	if rt == nil {
		rt = newRT()
		en.rt[r.Name] = rt
	}
	return rt
}

func (en *Engine) Statuses() []RuleStatus {
	en.mu.Lock()
	defer en.mu.Unlock()
	var out []RuleStatus
	for _, r := range en.cfg.Rules {
		st, msg, last := "idle", "", time.Time{}
		if rt := en.rt[r.Name]; rt != nil {
			rt.smu.Lock()
			st, msg, last = rt.status, rt.lastErr, rt.lastRun
			rt.smu.Unlock()
		}
		if r.Err != "" {
			st, msg = "config-error", r.Err
		} else if !r.Enabled {
			st = "disabled"
		}
		out = append(out, RuleStatus{r.Name, st, msg, last})
	}
	return out
}

// Trigger — «синхронизировать сейчас» (полное сканирование).
func (en *Engine) Trigger(rule string) {
	en.mu.Lock()
	rt := en.rt[rule]
	en.mu.Unlock()
	if rt != nil {
		select {
		case rt.trigger <- struct{}{}:
		default:
		}
	}
}

func (en *Engine) notify(r *Rule, title, msg string) {
	if r.Notify && en.OnNotify != nil {
		en.OnNotify(r.Name, title, msg)
	}
}

func (en *Engine) statePath(r *Rule) string {
	return filepath.Join(en.stateDir, safeName(r.Name)+".json")
}

func markerName(rule string) string { return ".lsx-" + safeName(rule) + ".marker" }

// report пишет в журнал предупреждения и ошибки конфигурации; true, если есть ошибки правил.
func (en *Engine) report(cfg *Config) bool {
	bad := false
	for _, s := range cfg.Warnings {
		en.logf("config", "WARN", "%s", s)
	}
	for _, r := range cfg.Rules {
		if r.Err != "" {
			en.logf(r.Name, "ERROR", "ошибка конфигурации, правило не запущено: %s", r.Err)
			bad = true
		}
	}
	return bad
}

// ---------- запуск ----------

// RunOnce — один полный проход по всем правилам. true, если без ошибок.
func (en *Engine) RunOnce() bool {
	ok := true
	en.mu.Lock()
	rules := en.cfg.Rules
	en.mu.Unlock()
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if r.Err != "" {
			en.logf(r.Name, "ERROR", "правило отключено из-за ошибки конфигурации: %s", r.Err)
			ok = false
			continue
		}
		if err := en.SyncRule(r); err != nil {
			ok = false
		}
	}
	return ok
}

// SyncRule — один полный проход по правилу с немедленным сохранением состояния.
func (en *Engine) SyncRule(r *Rule) error {
	_, err := en.pass(r, en.rtFor(r), true, nil, true)
	return err
}

// Start запускает фоновую работу всех правил.
func (en *Engine) Start(ctx context.Context) {
	en.mu.Lock()
	en.ctx = ctx
	cfg := en.cfg
	en.mu.Unlock()
	en.Apply(cfg)
}

// Stop останавливает все правила и ждёт сохранения состояния.
func (en *Engine) Stop() {
	en.mu.Lock()
	defer en.mu.Unlock()
	for name, ru := range en.run {
		ru.cancel()
		<-ru.done
		delete(en.run, name)
	}
}

// Apply применяет новую конфигурацию «на лету»: неизменённые правила работают без перерыва,
// изменённые перезапускаются, удалённые останавливаются, новые запускаются.
func (en *Engine) Apply(cfg *Config) {
	en.mu.Lock()
	defer en.mu.Unlock()
	en.cfg = cfg
	want := map[string]*Rule{}
	for _, r := range cfg.Rules {
		if r.Enabled && r.Err == "" {
			want[r.Name] = r
		}
	}
	for name, ru := range en.run {
		if nr, ok := want[name]; ok && nr.sig() == ru.sig {
			continue
		}
		for _, nr := range cfg.Rules {
			if nr.Name == name && nr.Enabled && nr.Err != "" {
				// Опечатка при правке не должна останавливать работающее правило.
				en.logf(name, "WARN", "в новой версии правила ошибка, продолжает работать прежняя версия")
				goto keep
			}
		}
		ru.cancel()
		<-ru.done
		delete(en.run, name)
		delete(en.rt, name)
		en.logf(name, "INFO", "правило остановлено (изменено, отключено или удалено)")
	keep:
	}
	if en.ctx == nil {
		return
	}
	for _, r := range cfg.Rules {
		if _, ok := en.run[r.Name]; ok || !r.Enabled || r.Err != "" {
			continue
		}
		rt := newRT()
		en.rt[r.Name] = rt
		ctx, cancel := context.WithCancel(en.ctx)
		ru := &running{sig: r.sig(), cancel: cancel, done: make(chan struct{})}
		en.run[r.Name] = ru
		go func(r *Rule) {
			defer close(ru.done)
			en.ruleLoop(ctx, r, rt)
		}(r)
		en.logf(r.Name, "INFO", "правило запущено (путей: %d, режим %s, слежение %v)", len(r.Endpoints), r.Mode, r.Watch)
	}
}

var cfgPoll = 2 * time.Second

// WatchConfig следит за файлом конфигурации и применяет изменения без перезапуска программы.
// При ошибке в новом файле остаются прежние настройки.
func (en *Engine) WatchConfig(ctx context.Context, file string, load func(string) (*Config, error)) {
	var lastMod time.Time
	var lastSize int64 = -1
	if fi, err := os.Stat(file); err == nil {
		lastMod, lastSize = fi.ModTime(), fi.Size()
	}
	t := time.NewTicker(cfgPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fi, err := os.Stat(file)
		if err != nil || (fi.ModTime().Equal(lastMod) && fi.Size() == lastSize) {
			continue
		}
		select { // подождать, пока редактор допишет файл
		case <-ctx.Done():
			return
		case <-time.After(cfgPoll / 4):
		}
		if fi, err = os.Stat(file); err != nil {
			continue
		}
		lastMod, lastSize = fi.ModTime(), fi.Size()
		cfg, err := load(file)
		if err != nil {
			en.logf("config", "ERROR", "не удалось перечитать конфиг, остаются прежние настройки: %v", err)
			continue
		}
		en.logf("config", "INFO", "конфиг изменён, применяю настройки")
		en.report(cfg)
		en.Apply(cfg)
	}
}

// ---------- цикл правила ----------

func (en *Engine) startWatchers(ctx context.Context, r *Rule, emit func(ep int, rel string, overflow bool), bad *atomic.Bool) {
	for i, e := range r.Endpoints {
		go func(i int, dir string) {
			attempts := 0
			for ctx.Err() == nil {
				first := attempts == 0
				attempts++
				err := watchDir(ctx, dir, r.Recursive && !r.FileMode, func() {
					if !first { // после перезапуска наблюдателя возможна потеря событий
						emit(i, "", true)
					}
				}, func(rel string, overflow bool) { emit(i, rel, overflow) })
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, errWatchUnsupported) {
					bad.Store(true)
					return
				}
				bad.Store(true)
				wait := 15 * time.Second
				if attempts <= 5 { // папка могла ещё не существовать при первом запуске
					wait = time.Second
				} else if attempts == 6 {
					en.logf(r.Name, "WARN", "слежение за %s не работает: %v; работаю по таймеру, повтор каждые 15 с", dir, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}(i, e.Dir)
	}
}

func (en *Engine) ruleLoop(ctx context.Context, r *Rule, rt *ruleRT) {
	evCh := make(chan rawEv, 4096)
	var lost, watchBad atomic.Bool
	if r.Watch {
		en.startWatchers(ctx, r, func(ep int, rel string, overflow bool) {
			if overflow {
				lost.Store(true)
				return
			}
			select {
			case evCh <- rawEv{ep, rel}:
			default:
				lost.Store(true)
			}
		}, &watchBad)
	}
	defer func() {
		rt.mu.Lock()
		en.flushState(r, rt, true)
		rt.mu.Unlock()
	}()

	debounce := time.Duration(r.Debounce) * time.Millisecond
	needFull := true
	nextFull := time.Now()
	var raws []rawEv
	var retryAt time.Time
	run := true

	for {
		if lost.Swap(false) {
			needFull, run = true, true
		}
		if run {
			batch := raws
			raws = nil
			wasFull := needFull
			retry, err := en.pass(r, rt, needFull, batch, false)
			needFull, run = false, false
			if wasFull {
				nextFull = time.Now().Add(en.scanEvery(r, &watchBad))
			}
			if err != nil && retry <= 0 {
				retry = 30 * time.Second
			}
			retryAt = time.Time{}
			if retry > 0 {
				retryAt = time.Now().Add(retry)
			}
		}

		var timers []*time.Timer
		mk := func(d time.Duration) <-chan time.Time {
			if d < 0 {
				d = 0
			}
			t := time.NewTimer(d)
			timers = append(timers, t)
			return t.C
		}
		var debC, fullC, retryC, flushC <-chan time.Time
		if r.Scan > 0 || watchBad.Load() {
			fullC = mk(time.Until(nextFull))
		}
		if !retryAt.IsZero() {
			retryC = mk(time.Until(retryAt))
		}
		if rt.st != nil && rt.st.dirty {
			flushC = mk(stateFlush)
		}
		stopAll := func() {
			for _, t := range timers {
				t.Stop()
			}
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				stopAll()
				return
			case ev := <-evCh:
				raws = append(raws, ev)
				if debC == nil {
					debC = mk(debounce)
				}
			case <-debC:
				run = true
				break wait
			case <-fullC:
				needFull, run = true, true
				break wait
			case <-retryC:
				run = true
				break wait
			case <-rt.trigger:
				needFull, run = true, true
				break wait
			case <-flushC:
				flushC = nil
				rt.mu.Lock()
				en.flushState(r, rt, true)
				rt.mu.Unlock()
			}
		}
		stopAll()
	}
}

func (en *Engine) scanEvery(r *Rule, watchBad *atomic.Bool) time.Duration {
	s := r.Scan
	if watchBad.Load() && (s == 0 || s > 60) {
		s = 60
	}
	if s <= 0 {
		s = 24 * 3600
	}
	return time.Duration(s) * time.Second
}

func (en *Engine) flushState(r *Rule, rt *ruleRT, force bool) {
	if rt.st == nil || !rt.st.dirty || en.dry {
		return
	}
	if !force && time.Since(rt.lastSave) < stateFlush {
		return
	}
	if err := rt.st.save(en.statePath(r)); err != nil {
		en.logf(r.Name, "ERROR", "не удалось сохранить состояние: %v", err)
		return
	}
	rt.lastSave = time.Now()
}

// ---------- проход по правилу ----------

type copyTo struct {
	dst    int
	rename bool // существующий файл назначения — проигравший в конфликте: сначала переименовать
}

type job struct {
	key         string
	rel         string
	winner      int // -1 — удаление
	copies      []copyTo
	touch       []int
	deletes     []int
	noBase      bool
	askConflict bool
}

type passStats struct{ copied, deleted, conflicts, failed, pending int }

// pass: full — полное сканирование; иначе обрабатываются только raws (события) и rt.pend.
// Возвращает, через сколько повторить (0 — не нужно).
func (en *Engine) pass(r *Rule, rt *ruleRT, full bool, raws []rawEv, force bool) (retry time.Duration, err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.setStatus("syncing", "")
	started := time.Now()
	defer func() {
		en.flushState(r, rt, force || err != nil)
		if err != nil {
			rt.setStatus("error", err.Error())
			en.logf(r.Name, "ERROR", "%v", err)
			en.notify(r, "Ошибка синхронизации", err.Error())
		} else {
			rt.setStatus("ok", "")
		}
	}()

	r.prepare()
	if r.Err != "" {
		return 0, errors.New(r.Err)
	}
	if rt.st == nil {
		st, lerr := loadState(en.statePath(r))
		if lerr != nil {
			return 0, lerr
		}
		rt.st = st
	}
	st := rt.st

	joining, err := en.checkEndpoints(r, st)
	if err != nil {
		rt.cache = nil
		return 0, err
	}
	n := len(r.Endpoints)
	var only map[string]string
	if full || rt.cache == nil {
		cache := make([]*epCache, n)
		for i, e := range r.Endpoints {
			c, serr := scanEndpoint(r, e)
			if serr != nil {
				rt.cache = nil
				return 0, fmt.Errorf("сканирование %s: %w", e.Raw, serr)
			}
			cache[i] = c
		}
		rt.cache = cache
	} else {
		only = map[string]string{}
		for k, v := range rt.pend {
			only[k] = v
		}
		if err = en.resolveEvents(r, rt, raws, only); err == nil {
			err = en.refreshKeys(r, rt, only)
		}
		if err != nil {
			rt.cache = nil
			return 0, err
		}
		if len(only) == 0 {
			rt.pend = map[string]string{}
			return 0, nil
		}
	}

	now := time.Now()
	scans := make([]map[string]FInfo, n)
	for i := range scans {
		scans[i] = rt.cache[i].files
	}
	var jobs []*job
	var hot map[string]string
	total := 0
	if r.Mode == "oneway" {
		jobs, hot = en.planOneWay(r, scans, only, now)
		for _, m := range scans {
			if len(m) > total {
				total = len(m)
			}
		}
	} else {
		jobs, hot = en.planTwoWay(r, st, scans, joining, only, now)
		total = len(st.Base)
	}

	dels := 0
	for _, j := range jobs {
		if j.winner < 0 && len(j.deletes) > 0 {
			dels++
		}
	}
	if dels >= 5 && dels*100 > r.MaxDeletePct*total {
		rt.cache = nil
		return 0, fmt.Errorf("защита от массового удаления: планируется удалить %d из %d файлов (лимит %d%%). Проверьте диски; если это нормально — увеличьте MaxDeletePercent в правиле", dels, total, r.MaxDeletePct)
	}

	var ps passStats
	failed := map[string]string{}
	touched := map[string]string{}
	for _, j := range jobs {
		touched[j.key] = j.rel
		if !en.execJob(r, rt, st, scans, j, &ps) {
			failed[j.key] = j.rel
		}
	}
	if len(touched) > 0 && !en.dry {
		if rerr := en.refreshKeys(r, rt, touched); rerr != nil {
			rt.cache = nil
		}
	}
	rt.pend = map[string]string{}
	for k, v := range hot {
		rt.pend[k] = v
	}
	for k, v := range failed {
		rt.pend[k] = v
	}
	if !en.dry && time.Since(rt.lastCleanup) > time.Hour {
		if r.TrashDays > 0 {
			en.cleanTrash(r)
		}
		cleanWorkTmp(r)
		rt.lastCleanup = time.Now()
	}
	if len(jobs) > 0 || ps.failed > 0 {
		kind := "полный"
		if only != nil {
			kind = fmt.Sprintf("по событиям (%d)", len(only))
		}
		en.logf(r.Name, "INFO", "проход %s: копий %d, удалений %d, конфликтов %d, ошибок %d, отложено %d, за %v", kind, ps.copied, ps.deleted, ps.conflicts, ps.failed, ps.pending, time.Since(started).Round(time.Millisecond))
	}
	switch {
	case len(failed) > 0:
		retry = 30 * time.Second
	case len(hot) > 0:
		retry = settle + time.Second
	}
	if ps.failed > 0 {
		return retry, fmt.Errorf("ошибок при выполнении операций: %d (см. журнал)", ps.failed)
	}
	return retry, nil
}

// checkEndpoints: доступность путей, серийные номера томов, маркеры.
// Возвращает, какие пути «присоединяются» к правилу с историей (их пустота — не удаление).
func (en *Engine) checkEndpoints(r *Rule, st *State) ([]bool, error) {
	joining := make([]bool, len(r.Endpoints))
	for i, e := range r.Endpoints {
		key := strings.ToLower(e.Raw)
		id, known := st.IDs[key]
		joining[i] = !known && len(st.Base) > 0
		fi, err := os.Stat(e.Dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: %w", e.Dir, err)
			}
			if known {
				return nil, fmt.Errorf("%s недоступен (диск отключён или папка удалена) — синхронизация правила приостановлена, ничего не удалено", e.Dir)
			}
			if en.dry {
				continue
			}
			if err := os.MkdirAll(e.Dir, 0o755); err != nil {
				return nil, fmt.Errorf("не удалось создать %s: %w", e.Dir, err)
			}
		} else if !fi.IsDir() {
			return nil, fmt.Errorf("%s — не папка", e.Dir)
		}
		if vid := volumeID(e.Dir); vid != "" {
			if old := st.Vols[key]; old != "" && old != vid {
				return nil, fmt.Errorf("%s: изменился серийный номер тома (был %s, стал %s) — диск подменён или отформатирован. Правило приостановлено; чтобы принять новый диск, удалите файл состояния %s", e.Dir, old, vid, en.statePath(r))
			} else if old == "" && !en.dry {
				st.setVol(key, vid)
			}
		}
		if r.FileMode {
			if !known && !en.dry {
				st.setID(key, "file")
			}
			continue
		}
		if !r.useMarker(i) {
			// Маркер выключен: в папку ничего не пишем, защита — по пути, серийному номеру тома и базе в state.
			if id != "-" && !en.dry {
				st.setID(key, "-")
			}
			continue
		}
		if id == "-" {
			known = false // раньше маркер не использовался — начинаем
		}
		marker := filepath.Join(e.Dir, markerName(r.Name))
		b, rerr := os.ReadFile(marker)
		switch {
		case rerr == nil:
			got := strings.TrimSpace(string(b))
			if known && got != id {
				return nil, fmt.Errorf("%s: маркер папки не совпадает с сохранённым (диск подменён?) — правило приостановлено", e.Dir)
			}
			if !known && !en.dry {
				st.setID(key, got)
			}
		case known:
			return nil, fmt.Errorf("%s: потерян маркер папки (диск отформатирован или папка пересоздана?) — правило приостановлено, ничего не удалено. Если так и задумано, отключите маркер (nomarker=%d) или удалите файл состояния %s", e.Dir, i+1, en.statePath(r))
		default:
			if en.dry {
				continue
			}
			nid := randID()
			if err := os.WriteFile(marker, []byte(nid+"\n"), 0o644); err != nil {
				return nil, fmt.Errorf("не удалось записать маркер в %s: %w", e.Dir, err)
			}
			st.setID(key, nid)
		}
	}
	return joining, nil
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- сканирование ----------

func matchAny(patterns []string, lname string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, lname); ok {
			return true
		}
	}
	return false
}

var tempNameRe = regexp.MustCompile(`(?i)^(localsyncext-\d{8}-\d{6}-|\.lsx-tmp-)`)

func ignored(r *Rule, name string) bool {
	l := strings.ToLower(name)
	return matchAny(defaultIgnore, l) || matchAny(r.Ignore, l) || tempNameRe.MatchString(name)
}

// removeStaleTemp удаляет «осиротевшие» временные файлы программы старше суток.
func removeStaleTemp(p string, d fs.DirEntry) {
	if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
		_ = os.Remove(p)
	}
}

func ignoredPath(r *Rule, rel string) bool {
	for _, c := range strings.FieldsFunc(rel, func(c rune) bool { return c == '\\' || c == '/' }) {
		if ignored(r, c) {
			return true
		}
	}
	return false
}

func isHot(mtime int64, now time.Time) bool {
	d := now.Sub(time.Unix(0, mtime))
	return d < settle && d > -time.Minute
}

func newCache() *epCache {
	return &epCache{files: map[string]FInfo{}, dirs: map[string]string{}}
}

func relKey(rel string) string { return strings.ToLower(filepath.ToSlash(rel)) }

func scanEndpoint(r *Rule, e *Endpoint) (*epCache, error) {
	c := newCache()
	if r.FileMode {
		fi, err := os.Lstat(filepath.Join(e.Dir, e.File))
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s — не обычный файл", e.File)
		}
		c.files[fileKey] = FInfo{Rel: e.File, Size: fi.Size(), MTime: fi.ModTime().UnixNano()}
		return c, nil
	}
	root := e.Dir
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// Нечитаемая папка не должна выглядеть как «файлы удалены».
			return werr
		}
		if p == root {
			return nil
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if !r.Recursive || ignored(r, name) {
				return fs.SkipDir
			}
			rel, _ := filepath.Rel(root, p)
			c.dirs[relKey(rel)] = filepath.ToSlash(rel)
			return nil
		}
		if d.Type().IsRegular() && tempNameRe.MatchString(name) {
			removeStaleTemp(p, d)
			return nil
		}
		if ignored(r, name) || !d.Type().IsRegular() {
			return nil
		}
		if len(r.Masks) > 0 && !matchAny(r.Masks, strings.ToLower(name)) {
			return nil
		}
		info, ierr := d.Info()
		if errors.Is(ierr, fs.ErrNotExist) {
			return nil
		}
		if ierr != nil {
			return ierr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		c.files[strings.ToLower(rel)] = FInfo{Rel: rel, Size: info.Size(), MTime: info.ModTime().UnixNano()}
		return nil
	})
	return c, err
}

// statEntry проверяет один файл. ok=false — файла нет (или он не подходит под правило).
func statEntry(r *Rule, e *Endpoint, rel string) (FInfo, bool, error) {
	fi, err := os.Lstat(r.abs(e, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return FInfo{}, false, nil
	}
	if err != nil {
		return FInfo{}, false, err
	}
	if !fi.Mode().IsRegular() {
		return FInfo{}, false, nil
	}
	if !r.FileMode {
		if ignored(r, fi.Name()) || (len(r.Masks) > 0 && !matchAny(r.Masks, strings.ToLower(fi.Name()))) {
			return FInfo{}, false, nil
		}
	}
	return FInfo{Rel: rel, Size: fi.Size(), MTime: fi.ModTime().UnixNano()}, true, nil
}

// refreshKeys обновляет кэш для перечисленных ключей во всех путях правила.
func (en *Engine) refreshKeys(r *Rule, rt *ruleRT, keys map[string]string) error {
	for key, rel := range keys {
		for i, e := range r.Endpoints {
			c := rt.cache[i]
			use := rel
			if old, ok := c.files[key]; ok {
				use = old.Rel
			}
			fi, ok, err := statEntry(r, e, use)
			if err != nil {
				return fmt.Errorf("проверка %s: %w", r.abs(e, use), err)
			}
			if ok {
				c.files[key] = fi
			} else {
				delete(c.files, key)
			}
		}
	}
	return nil
}

// resolveEvents превращает события файловой системы в набор изменённых ключей (key -> rel).
func (en *Engine) resolveEvents(r *Rule, rt *ruleRT, raws []rawEv, out map[string]string) error {
	seen := map[string]bool{}
	for _, ev := range raws {
		e := r.Endpoints[ev.ep]
		c := rt.cache[ev.ep]
		if r.FileMode {
			if strings.EqualFold(ev.rel, e.File) {
				out[fileKey] = e.File
			}
			continue
		}
		if ev.rel == "" || ignoredPath(r, ev.rel) {
			continue
		}
		rel := filepath.ToSlash(ev.rel)
		lrel := strings.ToLower(rel)
		if !r.Recursive && strings.Contains(rel, "/") {
			continue
		}
		if sk := fmt.Sprintf("%d|%s", ev.ep, lrel); seen[sk] {
			continue
		} else {
			seen[sk] = true
		}
		_, wasDir := c.dirs[lrel]
		if wasDir {
			prefix := lrel + "/"
			for k, fi := range c.files {
				if strings.HasPrefix(k, prefix) {
					out[k] = fi.Rel
				}
			}
		}
		fi, err := os.Lstat(filepath.Join(e.Dir, filepath.FromSlash(rel)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if wasDir {
				prefix := lrel + "/"
				delete(c.dirs, lrel)
				for k := range c.dirs {
					if strings.HasPrefix(k, prefix) {
						delete(c.dirs, k)
					}
				}
			} else {
				out[lrel] = rel
			}
		case err != nil:
			return err
		case fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			out[lrel] = rel
		case fi.IsDir():
			if !r.Recursive {
				continue
			}
			c.dirs[lrel] = rel
			if werr := walkNew(r, e, c, rel, out); werr != nil {
				return werr
			}
		default:
			out[lrel] = rel
		}
	}
	return nil
}

// walkNew обходит появившуюся (или переименованную) папку и помечает файлы в ней изменёнными.
func walkNew(r *Rule, e *Endpoint, c *epCache, rel string, out map[string]string) error {
	root := filepath.Join(e.Dir, filepath.FromSlash(rel))
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if errors.Is(werr, fs.ErrNotExist) {
				return nil
			}
			return werr
		}
		if p == root || d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return nil
		}
		x, _ := filepath.Rel(e.Dir, p)
		x = filepath.ToSlash(x)
		if d.IsDir() {
			if ignored(r, d.Name()) {
				return fs.SkipDir
			}
			c.dirs[strings.ToLower(x)] = x
			return nil
		}
		if !ignored(r, d.Name()) {
			out[strings.ToLower(x)] = x
		}
		return nil
	})
	return err
}

func (r *Rule) abs(e *Endpoint, rel string) string {
	if r.FileMode {
		return filepath.Join(e.Dir, e.File)
	}
	return filepath.Join(e.Dir, filepath.FromSlash(rel))
}

func labelOf(e *Endpoint, i int) string {
	if len(e.Dir) >= 2 && e.Dir[1] == ':' {
		return strings.ToUpper(e.Dir[:1])
	}
	return fmt.Sprintf("EP%d", i+1)
}

func absI64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- планирование ----------

func keyList(scans []map[string]FInfo, extra map[string]bool, only map[string]string) []string {
	set := map[string]bool{}
	if only != nil {
		for k := range only {
			set[k] = true
		}
	} else {
		for _, m := range scans {
			for k := range m {
				set[k] = true
			}
		}
		for k := range extra {
			set[k] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (en *Engine) planTwoWay(r *Rule, st *State, scans []map[string]FInfo, joining []bool, only map[string]string, now time.Time) ([]*job, map[string]string) {
	n := len(scans)
	baseKeys := map[string]bool{}
	if only == nil {
		for k := range st.Base {
			baseKeys[k] = true
		}
	}
	var jobs []*job
	hotKeys := map[string]string{}
	for _, key := range keyList(scans, baseKeys, only) {
		cur := make([]*FInfo, n)
		rel := only[key]
		hot := false
		for i := range scans {
			if fi, ok := scans[i][key]; ok {
				f := fi
				cur[i] = &f
				rel = f.Rel
				hot = hot || isHot(f.MTime, now)
			}
		}
		if hot {
			hotKeys[key] = rel
			continue
		}
		b, hasBase := st.Base[key]
		var mods, dels []int
		for i := 0; i < n; i++ {
			c := cur[i]
			switch {
			case c == nil:
				if hasBase && !joining[i] {
					dels = append(dels, i)
				}
			case !hasBase, c.Size != b.Size, absI64(c.MTime-b.MTime) > tolNS:
				mods = append(mods, i)
			}
		}
		if len(mods) == 0 && len(dels) == 0 {
			continue
		}
		if len(mods) == 0 {
			j := &job{key: key, rel: rel, winner: -1}
			for i := range cur {
				if cur[i] != nil {
					j.deletes = append(j.deletes, i)
				}
			}
			jobs = append(jobs, j)
			continue
		}

		same := func(i, k int) bool {
			a, c := cur[i], cur[k]
			if a.Size != c.Size {
				return false
			}
			if absI64(a.MTime-c.MTime) <= tolNS {
				return true
			}
			ha, e1 := hashFile(r.abs(r.Endpoints[i], a.Rel))
			hb, e2 := hashFile(r.abs(r.Endpoints[k], c.Rel))
			return e1 == nil && e2 == nil && ha == hb
		}
		w := mods[0]
		for _, i := range mods[1:] {
			if cur[i].MTime > cur[w].MTime {
				w = i
			}
		}
		isMod := map[int]bool{}
		for _, i := range mods {
			isMod[i] = true
		}
		j := &job{key: key, rel: cur[w].Rel, winner: w}
		for i := 0; i < n; i++ {
			if i == w {
				continue
			}
			switch {
			case cur[i] == nil:
				j.copies = append(j.copies, copyTo{dst: i})
			case same(w, i):
				if absI64(cur[w].MTime-cur[i].MTime) > tolNS {
					j.touch = append(j.touch, i)
				}
			case isMod[i]:
				if r.Conflict == "ask" {
					j.askConflict = true
				}
				j.copies = append(j.copies, copyTo{dst: i, rename: true})
			default:
				j.copies = append(j.copies, copyTo{dst: i})
			}
		}
		jobs = append(jobs, j)
	}
	return jobs, hotKeys
}

func (en *Engine) planOneWay(r *Rule, scans []map[string]FInfo, only map[string]string, now time.Time) ([]*job, map[string]string) {
	m := r.Master
	var jobs []*job
	hotKeys := map[string]string{}
	for _, key := range keyList(scans, nil, only) {
		mi, mok := scans[m][key]
		if mok {
			if isHot(mi.MTime, now) {
				hotKeys[key] = mi.Rel
				continue
			}
			j := &job{key: key, rel: mi.Rel, winner: m, noBase: true}
			for i := range scans {
				if i == m {
					continue
				}
				si, ok := scans[i][key]
				if ok && isHot(si.MTime, now) {
					hotKeys[key] = mi.Rel
					continue
				}
				if !ok || si.Size != mi.Size || absI64(si.MTime-mi.MTime) > tolNS {
					j.copies = append(j.copies, copyTo{dst: i})
				}
			}
			if len(j.copies) > 0 {
				jobs = append(jobs, j)
			}
			continue
		}
		if !r.DeleteExtra {
			continue
		}
		for i := range scans {
			if i == m {
				continue
			}
			if si, ok := scans[i][key]; ok {
				if isHot(si.MTime, now) {
					hotKeys[key] = si.Rel
					continue
				}
				jobs = append(jobs, &job{key: key, rel: si.Rel, winner: -1, deletes: []int{i}, noBase: true})
			}
		}
	}
	return jobs, hotKeys
}

// ---------- выполнение ----------

func conflictName(dst, label string, mt time.Time) string {
	dir, base := filepath.Split(dst)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, fmt.Sprintf("%s.sync-conflict-%s-%s%s", stem, mt.Format("20060102-150405"), label, ext))
}

func (en *Engine) execJob(r *Rule, rt *ruleRT, st *State, scans []map[string]FInfo, j *job, ps *passStats) bool {
	if j.askConflict {
		if !rt.notified[j.key] {
			rt.notified[j.key] = true
			en.logf(r.Name, "WARN", "конфликт (режим ask), файл не тронут: %s", j.key)
			en.notify(r, "Конфликт файлов", j.key+" — изменён в нескольких местах, нужен ручной выбор")
		}
		return true
	}
	ok := true
	if j.winner >= 0 {
		wi := scans[j.winner][j.key]
		src := r.abs(r.Endpoints[j.winner], wi.Rel)
		for _, c := range j.copies {
			e := r.Endpoints[c.dst]
			rel := wi.Rel
			old, hadOld := scans[c.dst][j.key]
			if hadOld {
				rel = old.Rel
			}
			dst := r.abs(e, rel)
			if c.rename && hadOld {
				cn := conflictName(dst, labelOf(e, c.dst), time.Unix(0, old.MTime))
				en.logf(r.Name, "WARN", "конфликт: %s сохранён как %s", dst, filepath.Base(cn))
				if !en.dry {
					if err := os.Rename(dst, cn); err != nil {
						en.logf(r.Name, "ERROR", "не удалось переименовать %s: %v", dst, err)
						ps.failed++
						ok = false
						continue
					}
				}
				ps.conflicts++
				en.notify(r, "Конфликт файлов", filepath.Base(dst)+" — сохранена конфликтная копия")
			}
			en.logf(r.Name, "INFO", "копирую %s -> %s", src, dst)
			if err := en.copyFile(r, src, wi, c.dst, dst); err != nil {
				ok = false
				if errors.Is(err, errSourceChanged) {
					ps.pending++
					en.logf(r.Name, "INFO", "%s ещё меняется, повтор позже", src)
				} else {
					ps.failed++
					en.logf(r.Name, "ERROR", "копирование %s -> %s: %v", src, dst, err)
				}
				continue
			}
			ps.copied++
		}
		for _, i := range j.touch {
			if !en.dry {
				mt := time.Unix(0, wi.MTime)
				_ = os.Chtimes(r.abs(r.Endpoints[i], scans[i][j.key].Rel), mt, mt)
			}
		}
	}
	for _, i := range j.deletes {
		fi := scans[i][j.key]
		p := r.abs(r.Endpoints[i], fi.Rel)
		en.logf(r.Name, "INFO", "удаляю %s", p)
		if !en.dry {
			if _, err := en.removeOld(r, i, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				en.logf(r.Name, "ERROR", "удаление %s: %v", p, err)
				ps.failed++
				ok = false
				continue
			}
		}
		ps.deleted++
	}
	if ok && !j.noBase && !en.dry {
		if j.winner >= 0 {
			wi := scans[j.winner][j.key]
			st.setBase(j.key, Base{Size: wi.Size, MTime: wi.MTime})
		} else {
			st.delBase(j.key)
		}
	}
	return ok
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// createTemp создаёт временный файл для большого файла: рядом с целевым с приставкой
// LocalSyncExt-ДАТА-ВРЕМЯ- (переименование атомарно) или в рабочей папке (temp=work, только на том же томе).
func createTemp(r *Rule, dstPath string) (*os.File, string, error) {
	dir, base := filepath.Split(dstPath)
	if r.TempMode == "work" && sameVolume(r.workDir, dir) {
		dir = filepath.Join(r.workDir, "tmp")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, "", err
		}
	}
	stamp := time.Now().Format("20060102-150405")
	for n := 0; n < 1000; n++ {
		name := fmt.Sprintf("LocalSyncExt-%s-%s", stamp, base)
		if n > 0 {
			name = fmt.Sprintf("LocalSyncExt-%s-%d-%s", stamp, n, base)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("не удалось подобрать имя временного файла")
}

func (en *Engine) copyFile(r *Rule, srcPath string, wi FInfo, di int, dstPath string) error {
	if en.dry {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	if r.DirectKB > 0 && wi.Size <= int64(r.DirectKB)*1024 {
		return en.copyDirect(r, srcPath, wi, di, dstPath)
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, tmp, err := createTemp(r, dstPath)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	fi, err := os.Stat(srcPath)
	if err != nil || fi.Size() != wi.Size || fi.ModTime().UnixNano() != wi.MTime {
		os.Remove(tmp)
		return errSourceChanged
	}
	mt := time.Unix(0, wi.MTime)
	if err := os.Chtimes(tmp, mt, mt); err != nil {
		os.Remove(tmp)
		return err
	}
	if _, err := os.Lstat(dstPath); err == nil {
		if _, err := en.removeOld(r, di, dstPath); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyDirect — малые файлы: читаем в память и пишем сразу в целевой файл, без временного.
func (en *Engine) copyDirect(r *Rule, srcPath string, wi FInfo, di int, dstPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	fi, err := os.Stat(srcPath)
	if err != nil || fi.Size() != wi.Size || fi.ModTime().UnixNano() != wi.MTime || int64(len(data)) != wi.Size {
		return errSourceChanged
	}
	mt := time.Unix(0, wi.MTime)
	trashed := ""
	if _, lerr := os.Lstat(dstPath); lerr == nil {
		if r.TrashDays < 0 {
			if err := os.WriteFile(dstPath, data, 0o644); err != nil {
				return err
			}
			return os.Chtimes(dstPath, mt, mt)
		}
		if trashed, err = en.removeOld(r, di, dstPath); err != nil {
			return err
		}
	}
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		_ = os.Remove(dstPath)
		if trashed != "" {
			_ = os.Rename(trashed, dstPath) // вернуть прежнюю версию
		}
		return err
	}
	return os.Chtimes(dstPath, mt, mt)
}

// moveFile переносит файл; если Rename невозможен (другой том) — копирует и удаляет исходный.
func moveFile(src, dst string) error {
	rerr := os.Rename(src, dst)
	if rerr == nil {
		return nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		return rerr
	}
	return copyThenRemove(src, dst, fi)
}

func copyThenRemove(src, dst string, fi fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}

// removeOld: в корзину (путь возвращается) или удалить, в зависимости от TrashDays правила.
func (en *Engine) removeOld(r *Rule, i int, p string) (string, error) {
	if r.TrashDays < 0 {
		return "", os.Remove(p)
	}
	e := r.Endpoints[i]
	rel := filepath.Base(p)
	if !r.FileMode {
		if x, err := filepath.Rel(e.Dir, p); err == nil {
			rel = x
		}
	}
	root := r.trashRoot(i)
	dir, base := filepath.Split(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(root, dir, fmt.Sprintf("%s~%s%s", stem, stamp, ext))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = filepath.Join(root, dir, fmt.Sprintf("%s~%s-%d%s", stem, stamp, n, ext))
	}
	return dst, moveFile(p, dst)
}

var trashStampRe = regexp.MustCompile(`~(\d{8}-\d{6})`)

func (en *Engine) cleanTrash(r *Rule) {
	cutoff := time.Now().Add(-time.Duration(r.TrashDays) * 24 * time.Hour)
	seen := map[string]bool{}
	for i := range r.Endpoints {
		root := r.trashRoot(i)
		if seen[strings.ToLower(root)] {
			continue
		}
		seen[strings.ToLower(root)] = true
		var dirs []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			if m := trashStampRe.FindAllStringSubmatch(d.Name(), -1); len(m) > 0 {
				if t, perr := time.ParseInLocation("20060102-150405", m[len(m)-1][1], time.Local); perr == nil && t.Before(cutoff) {
					if os.Remove(p) == nil {
						en.logf(r.Name, "INFO", "корзина: удалён устаревший %s", p)
					}
				}
			}
			return nil
		})
		for k := len(dirs) - 1; k > 0; k-- {
			_ = os.Remove(dirs[k]) // удалится, только если пуста
		}
	}
}

// cleanWorkTmp удаляет осиротевшие временные файлы в рабочей папке (старше суток).
func cleanWorkTmp(r *Rule) {
	if r.TempMode != "work" || r.workDir == "" {
		return
	}
	_ = filepath.WalkDir(filepath.Join(r.workDir, "tmp"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			removeStaleTemp(p, d)
		}
		return nil
	})
}
