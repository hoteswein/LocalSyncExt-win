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
	tolNS     = int64(2 * time.Second) // допуск времени (FAT/exFAT хранят с точностью 2 с)
	settle    = 2 * time.Second        // файл, изменённый только что, пропускаем до следующего прохода
	fileKey   = "<file>"
	trashDir  = ".LocalSyncTrash"
	maxEvents = 300
)

var errSourceChanged = errors.New("файл изменился во время копирования")

type FInfo struct {
	Rel   string
	Size  int64
	MTime int64
	Hot   bool
}

type Event struct {
	Time  time.Time
	Rule  string
	Level string
	Msg   string
}

type ruleRT struct {
	mu          sync.Mutex
	trigger     chan struct{}
	status      string
	lastErr     string
	lastRun     time.Time
	lastCleanup time.Time
	notified    map[string]bool
}

type RuleStatus struct {
	Name, Status, LastErr string
	LastRun               time.Time
}

type Engine struct {
	cfg      *Config
	stateDir string
	dry      bool
	out      *log.Logger
	evMu     sync.Mutex
	events   []Event
	rt       map[string]*ruleRT
	// OnNotify вызывается для уведомлений (трей подключим позже).
	OnNotify func(rule, title, msg string)
}

func NewEngine(cfg *Config, stateDir string, dry bool, w io.Writer) *Engine {
	en := &Engine{cfg: cfg, stateDir: stateDir, dry: dry, out: log.New(w, "", log.Ldate|log.Ltime), rt: map[string]*ruleRT{}}
	for _, r := range cfg.Rules {
		en.rt[r.Name] = &ruleRT{trigger: make(chan struct{}, 1), status: "idle", notified: map[string]bool{}}
	}
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

func (en *Engine) Statuses() []RuleStatus {
	var out []RuleStatus
	for _, r := range en.cfg.Rules {
		rt := en.rt[r.Name]
		st := rt.status
		if r.Err != "" {
			st = "config-error"
		} else if !r.Enabled {
			st = "disabled"
		}
		out = append(out, RuleStatus{r.Name, st, rt.lastErr, rt.lastRun})
	}
	return out
}

// Trigger — «синхронизировать сейчас».
func (en *Engine) Trigger(rule string) {
	if rt := en.rt[rule]; rt != nil {
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

// ---------- запуск ----------

// RunOnce — один проход по всем правилам. true, если всё прошло без ошибок.
func (en *Engine) RunOnce() bool {
	ok := true
	for _, r := range en.cfg.Rules {
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

// Run — работа в фоне до отмены контекста.
func (en *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range en.cfg.Rules {
		if !r.Enabled {
			continue
		}
		if r.Err != "" {
			en.logf(r.Name, "ERROR", "правило отключено из-за ошибки конфигурации: %s", r.Err)
			continue
		}
		wg.Add(1)
		go func(r *Rule) {
			defer wg.Done()
			rt := en.rt[r.Name]
			for {
				_ = en.SyncRule(r)
				var tick <-chan time.Time
				var timer *time.Timer
				if r.Scan > 0 {
					timer = time.NewTimer(time.Duration(r.Scan) * time.Second)
					tick = timer.C
				}
				select {
				case <-ctx.Done():
					if timer != nil {
						timer.Stop()
					}
					return
				case <-tick:
				case <-rt.trigger:
					if timer != nil {
						timer.Stop()
					}
				}
			}
		}(r)
	}
	wg.Wait()
}

// ---------- проход по правилу ----------

type copyTo struct {
	dst    int
	rename bool // существующий файл назначения — проигравший в конфликте: сначала переименовать
}

type job struct {
	key         string
	winner      int // -1 — удаление
	copies      []copyTo
	touch       []int
	deletes     []int
	noBase      bool
	askConflict bool
}

type passStats struct{ copied, deleted, conflicts, failed, pending int }

func (en *Engine) SyncRule(r *Rule) (err error) {
	rt := en.rt[r.Name]
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.status, rt.lastErr = "syncing", ""
	started := time.Now()
	defer func() {
		rt.lastRun = time.Now()
		if err != nil {
			rt.status, rt.lastErr = "error", err.Error()
			en.logf(r.Name, "ERROR", "%v", err)
			en.notify(r, "Ошибка синхронизации", err.Error())
		} else {
			rt.status = "ok"
		}
	}()

	r.prepare()
	if r.Err != "" {
		return errors.New(r.Err)
	}
	st, err := loadState(en.statePath(r))
	if err != nil {
		return err
	}
	defer func() {
		if !en.dry {
			if serr := st.save(en.statePath(r)); serr != nil && err == nil {
				err = serr
			}
		}
	}()

	joining, err := en.checkEndpoints(r, st)
	if err != nil {
		return err
	}
	now := time.Now()
	scans := make([]map[string]FInfo, len(r.Endpoints))
	for i, e := range r.Endpoints {
		m, serr := scanEndpoint(r, e, now)
		if serr != nil {
			return fmt.Errorf("сканирование %s: %w", e.Raw, serr)
		}
		scans[i] = m
	}

	var jobs []*job
	total := 0
	if r.Mode == "oneway" {
		jobs = en.planOneWay(r, scans)
		for _, m := range scans {
			if len(m) > total {
				total = len(m)
			}
		}
	} else {
		jobs = en.planTwoWay(r, st, scans, joining)
		total = len(st.Base)
	}

	dels := 0
	for _, j := range jobs {
		if j.winner < 0 && len(j.deletes) > 0 {
			dels++
		}
	}
	if dels >= 5 && dels*100 > r.MaxDeletePct*total {
		return fmt.Errorf("защита от массового удаления: планируется удалить %d из %d файлов (лимит %d%%). Проверьте диски; если это нормально — увеличьте MaxDeletePercent в правиле", dels, total, r.MaxDeletePct)
	}

	var ps passStats
	for _, j := range jobs {
		en.execJob(r, st, scans, j, &ps)
	}
	if !en.dry && r.TrashDays > 0 && time.Since(rt.lastCleanup) > time.Hour {
		en.cleanTrash(r)
		rt.lastCleanup = time.Now()
	}
	if len(jobs) > 0 || ps.failed > 0 {
		en.logf(r.Name, "INFO", "проход: копий %d, удалений %d, конфликтов %d, ошибок %d, отложено %d, за %v", ps.copied, ps.deleted, ps.conflicts, ps.failed, ps.pending, time.Since(started).Round(time.Millisecond))
	}
	if ps.failed > 0 {
		return fmt.Errorf("ошибок при выполнении операций: %d (см. журнал)", ps.failed)
	}
	return nil
}

// checkEndpoints: доступность путей и маркеры. Возвращает, какие пути «присоединяются» к правилу с историей.
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
		if r.FileMode {
			if !known && !en.dry {
				st.IDs[key] = "file"
			}
			continue
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
				st.IDs[key] = got
			}
		case known:
			return nil, fmt.Errorf("%s: потерян маркер папки (диск отформатирован или папка пересоздана?) — правило приостановлено, ничего не удалено", e.Dir)
		default:
			if en.dry {
				continue
			}
			nid := randID()
			if err := os.WriteFile(marker, []byte(nid+"\n"), 0o644); err != nil {
				return nil, fmt.Errorf("не удалось записать маркер в %s: %w", e.Dir, err)
			}
			st.IDs[key] = nid
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

func ignored(r *Rule, name string) bool {
	l := strings.ToLower(name)
	return matchAny(defaultIgnore, l) || matchAny(r.Ignore, l)
}

func isHot(mtime int64, now time.Time) bool {
	d := now.Sub(time.Unix(0, mtime))
	return d < settle && d > -time.Minute
}

func scanEndpoint(r *Rule, e *Endpoint, now time.Time) (map[string]FInfo, error) {
	out := map[string]FInfo{}
	if r.FileMode {
		fi, err := os.Lstat(filepath.Join(e.Dir, e.File))
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s — не обычный файл", e.File)
		}
		mt := fi.ModTime().UnixNano()
		out[fileKey] = FInfo{Rel: e.File, Size: fi.Size(), MTime: mt, Hot: isHot(mt, now)}
		return out, nil
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
		mt := info.ModTime().UnixNano()
		out[strings.ToLower(rel)] = FInfo{Rel: rel, Size: info.Size(), MTime: mt, Hot: isHot(mt, now)}
		return nil
	})
	return out, err
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

func (en *Engine) planTwoWay(r *Rule, st *State, scans []map[string]FInfo, joining []bool) []*job {
	n := len(scans)
	keySet := map[string]bool{}
	for _, m := range scans {
		for k := range m {
			keySet[k] = true
		}
	}
	for k := range st.Base {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var jobs []*job
	for _, key := range keys {
		cur := make([]*FInfo, n)
		hot := false
		for i := range scans {
			if fi, ok := scans[i][key]; ok {
				f := fi
				cur[i] = &f
				hot = hot || f.Hot
			}
		}
		if hot {
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
			j := &job{key: key, winner: -1}
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
		j := &job{key: key, winner: w}
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
	return jobs
}

func (en *Engine) planOneWay(r *Rule, scans []map[string]FInfo) []*job {
	m := r.Master
	var jobs []*job
	keys := make([]string, 0, len(scans[m]))
	for k := range scans[m] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		mi := scans[m][key]
		if mi.Hot {
			continue
		}
		j := &job{key: key, winner: m, noBase: true}
		for i := range scans {
			if i == m {
				continue
			}
			si, ok := scans[i][key]
			if ok && si.Hot {
				continue
			}
			if !ok || si.Size != mi.Size || absI64(si.MTime-mi.MTime) > tolNS {
				j.copies = append(j.copies, copyTo{dst: i})
			}
		}
		if len(j.copies) > 0 {
			jobs = append(jobs, j)
		}
	}
	if r.DeleteExtra {
		for i := range scans {
			if i == m {
				continue
			}
			var extra []string
			for k, si := range scans[i] {
				if _, ok := scans[m][k]; !ok && !si.Hot {
					extra = append(extra, k)
				}
			}
			sort.Strings(extra)
			for _, k := range extra {
				jobs = append(jobs, &job{key: k, winner: -1, deletes: []int{i}, noBase: true})
			}
		}
	}
	return jobs
}

// ---------- выполнение ----------

func conflictName(dst, label string, mt time.Time) string {
	dir, base := filepath.Split(dst)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, fmt.Sprintf("%s.sync-conflict-%s-%s%s", stem, mt.Format("20060102-150405"), label, ext))
}

func (en *Engine) execJob(r *Rule, st *State, scans []map[string]FInfo, j *job, ps *passStats) {
	rt := en.rt[r.Name]
	if j.askConflict {
		if !rt.notified[j.key] {
			rt.notified[j.key] = true
			en.logf(r.Name, "WARN", "конфликт (режим ask), файл не тронут: %s", j.key)
			en.notify(r, "Конфликт файлов", j.key+" — изменён в нескольких местах, нужен ручной выбор")
		}
		return
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
			if err := en.copyFile(r, src, wi, e, dst); err != nil {
				ok = false
				if errors.Is(err, errSourceChanged) {
					ps.pending++
					en.logf(r.Name, "INFO", "%s ещё меняется, повтор в следующем проходе", src)
				} else {
					ps.failed++
					en.logf(r.Name, "ERROR", "копирование %s -> %s: %v", src, dst, err)
				}
				continue
			}
			ps.copied++
		}
		for _, i := range j.touch {
			fi := scans[i][j.key]
			if !en.dry {
				mt := time.Unix(0, wi.MTime)
				_ = os.Chtimes(r.abs(r.Endpoints[i], fi.Rel), mt, mt)
			}
		}
	}
	for _, i := range j.deletes {
		fi := scans[i][j.key]
		p := r.abs(r.Endpoints[i], fi.Rel)
		en.logf(r.Name, "INFO", "удаляю %s", p)
		if !en.dry {
			if err := en.removeOld(r, r.Endpoints[i], p); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
			st.Base[j.key] = Base{Size: wi.Size, MTime: wi.MTime}
		} else {
			delete(st.Base, j.key)
		}
	}
}

var tmpCounter uint64

func (en *Engine) copyFile(r *Rule, srcPath string, wi FInfo, e *Endpoint, dstPath string) error {
	if en.dry {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dstPath), fmt.Sprintf(".lsx-tmp-%d-%d", os.Getpid(), atomic.AddUint64(&tmpCounter, 1)))
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
		if err := en.removeOld(r, e, dstPath); err != nil {
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

// removeOld: в корзину или удалить, в зависимости от TrashDays правила.
func (en *Engine) removeOld(r *Rule, e *Endpoint, p string) error {
	if r.TrashDays < 0 {
		return os.Remove(p)
	}
	rel := filepath.Base(p)
	if !r.FileMode {
		if x, err := filepath.Rel(e.Dir, p); err == nil {
			rel = x
		}
	}
	dir, base := filepath.Split(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(e.Dir, trashDir, dir, fmt.Sprintf("%s~%s%s", stem, stamp, ext))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = filepath.Join(e.Dir, trashDir, dir, fmt.Sprintf("%s~%s-%d%s", stem, stamp, i, ext))
	}
	return os.Rename(p, dst)
}

var trashStampRe = regexp.MustCompile(`~(\d{8}-\d{6})`)

func (en *Engine) cleanTrash(r *Rule) {
	cutoff := time.Now().Add(-time.Duration(r.TrashDays) * 24 * time.Hour)
	seen := map[string]bool{}
	for _, e := range r.Endpoints {
		root := filepath.Join(e.Dir, trashDir)
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
		for i := len(dirs) - 1; i > 0; i-- {
			_ = os.Remove(dirs[i]) // удалится, только если пуста
		}
	}
}
