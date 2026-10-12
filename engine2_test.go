package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Инкрементальная обработка событий (без настоящего наблюдателя: события подаём вручную).
func TestIncrementalEvents(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "old.txt"), "old", time.Minute)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	rt := en.rtFor(r)
	ev := func(ep int, rel string) []rawEv { return []rawEv{{ep, rel}} }

	// новый файл
	wr(t, filepath.Join(a, "new.txt"), "new", time.Minute)
	if _, err := en.pass(r, rt, false, ev(0, "new.txt"), true); err != nil {
		t.Fatal(err)
	}
	if rd(t, filepath.Join(b, "new.txt")) != "new" {
		t.Fatal("новый файл не скопирован по событию")
	}
	// удаление файла
	os.Remove(filepath.Join(a, "old.txt"))
	if _, err := en.pass(r, rt, false, ev(0, "old.txt"), true); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(b, "old.txt")) {
		t.Fatal("удаление не разошлось по событию")
	}
	// новая папка с файлами одним событием
	wr(t, filepath.Join(a, "d", "x.txt"), "x", time.Minute)
	wr(t, filepath.Join(a, "d", "deep", "y.txt"), "y", time.Minute)
	if _, err := en.pass(r, rt, false, ev(0, "d"), true); err != nil {
		t.Fatal(err)
	}
	if rd(t, filepath.Join(b, "d", "x.txt")) != "x" || rd(t, filepath.Join(b, "d", "deep", "y.txt")) != "y" {
		t.Fatal("файлы новой папки не скопированы")
	}
	// удаление всей папки одним событием
	os.RemoveAll(filepath.Join(a, "d"))
	if _, err := en.pass(r, rt, false, ev(0, "d"), true); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(b, "d", "x.txt")) || exists(filepath.Join(b, "d", "deep", "y.txt")) {
		t.Fatal("удаление папки не разошлось")
	}
	// служебные файлы игнорируются
	if _, err := en.pass(r, rt, false, ev(0, trashDir+"/q.txt"), true); err != nil {
		t.Fatal(err)
	}
	// свежий файл откладывается и копируется при повторе
	wr(t, filepath.Join(a, "hot.txt"), "hot", 0)
	retry, err := en.pass(r, rt, false, ev(0, "hot.txt"), true)
	if err != nil || retry <= 0 {
		t.Fatalf("ожидался повтор для свежего файла: %v %v", retry, err)
	}
	if exists(filepath.Join(b, "hot.txt")) {
		t.Fatal("свежий файл скопирован слишком рано")
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := en.pass(r, rt, false, nil, true); err != nil {
		t.Fatal(err)
	}
	if rd(t, filepath.Join(b, "hot.txt")) != "hot" {
		t.Fatal("отложенный файл не скопирован при повторе")
	}
}

func markers(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, ".lsx-*.marker"))
	return m
}

func TestMarkerOff(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\nnomarker=2\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	for i := 0; i < 3; i++ {
		wr(t, filepath.Join(a, string(rune('a'+i))+".txt"), "d", time.Minute)
	}
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if len(markers(a)) != 1 || len(markers(b)) != 0 {
		t.Fatalf("маркеры: a=%v b=%v", markers(a), markers(b))
	}
	ents, _ := os.ReadDir(b)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("в папке без маркера появился служебный файл %s", e.Name())
		}
	}
	// исчезновение папки без маркера всё равно защищено по состоянию
	os.RemoveAll(b)
	if en.RunOnce() {
		t.Fatal("ожидалась ошибка: папка b недоступна")
	}
	if !exists(filepath.Join(a, "a.txt")) {
		t.Fatal("файлы потеряны")
	}
}

func TestMarkerAllOff(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\nmarker=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	wr(t, filepath.Join(r.Endpoints[0].Raw, "x.txt"), "x", time.Minute)
	en.RunOnce()
	if len(markers(r.Endpoints[0].Raw))+len(markers(r.Endpoints[1].Raw)) != 0 {
		t.Fatal("при marker=0 маркеров быть не должно")
	}
}

func TestConfigDefaults(t *testing.T) {
	_, cfg := setup(t, "[W]\n{R}/a\n{R}/b\n[P]\nwatch=0\n{R}/c\n{R}/d\n[S]\nscan=30\n{R}/e\n{R}/f\n")
	if cfg.Rules[0].Scan != 900 || cfg.Rules[1].Scan != 60 || cfg.Rules[2].Scan != 30 {
		t.Fatalf("scan по умолчанию: %d %d %d", cfg.Rules[0].Scan, cfg.Rules[1].Scan, cfg.Rules[2].Scan)
	}
}

// Перезагрузка конфига на лету: добавляем правило, пока программа работает.
func TestHotReload(t *testing.T) {
	cfgPoll = 100 * time.Millisecond
	root := t.TempDir()
	ini := func(extra string) string {
		s := "[R1]\nscan=1\nwatch=0\n{R}/a\n{R}/b\n" + extra
		return strings.ReplaceAll(s, "{R}", filepath.ToSlash(root))
	}
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini("")), 0o644)
	cfg, err := loadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	en := NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	en.Start(ctx)
	go en.WatchConfig(ctx, f, loadConfig)
	defer func() { cancel(); en.Stop() }()

	wr(t, filepath.Join(root, "a", "one.txt"), "1", time.Minute)
	waitFor(t, 8*time.Second, func() bool { return exists(filepath.Join(root, "b", "one.txt")) }, "первое правило")

	wr(t, filepath.Join(root, "c", "two.txt"), "2", time.Minute)
	time.Sleep(1200 * time.Millisecond) // чтобы изменился mtime ini
	os.WriteFile(f, []byte(ini("[R2]\nscan=1\nwatch=0\n{R}/c\n{R}/d\n")), 0o644)
	waitFor(t, 10*time.Second, func() bool { return exists(filepath.Join(root, "d", "two.txt")) }, "новое правило после правки ini")

	// битый конфиг не ломает работу
	time.Sleep(1200 * time.Millisecond)
	os.WriteFile(f, []byte("[R1]\nmode=bogus\n{R}/a\n{R}/b\n"), 0o644)
	time.Sleep(600 * time.Millisecond)
	wr(t, filepath.Join(root, "a", "three.txt"), "3", time.Minute)
	waitFor(t, 8*time.Second, func() bool { return exists(filepath.Join(root, "b", "three.txt")) }, "работа после битого конфига")
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}
