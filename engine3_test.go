package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrashInWorkDir(t *testing.T) {
	en, cfg := setup(t, "[Settings]\nworkdir={R}/work\n[R1]\nscan=0\nwatch=0\ntrash=work\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "x.txt"), "x", time.Minute)
	en.RunOnce()
	os.Remove(filepath.Join(a, "x.txt"))
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if exists(filepath.Join(b, "x.txt")) || exists(filepath.Join(b, trashDir)) || exists(filepath.Join(a, trashDir)) {
		t.Fatal("корзина не должна появляться в синхронизируемых папках")
	}
	found := 0
	filepath.WalkDir(filepath.Join(filepath.Dir(a), "work", "trash"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "x~") {
			found++
		}
		return nil
	})
	if found != 1 {
		t.Fatalf("в корзине рабочей папки ожидался 1 файл, найдено %d", found)
	}
}

func TestLargeFileViaTemp(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\ndirectbelow=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	big := bytes.Repeat([]byte("0123456789abcdef"), 200000) // ~3 МБ
	os.MkdirAll(a, 0o755)
	os.WriteFile(filepath.Join(a, "big.bin"), big, 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(filepath.Join(a, "big.bin"), old, old)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	got, err := os.ReadFile(filepath.Join(b, "big.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatal("большой файл скопирован неверно")
	}
	// обновление: старая версия уходит в корзину, временных файлов не остаётся
	os.WriteFile(filepath.Join(a, "big.bin"), append(big, 'Z'), 0o644)
	n := time.Now().Add(-30 * time.Second)
	os.Chtimes(filepath.Join(a, "big.bin"), n, n)
	en.RunOnce()
	if sz := fileSize(filepath.Join(b, "big.bin")); sz != int64(len(big)+1) {
		t.Fatalf("размер после обновления %d", sz)
	}
	left, _ := filepath.Glob(filepath.Join(b, "LocalSyncExt-*"))
	if len(left) != 0 {
		t.Fatalf("остались временные файлы: %v", left)
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func TestDirectOverwriteKeepsOldInTrash(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "s.txt"), "v1", time.Hour)
	en.RunOnce()
	wr(t, filepath.Join(a, "s.txt"), "v2", time.Minute)
	en.RunOnce()
	if rd(t, filepath.Join(b, "s.txt")) != "v2" {
		t.Fatal("новая версия не записана")
	}
	ents, _ := os.ReadDir(filepath.Join(b, trashDir))
	if len(ents) != 1 {
		t.Fatalf("прежняя версия должна лежать в корзине: %v", ents)
	}
}

func TestStaleTempCleanup(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	staleP := filepath.Join(a, "LocalSyncExt-20200101-000000-x.txt")
	wr(t, staleP, "stale", 72*time.Hour)
	freshP := filepath.Join(a, "LocalSyncExt-20261012-120000-y.txt")
	wr(t, freshP, "fresh", time.Minute)
	userP := filepath.Join(a, "LocalSyncExt-notes.txt")
	wr(t, userP, "notes", time.Minute)
	en.RunOnce()
	if exists(staleP) {
		t.Fatal("старый временный файл должен быть удалён")
	}
	if !exists(freshP) || exists(filepath.Join(b, "LocalSyncExt-20261012-120000-y.txt")) {
		t.Fatal("свежий временный файл не должен ни удаляться, ни синхронизироваться")
	}
	if rd(t, filepath.Join(b, "LocalSyncExt-notes.txt")) != "notes" {
		t.Fatal("обычный файл с похожим именем должен синхронизироваться")
	}
}

func TestWorkDirInsideEndpoint(t *testing.T) {
	root := t.TempDir()
	ini := "[Settings]\nworkdir={R}/a/work\n[R1]\ntrash=work\n{R}/a\n{R}/b\n"
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini), 0o644)
	cfg, _ := ParseConfig(f)
	cfg.Validate()
	if cfg.Rules[0].Err == "" {
		t.Fatal("рабочая папка внутри синхронизируемой должна давать ошибку")
	}
}

func TestMoveFallbackCopy(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "s.txt"), filepath.Join(d, "d.txt")
	wr(t, src, "data", time.Hour)
	fi, _ := os.Stat(src)
	if err := copyThenRemove(src, dst, fi); err != nil {
		t.Fatal(err)
	}
	if exists(src) || rd(t, dst) != "data" {
		t.Fatal("перенос копированием работает неверно")
	}
	di, _ := os.Stat(dst)
	if !di.ModTime().Equal(fi.ModTime()) {
		t.Fatal("время изменения должно сохраняться")
	}
}
