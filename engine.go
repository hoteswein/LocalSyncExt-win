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
	settle     = 2 * time.Second // только что изменённый файл пропускаем до следующего прохода
	fileKey    = "<file>"
	trashDir   = ".LocalSyncTrash"
	maxEvents  = 1000
	stateFlush = 5 * time.Second
)

var (
	errSourceChanged    = errors.New("файл изменился во время копирования")
	errStalled          = errors.New("нет ответа от диска (завис или отключился)")
	errWatchUnsupported = errors.New("слежение за папками не поддерживается на этой ОС")
)

type FInfo struct {
	Rel   string
	Size  int64
	MTime int64
	Attr  *Attrs // права и владелец (Linux/Android), nil на Windows
}

// epCache — известное содержимое пути (в памяти), чтобы по событию проверять только изменённое.
type epCache struct {
	files   map[string]FInfo    // ключ файла -> файл
	dirs    map[string]string   // ключ папки -> относительный путь
	coll    map[string][]string // ключи, под которые попали файлы, отличающиеся только регистром
	collDir map[string][]string // ключи папок, отличающихся только регистром
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

	smu        sync.Mutex // поля статуса
	status     string
	lastErr    string
	lastRun    time.Time
	lastChange time.Time
	copied     int64
	deleted    int64
	conflicts  int64
	failed     int64

	paused      atomic.Bool
	lastCleanup time.Time
	lastSave    time.Time
	notified    map[string]bool
	st          *State
	cache       []*epCache
	pend        map[string]string // ключи для повторной проверки (файл ещё пишется / ошибка)
	caseIdx     caseIndex         // группы имён, различающихся регистром (режим mapped)
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
	Desc, Mode            string
	Paths                 []string
	Enabled, Paused       bool
	LastChange            time.Time
	Copied, Deleted       int64
	Conflicts, Failed     int64
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
	// OnConfig вызывается после применения новой конфигурации (например, чтобы перезапустить веб-сервер).
	OnConfig func(cfg *Config)

	mu  sync.Mutex
	ctx context.Context
	cfg *Config
	rt  map[string]*ruleRT
	run map[string]*running
	cm  *connMgr
}

func NewEngine(cfg *Config, stateDir string, dry bool, w io.Writer) *Engine {
	en := &Engine{cfg: cfg, stateDir: stateDir, dry: dry,
		out: log.New(w, "", log.Ldate|log.Ltime), rt: map[string]*ruleRT{}, run: map[string]*running{}}
	en.cm = newConnMgr(stateDir)
	en.cm.Update(cfg.Conns)
	return en
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
		rs := RuleStatus{Name: r.Name, Status: "idle", Desc: r.Desc, Mode: r.Mode, Enabled: r.Enabled}
		for _, e := range r.Endpoints {
			rs.Paths = append(rs.Paths, e.Raw)
		}
		if rt := en.rt[r.Name]; rt != nil {
			rt.smu.Lock()
			rs.Status, rs.LastErr, rs.LastRun, rs.LastChange = rt.status, rt.lastErr, rt.lastRun, rt.lastChange
			rs.Copied, rs.Deleted, rs.Conflicts, rs.Failed = rt.copied, rt.deleted, rt.conflicts, rt.failed
			rt.smu.Unlock()
			rs.Paused = rt.paused.Load()
			if rs.Paused && rs.Status != "syncing" {
				rs.Status = "paused"
			}
		}
		if r.Err != "" {
			rs.Status, rs.LastErr = "config-error", r.Err
		} else if !r.Enabled {
			rs.Status = "disabled"
		}
		out = append(out, rs)
	}
	return out
}

// SetPaused ставит правило на паузу или снимает её (после снятия — полное сканирование).
func (en *Engine) SetPaused(rule string, p bool) {
	en.mu.Lock()
	rt := en.rt[rule]
	en.mu.Unlock()
	if rt == nil {
		return
	}
	rt.paused.Store(p)
	if p {
		rt.smu.Lock()
		if rt.status != "syncing" {
			rt.status = "paused"
		}
		rt.smu.Unlock()
		en.logf(rule, "INFO", "пауза")
		return
	}
	en.logf(rule, "INFO", "пауза снята")
	select {
	case rt.trigger <- struct{}{}:
	default:
	}
}

func (en *Engine) ruleNames() []string {
	en.mu.Lock()
	defer en.mu.Unlock()
	var names []string
	for name := range en.rt {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PauseAll — пауза/продолжение для всех запущенных правил.
func (en *Engine) PauseAll(p bool) {
	for _, n := range en.ruleNames() {
		en.SetPaused(n, p)
	}
}

func (en *Engine) IsPaused(rule string) bool {
	en.mu.Lock()
	defer en.mu.Unlock()
	rt := en.rt[rule]
	return rt != nil && rt.paused.Load()
}

// AllPaused: есть запущенные правила и все они на паузе.
func (en *Engine) AllPaused() bool {
	en.mu.Lock()
	defer en.mu.Unlock()
	if len(en.rt) == 0 {
		return false
	}
	for _, rt := range en.rt {
		if !rt.paused.Load() {
			return false
		}
	}
	return true
}

// Busy: хотя бы одно правило сейчас синхронизирует (для анимации значка).
func (en *Engine) Busy() bool {
	en.mu.Lock()
	defer en.mu.Unlock()
	for _, rt := range en.rt {
		rt.smu.Lock()
		b := rt.status == "syncing"
		rt.smu.Unlock()
		if b {
			return true
		}
	}
	return false
}

// TriggerAll — «синхронизировать всё сейчас».
func (en *Engine) TriggerAll() {
	for _, n := range en.ruleNames() {
		en.Trigger(n)
	}
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
	en.applyRules(cfg)
	if f := en.OnConfig; f != nil {
		f(cfg)
	}
}

func (en *Engine) applyRules(cfg *Config) {
	en.mu.Lock()
	defer en.mu.Unlock()
	en.cm.Update(cfg.Conns)
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
		if r.kind(i) != "local" {
			continue // сетевые диски и облако событий не присылают — их проверяет периодическое сканирование
		}
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
	streak := 0

	for {
		if lost.Swap(false) {
			needFull, run = true, true
		}
		if run && rt.paused.Load() {
			raws, needFull, run = nil, true, false
			retryAt = time.Time{}
			rt.smu.Lock()
			if rt.status != "syncing" {
				rt.status = "paused"
			}
			rt.smu.Unlock()
		}
		if run {
			batch := raws
			raws = nil
			wasFull := needFull
			retry, err := en.pass(r, rt, needFull, batch, false)
			needFull, run = false, false
			if err != nil {
				if streak < 8 {
					streak++
				}
				if back := 30 * time.Second << uint(streak-1); back > retry {
					retry = back // 30 с, 1 мин, 2 мин ... до 10 минут (сеть могла пропасть надолго)
				}
				if retry > 10*time.Minute {
					retry = 10 * time.Minute
				}
			} else {
				streak = 0
			}
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
				if rt.paused.Load() {
					needFull = true // события на паузе не копим: после паузы будет полное сканирование
					break
				}
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
	wi          FInfo  // версия победителя на момент планирования (для базы состояния)
	attr        *Attrs // права, запомненные в базе (для записи на Linux после обхода через Windows)
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
	if aerr := en.attachFS(r); aerr != nil {
		return 0, aerr
	}
	r.prepare() // теперь можно определить, файл это или папка, и на SFTP-путях
	r.detectCaseMode()
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
			c, serr := scanGuarded(r, i, e)
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

	en.reconcileNames(r, rt)
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

	if len(rt.cache) > 0 {
		jobs = en.skipCaseCollisions(r, rt, jobs)
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
	defer func() {
		rt.smu.Lock()
		rt.copied += int64(ps.copied)
		rt.deleted += int64(ps.deleted)
		rt.conflicts += int64(ps.conflicts)
		rt.failed += int64(ps.failed)
		if ps.copied+ps.deleted+ps.conflicts > 0 {
			rt.lastChange = time.Now()
		}
		rt.smu.Unlock()
	}()
	failed := map[string]string{}
	touched := map[string]string{}
	var okWin []*job
	for _, j := range jobs {
		touched[j.key] = j.rel
		if !en.execJob(r, rt, st, scans, j, &ps) {
			failed[j.key] = j.rel
		} else if j.winner >= 0 && !j.noBase && !j.askConflict {
			okWin = append(okWin, j)
		}
	}
	if len(touched) > 0 && !en.dry {
		if rerr := en.refreshKeys(r, rt, touched); rerr != nil {
			rt.cache = nil
		}
	}
	if !en.dry {
		for _, j := range okWin {
			b := Base{Size: j.wi.Size, MTime: j.wi.MTime}
			if rt.cache != nil { // что реально увидели на каждом пути после копирования
				b.Obs = map[string]int64{}
				for i, e := range r.Endpoints {
					if i == j.winner {
						b.Obs[epID(e)] = j.wi.MTime
					} else if f, ok := rt.cache[i].files[j.key]; ok && f.Size == j.wi.Size {
						b.Obs[epID(e)] = f.MTime
					}
				}
			}
			b.Attr = j.wi.Attr
			if b.Attr == nil && rt.cache != nil {
				for i := range r.Endpoints {
					if f, ok := rt.cache[i].files[j.key]; ok && f.Attr != nil {
						b.Attr = f.Attr
						break
					}
				}
			}
			if b.Attr == nil {
				b.Attr = j.attr
			}
			st.setBase(j.key, b)
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
		var fi fs.FileInfo
		var err error
		if d := r.stall(i) / 3; d > 0 { // удалённый диск может зависнуть: не ждём вечно
			err = guardOp(d, func() (serr error) { fi, serr = e.FS.Stat(e.Dir); return })
			if errors.Is(err, errStalled) {
				return nil, fmt.Errorf("%s не отвечает более %v — правило приостановлено до восстановления связи, ничего не удалено", e.Dir, d)
			}
		} else {
			fi, err = e.FS.Stat(e.Dir)
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				if e.Conn != "" {
					return nil, fmt.Errorf("sftp://%s%s: %w", e.Conn, e.Dir, err)
				}
				return nil, fmt.Errorf("%s: %w", e.Dir, err)
			}
			if known {
				return nil, fmt.Errorf("%s недоступен (диск отключён или папка удалена) — синхронизация правила приостановлена, ничего не удалено", e.Dir)
			}
			if en.dry {
				continue
			}
			if err := r.op(i, func() error { return e.FS.MkdirAll(e.Dir) }); err != nil {
				return nil, fmt.Errorf("не удалось создать %s: %w", e.Dir, err)
			}
		} else if !fi.IsDir() {
			return nil, fmt.Errorf("%s — не папка", e.Dir)
		}
		vid := ""
		if e.FS.Local() && r.stall(i) == 0 {
			vid = volumeID(e.Dir)
		}
		if vid != "" {
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
		marker := e.FS.Join(e.Dir, markerName(r.Name))
		var b []byte
		rerr := r.op(i, func() (xerr error) { b, xerr = e.FS.ReadFile(marker); return })
		if errors.Is(rerr, errStalled) {
			return nil, fmt.Errorf("%s не отвечает — правило приостановлено", e.Dir)
		}
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
			if err := r.op(i, func() error { return e.FS.WriteFile(marker, []byte(nid+"\n")) }); err != nil {
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
func removeStaleTemp(f FileFS, p string, d fs.DirEntry) {
	if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
		_ = f.Remove(p)
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
	return &epCache{files: map[string]FInfo{}, dirs: map[string]string{}, coll: map[string][]string{}, collDir: map[string][]string{}}
}

func relKey(rel string) string { return strings.ToLower(filepath.ToSlash(rel)) }

// scanGuarded сканирует путь; для удалённых путей прерывается, если обход не движется дольше таймаута.
func scanGuarded(r *Rule, i int, e *Endpoint) (*epCache, error) {
	stall := r.stall(i)
	if stall == 0 {
		return scanEndpoint(r, i, e, nil)
	}
	type res struct {
		c   *epCache
		err error
	}
	var prog atomic.Int64
	done := make(chan res, 1)
	go func() {
		c, err := scanEndpoint(r, i, e, &prog)
		done <- res{c, err}
	}()
	last, lastT := int64(-1), time.Now()
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case x := <-done:
			return x.c, x.err
		case <-tk.C:
			if p := prog.Load(); p != last {
				last, lastT = p, time.Now()
			} else if time.Since(lastT) > stall {
				return nil, errStalled
			}
		}
	}
}

func scanEndpoint(r *Rule, ci int, e *Endpoint, prog *atomic.Int64) (*epCache, error) {
	c := newCache()
	physSeen := map[string]string{}
	if r.FileMode {
		fi, err := e.FS.Lstat(e.FS.Join(e.Dir, e.File))
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s — не обычный файл", e.File)
		}
		c.files[fileKey] = FInfo{Rel: e.File, Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Attr: e.FS.AttrsOf(fi)}
		return c, nil
	}
	root := e.Dir
	err := walkFS(e.FS, root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// Нечитаемая папка не должна выглядеть как «файлы удалены».
			return werr
		}
		if p == root {
			return nil
		}
		if prog != nil {
			prog.Add(1)
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if !r.Recursive || ignored(r, name) {
				return fs.SkipDir
			}
			rel, _ := e.FS.RelSlash(root, p)
			dk := r.dirKey(rel)
			if prev, ok := c.dirs[dk]; ok && prev != rel && r.caseMode == "mapped" {
				c.collDir[dk] = append(c.collDir[dk], prev, rel)
			}
			c.dirs[dk] = rel
			return nil
		}
		if d.Type().IsRegular() && tempNameRe.MatchString(name) {
			removeStaleTemp(e.FS, p, d)
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
		rel, _ := e.FS.RelSlash(root, p)
		key := r.keyOf(r.logical(ci, rel))
		if r.caseMode == "mapped" && !e.FS.CaseSensitive() {
			// диск без учёта регистра (в том числе смонтированный облачный) показал имена, отличающиеся регистром
			pk := strings.ToLower(rel)
			if prevRel, seen := physSeen[pk]; seen {
				pkey := r.keyOf(r.logical(ci, prevRel))
				c.coll[pkey] = append(c.coll[pkey], prevRel)
				c.coll[key] = append(c.coll[key], rel)
			}
			physSeen[pk] = rel
		}
		if prev, dup := c.files[key]; dup {
			if len(c.coll[key]) == 0 {
				c.coll[key] = []string{prev.Rel}
			}
			c.coll[key] = append(c.coll[key], rel)
		}
		c.files[key] = FInfo{Rel: rel, Size: info.Size(), MTime: info.ModTime().UnixNano(), Attr: e.FS.AttrsOf(info)}
		return nil
	})
	return c, err
}

// statEntry проверяет один файл. ok=false — файла нет (или он не подходит под правило).
func statEntry(r *Rule, e *Endpoint, rel string) (FInfo, bool, error) {
	fi, err := e.FS.Lstat(r.abs(e, rel))
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
	return FInfo{Rel: rel, Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Attr: e.FS.AttrsOf(fi)}, true, nil
}

// refreshKeys обновляет кэш для перечисленных ключей во всех путях правила.
func (en *Engine) refreshKeys(r *Rule, rt *ruleRT, keys map[string]string) error {
	for key, rel := range keys {
		for i, e := range r.Endpoints {
			c := rt.cache[i]
			use := rel
			if old, ok := c.files[key]; ok {
				use = old.Rel
			} else {
				use = r.relFor(rt, i, key, rel)
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
		lrel := r.dirKey(rel)
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
					out[k] = r.logical(ev.ep, fi.Rel)
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
				lr := r.logical(ev.ep, rel)
				out[r.keyOf(lr)] = lr
			}
		case err != nil:
			return err
		case fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			lr := r.logical(ev.ep, rel)
			out[r.keyOf(lr)] = lr
		case fi.IsDir():
			if !r.Recursive {
				continue
			}
			c.dirs[lrel] = rel
			if werr := walkNew(r, ev.ep, e, c, rel, out); werr != nil {
				return werr
			}
		default:
			lr := r.logical(ev.ep, rel)
			out[r.keyOf(lr)] = lr
		}
	}
	return nil
}

// walkNew обходит появившуюся (или переименованную) папку и помечает файлы в ней изменёнными.
func walkNew(r *Rule, ci int, e *Endpoint, c *epCache, rel string, out map[string]string) error {
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
			c.dirs[r.dirKey(x)] = x
			return nil
		}
		if !ignored(r, d.Name()) {
			lx := r.logical(ci, x)
			out[r.keyOf(lx)] = lx
		}
		return nil
	})
	return err
}

func (r *Rule) abs(e *Endpoint, rel string) string {
	if r.FileMode {
		return e.FS.Join(e.Dir, e.File)
	}
	return e.FS.Join(e.Dir, e.FS.FromRel(rel))
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

func hashFile(f FileFS, p string) (string, error) {
	in, err := f.Open(p)
	if err != nil {
		return "", err
	}
	defer in.Close()
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
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

func epID(e *Endpoint) string { return strings.ToLower(e.Raw) }

func (en *Engine) planTwoWay(r *Rule, st *State, scans []map[string]FInfo, joining []bool, only map[string]string, now time.Time) ([]*job, map[string]string) {
	n := len(scans)
	tol := r.tol()
	epIDs := make([]string, n)
	for i, e := range r.Endpoints {
		epIDs[i] = epID(e)
	}
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
				rel = r.logical(i, f.Rel)
				hot = hot || isHot(f.MTime, now)
			}
		}
		if hot {
			hotKeys[key] = rel
			continue
		}
		b, hasBase := st.Base[key]
		modified := func(i int, c *FInfo, b Base) bool {
			if c.Size != b.Size {
				return true
			}
			if !r.mtimeReliable(i) {
				return false // облако: время не сохраняется, меняется только размер
			}
			ref := b.MTime
			if o, ok := b.Obs[epIDs[i]]; ok {
				ref = o
			}
			return absI64(c.MTime-ref) > tol
		}
		var mods, dels []int
		for i := 0; i < n; i++ {
			c := cur[i]
			switch {
			case c == nil:
				if hasBase && !joining[i] {
					dels = append(dels, i)
				}
			case !hasBase || modified(i, c, b):
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

		isMod := map[int]bool{}
		for _, i := range mods {
			isMod[i] = true
		}
		same := func(i, k int) bool {
			a, c := cur[i], cur[k]
			if a.Size != c.Size {
				return false
			}
			unrel := !r.mtimeReliable(i) || !r.mtimeReliable(k)
			if !unrel && absI64(a.MTime-c.MTime) <= tol {
				return true
			}
			if unrel && hasBase && !(isMod[i] && isMod[k]) {
				return false // не скачиваем файл из облака ради сравнения: проще скопировать заново
			}
			ha, e1 := hashFile(r.Endpoints[i].FS, r.abs(r.Endpoints[i], a.Rel))
			hb, e2 := hashFile(r.Endpoints[k].FS, r.abs(r.Endpoints[k], c.Rel))
			return e1 == nil && e2 == nil && ha == hb
		}
		// победитель: у кого время изменения надёжно и новее (у облака время ненадёжно — оно проигрывает)
		w := mods[0]
		for _, i := range mods[1:] {
			ri, rw := r.mtimeReliable(i), r.mtimeReliable(w)
			if (ri && !rw) || (ri == rw && cur[i].MTime > cur[w].MTime) {
				w = i
			}
		}
		j := &job{key: key, rel: r.logical(w, cur[w].Rel), winner: w, wi: *cur[w]}
		if hasBase {
			j.attr = b.Attr
		}
		for i := 0; i < n; i++ {
			if i == w {
				continue
			}
			switch {
			case cur[i] == nil:
				j.copies = append(j.copies, copyTo{dst: i})
			case same(w, i):
				if r.mtimeReliable(i) && r.mtimeReliable(w) && absI64(cur[w].MTime-cur[i].MTime) > tol {
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
	tol := r.tol()
	var jobs []*job
	hotKeys := map[string]string{}
	for _, key := range keyList(scans, nil, only) {
		mi, mok := scans[m][key]
		if mok {
			if isHot(mi.MTime, now) {
				hotKeys[key] = r.logical(m, mi.Rel)
				continue
			}
			j := &job{key: key, rel: r.logical(m, mi.Rel), winner: m, noBase: true}
			for i := range scans {
				if i == m {
					continue
				}
				si, ok := scans[i][key]
				if ok && isHot(si.MTime, now) {
					hotKeys[key] = r.logical(m, mi.Rel)
					continue
				}
				if !ok || si.Size != mi.Size || (r.mtimeReliable(i) && r.mtimeReliable(m) && absI64(si.MTime-mi.MTime) > tol) {
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
					hotKeys[key] = r.logical(i, si.Rel)
					continue
				}
				jobs = append(jobs, &job{key: key, rel: r.logical(i, si.Rel), winner: -1, deletes: []int{i}, noBase: true})
			}
		}
	}
	return jobs, hotKeys
}

// ---------- выполнение ----------

func conflictName(f FileFS, dst, label string, mt time.Time) string {
	dir, base := f.Dir(dst), f.Base(dst)
	stem, ext := splitName(base)
	return f.Join(dir, fmt.Sprintf("%s.sync-conflict-%s-%s%s", stem, mt.Format("20060102-150405"), label, ext))
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
			old, hadOld := scans[c.dst][j.key]
			var rel string
			if hadOld {
				rel = old.Rel
			} else {
				rel = r.relFor(rt, c.dst, j.key, r.logical(j.winner, wi.Rel))
			}
			dst := r.abs(e, rel)
			if c.rename && hadOld {
				cn := conflictName(e.FS, dst, labelOf(e, c.dst), time.Unix(0, old.MTime))
				en.logf(r.Name, "WARN", "конфликт: %s сохранён как %s", dst, e.FS.Base(cn))
				if !en.dry {
					if err := r.op(c.dst, func() error { return e.FS.Rename(dst, cn) }); err != nil {
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
			if err := en.copyFile(r, j.winner, src, wi, c.dst, dst, j.attr); err != nil {
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
				p := r.abs(r.Endpoints[i], scans[i][j.key].Rel)
				_ = r.op(i, func() error { return r.Endpoints[i].FS.Chtimes(p, mt) })
			}
		}
	}
	for _, i := range j.deletes {
		fi := scans[i][j.key]
		p := r.abs(r.Endpoints[i], fi.Rel)
		en.logf(r.Name, "INFO", "удаляю %s", p)
		if !en.dry {
			if err := r.op(i, func() error { _, rerr := en.removeOld(r, i, p); return rerr }); err != nil && !errors.Is(err, fs.ErrNotExist) {
				en.logf(r.Name, "ERROR", "удаление %s: %v", p, err)
				ps.failed++
				ok = false
				continue
			}
		}
		ps.deleted++
	}
	if ok && !j.noBase && !en.dry && j.winner < 0 {
		st.delBase(j.key)
	}
	return ok
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// createTemp создаёт временный файл для большого файла: рядом с целевым с приставкой
// LocalSyncExt-ДАТА-ВРЕМЯ- (переименование атомарно) или в рабочей папке (temp=work, только на том же томе).
func createTemp(r *Rule, di int, dstPath string) (WFile, string, error) {
	f := r.Endpoints[di].FS
	dir, base := f.Dir(dstPath), f.Base(dstPath)
	if r.TempMode == "work" && f.Local() && sameVolume(r.workDir, dir) {
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
		p := f.Join(dir, name)
		w, err := f.Create(p, true)
		if err == nil {
			return w, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("не удалось подобрать имя временного файла")
}

func (en *Engine) copyFile(r *Rule, si int, srcPath string, wi FInfo, di int, dstPath string, hint *Attrs) error {
	if en.dry {
		return nil
	}
	sf, df := r.Endpoints[si].FS, r.Endpoints[di].FS
	if err := r.op(di, func() error { return df.MkdirAll(df.Dir(dstPath)) }); err != nil {
		return err
	}
	if r.DirectKB > 0 && wi.Size <= int64(r.DirectKB)*1024 {
		return en.copyDirect(r, si, srcPath, wi, di, dstPath, hint)
	}
	in, err := sf.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	var out WFile
	var tmp string
	if err := r.op(di, func() (cerr error) { out, tmp, cerr = createTemp(r, di, dstPath); return }); err != nil {
		return err
	}
	if stall := maxDur(r.stall(si), r.stall(di)); stall > 0 {
		err = copyStall(out, in, stall)
		if errors.Is(err, errStalled) {
			go df.Remove(tmp) // может зависнуть вместе с диском — не ждём
			return err
		}
	} else {
		_, err = io.Copy(out, in)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		df.Remove(tmp)
		return err
	}
	fi, err := sf.Stat(srcPath)
	if err != nil || fi.Size() != wi.Size || fi.ModTime().UnixNano() != wi.MTime {
		df.Remove(tmp)
		return errSourceChanged
	}
	mt := time.Unix(0, wi.MTime)
	if err := r.op(di, func() error { return df.Chtimes(tmp, mt) }); err != nil && r.mtimeReliable(di) {
		df.Remove(tmp)
		return err
	}
	if want := wantAttrs(r, di, dstPath, wi, hint); want != nil {
		en.applyAttrs(r, di, tmp, *want)
	}
	if err := r.op(di, func() error {
		if _, lerr := df.Lstat(dstPath); lerr == nil {
			_, rerr := en.removeOld(r, di, dstPath)
			return rerr
		}
		return nil
	}); err != nil {
		df.Remove(tmp)
		return err
	}
	if err := r.op(di, func() error { return df.Rename(tmp, dstPath) }); err != nil {
		df.Remove(tmp)
		return err
	}
	return nil
}

// wantAttrs — какие права и владельца поставить новому файлу: как у заменяемого файла,
// иначе как у источника (Linux → Linux), иначе как запомнено в базе (после обхода через Windows).
func wantAttrs(r *Rule, di int, dstPath string, wi FInfo, hint *Attrs) *Attrs {
	df := r.Endpoints[di].FS
	if !df.HasAttrs() {
		return nil
	}
	var old *Attrs
	_ = r.op(di, func() error {
		if info, err := df.Lstat(dstPath); err == nil {
			old = df.AttrsOf(info)
		}
		return nil
	})
	switch {
	case old != nil:
		return old
	case wi.Attr != nil:
		return wi.Attr
	}
	return hint
}

func (en *Engine) applyAttrs(r *Rule, di int, p string, a Attrs) {
	df := r.Endpoints[di].FS
	if err := r.op(di, func() error { return df.SetAttrs(p, a) }); err != nil && r.attrWarned.CompareAndSwap(false, true) {
		en.logf(r.Name, "WARN", "права или владелец не применены к файлу на пути №%d: %v (для смены владельца нужен вход под root)", di+1, err)
	}
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// guardOp выполняет fn и возвращает errStalled, если ответа нет дольше d.
// Зависшая операция остаётся в своей горутине (прервать вызов файловой системы нельзя).
func guardOp(d time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return errStalled
	}
}

// copyStall копирует данные кусками и прерывается, если движения нет дольше stall
// (оборвалась сеть, завис rclone/sshfs).
func copyStall(dst io.WriteCloser, src io.ReadCloser, stall time.Duration) error {
	var prog atomic.Int64
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 256<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					done <- werr
					return
				}
				prog.Add(int64(n))
			}
			if err == io.EOF {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	last, lastT := int64(-1), time.Now()
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tk.C:
			if p := prog.Load(); p != last {
				last, lastT = p, time.Now()
			} else if time.Since(lastT) > stall {
				src.Close()
				dst.Close()
				return errStalled
			}
		}
	}
}

// copyDirect — малые файлы: читаем в память и пишем сразу в целевой файл, без временного.
func (en *Engine) copyDirect(r *Rule, si int, srcPath string, wi FInfo, di int, dstPath string, hint *Attrs) error {
	sf, df := r.Endpoints[si].FS, r.Endpoints[di].FS
	var data []byte
	err := r.op(si, func() (rerr error) { data, rerr = sf.ReadFile(srcPath); return })
	if err != nil {
		return err
	}
	fi, err := sf.Stat(srcPath)
	if err != nil || fi.Size() != wi.Size || fi.ModTime().UnixNano() != wi.MTime || int64(len(data)) != wi.Size {
		return errSourceChanged
	}
	mt := time.Unix(0, wi.MTime)
	setTime := func() error {
		if err := r.op(di, func() error { return df.Chtimes(dstPath, mt) }); err != nil && r.mtimeReliable(di) {
			return err
		}
		return nil
	}
	want := wantAttrs(r, di, dstPath, wi, hint)
	trashed := ""
	if _, lerr := df.Lstat(dstPath); lerr == nil {
		if r.TrashDays < 0 {
			if err := r.op(di, func() error { return df.WriteFile(dstPath, data) }); err != nil {
				return err
			}
			return setTime()
		}
		if err := r.op(di, func() (rerr error) { trashed, rerr = en.removeOld(r, di, dstPath); return }); err != nil {
			return err
		}
	}
	if err := r.op(di, func() error { return df.WriteFile(dstPath, data) }); err != nil {
		_ = df.Remove(dstPath)
		if trashed != "" {
			_ = df.Rename(trashed, dstPath) // вернуть прежнюю версию
		}
		return err
	}
	if want != nil {
		en.applyAttrs(r, di, dstPath, *want)
	}
	return setTime()
}

// moveFile переносит файл; если Rename невозможен (другой том) — копирует и удаляет исходный.

// removeOld: в корзину (путь возвращается) или удалить, в зависимости от TrashDays правила.
func (en *Engine) removeOld(r *Rule, i int, p string) (string, error) {
	sf := r.Endpoints[i].FS
	if r.TrashDays < 0 {
		return "", sf.Remove(p)
	}
	e := r.Endpoints[i]
	tf, root := r.trashRoot(i)
	rel := sf.Base(p)
	if !r.FileMode {
		if x, err := sf.RelSlash(e.Dir, p); err == nil {
			rel = x
		}
	}
	relN := tf.FromRel(rel)
	dir, base := tf.Dir(relN), tf.Base(relN)
	if dir == "." {
		dir = ""
	}
	stem, ext := splitName(base)
	stamp := time.Now().Format("20060102-150405")
	dst := tf.Join(root, dir, fmt.Sprintf("%s~%s%s", stem, stamp, ext))
	if err := tf.MkdirAll(tf.Dir(dst)); err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		if _, err := tf.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = tf.Join(root, dir, fmt.Sprintf("%s~%s-%d%s", stem, stamp, n, ext))
	}
	return dst, moveBetween(sf, p, tf, dst)
}

var trashStampRe = regexp.MustCompile(`~(\d{8}-\d{6})`)

func (en *Engine) cleanTrash(r *Rule) {
	cutoff := time.Now().Add(-time.Duration(r.TrashDays) * 24 * time.Hour)
	seen := map[string]bool{}
	for i := range r.Endpoints {
		tf, root := r.trashRoot(i)
		if seen[strings.ToLower(root)] {
			continue
		}
		seen[strings.ToLower(root)] = true
		if r.kind(i) != "local" && tf != theLocalFS {
			// корзина на удалённом диске: чистим тем же способом, но с ограничением ожидания
		}
		var dirs []string
		_ = walkFS(tf, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				return nil
			}
			if m := trashStampRe.FindAllStringSubmatch(d.Name(), -1); len(m) > 0 {
				if t, perr := time.ParseInLocation("20060102-150405", m[len(m)-1][1], time.Local); perr == nil && t.Before(cutoff) {
					if tf.Remove(p) == nil {
						en.logf(r.Name, "INFO", "корзина: удалён устаревший %s", p)
					}
				}
			}
			return nil
		})
		for k := len(dirs) - 1; k > 0; k-- {
			_ = tf.Remove(dirs[k]) // удалится, только если пуста
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
			removeStaleTemp(theLocalFS, p, d)
		}
		return nil
	})
}

// skipCaseCollisions исключает из плана файлы, имена которых различаются только регистром:
// Windows-программы (и rclone/sshfs на Windows) не различают их при открытии, и можно перезаписать не тот файл.
func (en *Engine) skipCaseCollisions(r *Rule, rt *ruleRT, jobs []*job) []*job {
	skip := map[string]bool{}
	for i, c := range rt.cache {
		for k, rels := range c.coll {
			skip[k] = true
			if !rt.notified["case:"+k] {
				rt.notified["case:"+k] = true
				en.logf(r.Name, "WARN", "файлы отличаются только регистром, пропущены: %s (путь №%d). имена по схеме [linux_case] применяются только когда одна из сторон — Linux (SFTP)", strings.Join(rels, " | "), i+1)
				en.notify(r, "Конфликт имён", "Файлы отличаются только регистром и пропущены: "+rels[0])
			}
		}
	}
	var skipDirs []string
	for i, c := range rt.cache {
		for k, rels := range c.collDir {
			skipDirs = append(skipDirs, k+"/")
			if !rt.notified["cased:"+k] {
				rt.notified["cased:"+k] = true
				en.logf(r.Name, "WARN", "папки отличаются только регистром, их содержимое пропущено: %s (путь №%d)", strings.Join(rels, " | "), i+1)
				en.notify(r, "Конфликт имён", "Папки отличаются только регистром и пропущены: "+rels[0])
			}
		}
	}
	if len(skip) == 0 && len(skipDirs) == 0 {
		return jobs
	}
	out := jobs[:0:0]
next:
	for _, j := range jobs {
		if skip[j.key] {
			continue
		}
		for _, d := range skipDirs {
			if strings.HasPrefix(j.key, d) {
				continue next
			}
		}
		out = append(out, j)
	}
	return out
}

// attachFS подставляет каждому пути правила его файловую систему (локальный диск или SFTP).
func (en *Engine) attachFS(r *Rule) error {
	for _, e := range r.Endpoints {
		if e.Conn == "" {
			e.FS = theLocalFS
			continue
		}
		f, err := en.cm.Get(e.Conn)
		if err != nil {
			return err
		}
		e.FS = f
	}
	return nil
}
