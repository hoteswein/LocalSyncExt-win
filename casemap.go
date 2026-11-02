package main

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Метки для имён, различающихся только регистром (Linux), при копировании на Windows (без учёта регистра):
//
//	file.ext, File.ext                ->  file.ext, File[linux_case].ext
//	another_file.ext, another_file.Ext ->  another_file.ext, another_file.Ext.ext
//
// Имя «по-настоящему» остаётся только у основного файла группы: в нижнем регистре, а если такого нет —
// первого по порядку байт. Остальные получают метку. Windows считает File[linux_case].ext и
// FILE[linux_case].ext одним именем, поэтому у второго и следующих «двойников» метка нумеруется:
// [linux_case], [linux_case2], [linux_case3]... Обратно метки распознаются и убираются.
const caseMark = "[linux_case"

var markRe = regexp.MustCompile(`\[linux_case(\d*)\]$`)

func splitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// decodeFileName превращает имя с меткой на Windows в настоящее (Linux) имя.
func decodeFileName(phys string) string {
	stem, ext := splitName(phys)
	if loc := markRe.FindStringIndex(stem); loc != nil {
		return stem[:loc[0]] + ext
	}
	if ext != "" {
		if _, ext2 := splitName(stem); ext2 != "" && ext2 != ext && strings.EqualFold(ext2, ext) {
			return stem // another_file.Ext.ext -> another_file.Ext
		}
	}
	return phys
}

func chooseBase(names []string) string {
	best := ""
	for _, n := range names {
		if n == strings.ToLower(n) {
			return n
		}
		if best == "" || n < best {
			best = n
		}
	}
	return best
}

func upperCount(s string) int {
	n := 0
	for _, r := range s {
		if r != unicode.ToLower(r) {
			n++
		}
	}
	return n
}

func markN(k int) string {
	if k <= 1 {
		return caseMark + "]"
	}
	return caseMark + strconv.Itoa(k) + "]"
}

// encodeGroup задаёт имена на Windows для группы имён, совпадающих без учёта регистра.
func encodeGroup(names []string) (enc map[string]string, skipped []string) {
	base := chooseBase(names)
	enc = map[string]string{base: base}
	used := map[string]bool{strings.ToLower(base): true}
	others := make([]string, 0, len(names))
	for _, n := range names {
		if n != base {
			others = append(others, n)
		}
	}
	sort.Slice(others, func(i, j int) bool {
		ui, uj := upperCount(others[i]), upperCount(others[j])
		if ui != uj {
			return ui < uj
		}
		return others[i] < others[j]
	})
	bstem, bext := splitName(base)
	nextMark := 1
	for _, n := range others {
		stem, ext := splitName(n)
		cand := ""
		if stem == bstem && ext != bext && bext != "" {
			cand = n + bext // только расширение отличается регистром
		}
		if cand == "" || used[strings.ToLower(cand)] {
			cand = stem + markN(nextMark) + ext
			nextMark++
		}
		if used[strings.ToLower(cand)] {
			skipped = append(skipped, n)
			continue
		}
		used[strings.ToLower(cand)] = true
		enc[n] = cand
	}
	return
}

// ---------- режим регистра правила ----------

// detectCaseMode: fold — все пути без учёта регистра (Windows); exact — все с учётом (Linux);
// mapped — смесь: имена на стороне Windows кодируются метками.
func (r *Rule) detectCaseMode() {
	sens, insens := false, false
	for _, e := range r.Endpoints {
		if e.FS == nil {
			continue
		}
		if e.FS.CaseSensitive() {
			sens = true
		} else {
			insens = true
		}
	}
	switch {
	case r.FileMode:
		r.caseMode = "fold"
	case sens && insens:
		r.caseMode = "mapped"
	case sens:
		r.caseMode = "exact"
	default:
		r.caseMode = "fold"
	}
}

// keyOf — ключ файла по его настоящему (логическому) относительному пути.
func (r *Rule) keyOf(rel string) string {
	switch r.caseMode {
	case "exact":
		return rel
	case "mapped":
		dir, name := splitRel(rel)
		if dir == "" {
			return name
		}
		return strings.ToLower(dir) + "/" + name
	}
	return strings.ToLower(rel)
}

func (r *Rule) dirKey(rel string) string {
	if r.caseMode == "exact" {
		return rel
	}
	return strings.ToLower(rel)
}

// logical — настоящий (Linux) относительный путь файла по его имени на пути №i.
func (r *Rule) logical(i int, phys string) string {
	if r.caseMode != "mapped" || r.Mode == "backup" || r.Endpoints[i].FS.CaseSensitive() {
		return phys
	}
	dir, name := splitRel(phys)
	return joinRel(dir, decodeFileName(name))
}

type caseGroup struct {
	names []string
	enc   map[string]string
}

type caseIndex map[string]map[string]*caseGroup // папка (ключ) -> имя без регистра -> группа

func (r *Rule) buildCaseIndex(files []map[string]FInfo) caseIndex {
	idx := caseIndex{}
	for _, m := range files {
		for key := range m {
			dir, name := splitRel(key)
			d := idx[dir]
			if d == nil {
				d = map[string]*caseGroup{}
				idx[dir] = d
			}
			fold := strings.ToLower(name)
			g := d[fold]
			if g == nil {
				g = &caseGroup{}
				d[fold] = g
			}
			found := false
			for _, n := range g.names {
				if n == name {
					found = true
					break
				}
			}
			if !found {
				g.names = append(g.names, name)
			}
		}
	}
	return idx
}

// physName — имя файла на стороне Windows для ключа.
func (ci caseIndex) physName(key string) string {
	dir, name := splitRel(key)
	g := ci[dir][strings.ToLower(name)]
	if g == nil || len(g.names) < 2 {
		return name
	}
	if g.enc == nil {
		g.enc, _ = encodeGroup(g.names)
	}
	if e, ok := g.enc[name]; ok {
		return e
	}
	return name
}

// relFor — относительный путь, по которому файл с этим ключом лежит (или должен лежать) на пути №i.
func (r *Rule) relFor(rt *ruleRT, i int, key, logRel string) string {
	if r.caseMode == "fold" || r.caseMode == "" {
		return logRel
	}
	dir, name := splitRel(logRel)
	if rt != nil && rt.cache != nil {
		if d, ok := rt.cache[i].dirs[r.dirKey(dir)]; ok {
			dir = d // папка на этом пути уже есть — используем её написание
		}
	}
	if r.caseMode == "mapped" && !r.Endpoints[i].FS.CaseSensitive() && r.Mode != "backup" && rt != nil && rt.caseIdx != nil {
		name = rt.caseIdx.physName(key)
	}
	return joinRel(dir, name)
}

// reconcileNames приводит имена файлов на стороне Windows к схеме меток: переименовывает файлы,
// чьё имя изменилось из-за появления или исчезновения «двойников» на Linux-стороне.
func (en *Engine) reconcileNames(r *Rule, rt *ruleRT) {
	if r.caseMode != "mapped" || en.dry || r.Mode == "backup" {
		return
	}
	files := make([]map[string]FInfo, len(rt.cache))
	for i := range files {
		files[i] = rt.cache[i].files
	}
	rt.caseIdx = r.buildCaseIndex(files)
	now := time.Now()
	for i, e := range r.Endpoints {
		if e.FS.CaseSensitive() {
			continue
		}
		keys := make([]string, 0, len(rt.cache[i].files))
		for k := range rt.cache[i].files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fi := rt.cache[i].files[key]
			if isHot(fi.MTime, now) {
				continue
			}
			dir, _ := splitRel(fi.Rel)
			want := joinRel(dir, rt.caseIdx.physName(key))
			if want == fi.Rel {
				continue
			}
			oldAbs, newAbs := r.abs(e, fi.Rel), r.abs(e, want)
			if !strings.EqualFold(fi.Rel, want) {
				if _, err := e.FS.Lstat(newAbs); err == nil {
					en.logf(r.Name, "WARN", "не удалось переименовать %s в %s: имя занято", fi.Rel, want)
					continue
				} else if !errors.Is(err, fs.ErrNotExist) {
					continue
				}
			}
			err := r.op(i, func() error { return e.FS.Rename(oldAbs, newAbs) })
			if err != nil {
				en.logf(r.Name, "WARN", "не удалось переименовать %s в %s: %v", fi.Rel, want, err)
				continue
			}
			en.logf(r.Name, "INFO", "имя по схеме [linux_case]: %s -> %s", fi.Rel, want)
			fi.Rel = want
			rt.cache[i].files[key] = fi
		}
	}
}

var _ = fmt.Sprintf
