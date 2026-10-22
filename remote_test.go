package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPathKindsConfig(t *testing.T) {
	root := t.TempDir()
	ini := "[A]\nremote=2\n{R}/a\n{R}/b\n[B]\ncloud=1\nmtimetol=5\ntimeout=30\n{R}/c\n{R}/d\n[C]\nwatch=0\n{R}/e\n{R}/f\n[D]\nremote=1\ncloud=1\n{R}/g\n{R}/h\n[E]\nremote=3\n{R}/i\n{R}/j\n[F]\nscan=0\ncloud=2\n{R}/k\n{R}/l\n[G]\ntimeout=3\n{R}/m\n{R}/n\n"
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini), 0o644)
	cfg, err := loadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Rules
	if r[0].kind(0) != "local" || r[0].kind(1) != "remote" || r[0].Scan != 120 {
		t.Fatalf("remote: %v %v scan=%d", r[0].kind(0), r[0].kind(1), r[0].Scan)
	}
	if r[1].kind(0) != "cloud" || r[1].Scan != 600 || r[1].MtimeTol != 5 || r[1].Timeout != 30 || r[1].mtimeReliable(0) {
		t.Fatalf("cloud: %+v", r[1])
	}
	if r[1].stall(0) != 30*time.Second || r[0].stall(1) != 60*time.Second || r[0].stall(0) != 0 {
		t.Fatal("таймауты по типам путей")
	}
	if r[2].Scan != 60 {
		t.Fatalf("локальное правило без слежения: scan=%d", r[2].Scan)
	}
	if r[3].Err == "" {
		t.Fatal("путь одновременно remote и cloud должен давать ошибку")
	}
	if r[4].Err == "" {
		t.Fatal("remote=3 при двух путях должен давать ошибку")
	}
	if r[6].Err == "" {
		t.Fatal("timeout=3 меньше допустимого")
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(strings.Join(cfg.Warnings, "\n"), "scan=0") {
		t.Fatalf("ожидалось предупреждение про scan=0: %v", cfg.Warnings)
	}
	// типы путей сохраняются при записи ini
	txt := cfg.Render()
	if !strings.Contains(txt, "Remote=2") || !strings.Contains(txt, "Cloud=1") || !strings.Contains(txt, "MtimeTol=5") {
		t.Fatalf("Render потерял настройки:\n%s", txt)
	}
}

func readCounters(en *Engine, r *Rule) (copied, conflicts int64) {
	rt := en.rtFor(r)
	rt.smu.Lock()
	defer rt.smu.Unlock()
	return rt.copied, rt.conflicts
}

// Облако (rclone mount): время изменения не сохраняется — изменения определяются по размеру.
func TestCloudDetectsBySize(t *testing.T) {
	en, cfg := setup(t, "[R1]\ncloud=2\nscan=0\nwatch=0\ndirectbelow=0\n{R}/local\n{R}/cloud\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "x.txt"), "hello", time.Minute)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(b, "x.txt")) != "hello" {
		t.Fatal("файл не скопирован в облако")
	}
	c0, _ := readCounters(en, r)
	// «облако» показало другое время изменения — это не изменение файла
	old := time.Now().Add(-10 * time.Hour)
	os.Chtimes(filepath.Join(b, "x.txt"), old, old)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if c1, _ := readCounters(en, r); c1 != c0 {
		t.Fatalf("смена только времени в облаке вызвала копирование (%d -> %d)", c0, c1)
	}
	if rd(t, filepath.Join(a, "x.txt")) != "hello" {
		t.Fatal("локальный файл изменился")
	}
	// изменился размер в облаке — это изменение
	wr(t, filepath.Join(b, "x.txt"), "hello world", 20*time.Hour)
	en.RunOnce()
	if rd(t, filepath.Join(a, "x.txt")) != "hello world" {
		t.Fatal("изменение размера в облаке не дошло до локального пути")
	}
	// конфликт: локальная версия (надёжное время) побеждает, облачная сохраняется копией
	wr(t, filepath.Join(a, "x.txt"), "LOCAL-edit-1", 5*time.Minute)
	wr(t, filepath.Join(b, "x.txt"), "CLOUD-edit-longer", 0) // время «новее», но облаку верить нельзя
	time.Sleep(2200 * time.Millisecond)                // файл с «текущим» временем сначала считается ещё пишущимся
	en.RunOnce()
	if rd(t, filepath.Join(a, "x.txt")) != "LOCAL-edit-1" || rd(t, filepath.Join(b, "x.txt")) != "LOCAL-edit-1" {
		t.Fatalf("при конфликте должна победить локальная версия: a=%q b=%q", rd(t, filepath.Join(a, "x.txt")), rd(t, filepath.Join(b, "x.txt")))
	}
	cf, _ := filepath.Glob(filepath.Join(b, "x.sync-conflict-*.txt"))
	if len(cf) != 1 || rd(t, cf[0]) != "CLOUD-edit-longer" {
		t.Fatalf("облачная версия должна сохраниться копией: %v", cf)
	}
}

// База помнит время, которое мы видели на каждом пути: если диск «переписывает» время, это не изменение.
func TestObservedMtimeInBase(t *testing.T) {
	_, cfg := setup(t, "[R1]\nscan=0\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	r.prepare()
	en := NewEngine(cfg, t.TempDir(), false, io.Discard)
	T := time.Now().Add(-24 * time.Hour).UnixNano()
	shifted := T + int64(time.Hour)
	scans := []map[string]FInfo{
		{"f": {Rel: "f", Size: 5, MTime: T}},
		{"f": {Rel: "f", Size: 5, MTime: shifted}},
	}
	st := &State{IDs: map[string]string{}, Vols: map[string]string{},
		Base: map[string]Base{"f": {Size: 5, MTime: T}}}
	if jobs, _ := en.planTwoWay(r, st, scans, []bool{false, false}, nil, time.Now()); len(jobs) != 1 || jobs[0].winner != 1 {
		t.Fatalf("без наблюдаемого времени сдвиг на час — изменение: %+v", jobs)
	}
	st.Base["f"] = Base{Size: 5, MTime: T, Obs: map[string]int64{epID(r.Endpoints[1]): shifted}}
	if jobs, _ := en.planTwoWay(r, st, scans, []bool{false, false}, nil, time.Now()); len(jobs) != 0 {
		t.Fatalf("наблюдаемое время совпало с базой — изменений нет: %+v", jobs)
	}
	scans[1]["f"] = FInfo{Rel: "f", Size: 5, MTime: shifted + int64(time.Minute)}
	if jobs, _ := en.planTwoWay(r, st, scans, []bool{false, false}, nil, time.Now()); len(jobs) != 1 {
		t.Fatal("новое изменение времени должно обнаруживаться")
	}
}

func TestCaseCollisionSkipped(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "ok.txt"), "ok", time.Minute)
	wr(t, filepath.Join(a, "Doc.txt"), "upper", time.Minute)
	wr(t, filepath.Join(a, "doc.txt"), "lower", time.Minute)
	ents, _ := os.ReadDir(a)
	if len(ents) != 3 {
		t.Skip("файловая система не различает регистр")
	}
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(b, "ok.txt")) != "ok" {
		t.Fatal("обычный файл должен копироваться")
	}
	if exists(filepath.Join(b, "Doc.txt")) || exists(filepath.Join(b, "doc.txt")) {
		t.Fatal("файлы, отличающиеся только регистром, копироваться не должны")
	}
	if !en.rtFor(r).notified["case:doc.txt"] {
		t.Fatal("о пропуске должно быть предупреждение")
	}
	if !exists(filepath.Join(a, "Doc.txt")) || !exists(filepath.Join(a, "doc.txt")) {
		t.Fatal("исходные файлы затрагиваться не должны")
	}
}

func TestGuardsAgainstHang(t *testing.T) {
	start := time.Now()
	err := guardOp(80*time.Millisecond, func() error { time.Sleep(3 * time.Second); return nil })
	if err != errStalled || time.Since(start) > time.Second {
		t.Fatalf("guardOp: %v за %v", err, time.Since(start))
	}
	if err := guardOp(time.Second, func() error { return nil }); err != nil {
		t.Fatal("быстрая операция должна завершаться без ошибки")
	}
	// копирование, из которого не приходят данные
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	dst, _ := os.CreateTemp(t.TempDir(), "x")
	start = time.Now()
	if err := copyStall(dst, pr, 500*time.Millisecond); err != errStalled {
		t.Fatalf("copyStall должен прерваться: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("прерывание заняло слишком долго: %v", time.Since(start))
	}
	// обычное копирование работает
	srcF := filepath.Join(t.TempDir(), "s")
	data := bytes.Repeat([]byte("abc"), 400000)
	os.WriteFile(srcF, data, 0o644)
	in, _ := os.Open(srcF)
	out, _ := os.Create(filepath.Join(t.TempDir(), "d"))
	if err := copyStall(out, in, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out.Name())
	if !bytes.Equal(got, data) {
		t.Fatal("данные при копировании искажены")
	}
}
