//go:build windows

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

func TestWatchDirEvents(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 64)
	go watchDir(ctx, dir, true, func() {}, func(rel string, overflow bool) { got <- rel })
	time.Sleep(300 * time.Millisecond)
	os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "deep", "f.txt"), []byte("x"), 0o644)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case rel := <-got:
			if strings.HasSuffix(rel, "f.txt") {
				return
			}
		case <-deadline:
			t.Fatal("событие о файле в подпапке не получено")
		}
	}
}

// Сквозной тест: настоящий наблюдатель, без таймера (scan=0).
func TestEndToEndWatcher(t *testing.T) {
	root := t.TempDir()
	ini := "[W]\nscan=0\ndebounce=200\n{R}/a\n{R}/b\n"
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini), 0o644)
	cfg, err := loadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	en := NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	en.Start(ctx)
	defer func() { cancel(); en.Stop() }()
	time.Sleep(1500 * time.Millisecond) // начальный проход и запуск наблюдателя

	os.WriteFile(filepath.Join(root, "a", "live.txt"), []byte("live"), 0o644)
	waitFor(t, 15*time.Second, func() bool { return exists(filepath.Join(root, "b", "live.txt")) }, "копирование по событию")

	os.MkdirAll(filepath.Join(root, "b", "dir", "in"), 0o755)
	os.WriteFile(filepath.Join(root, "b", "dir", "in", "z.txt"), []byte("z"), 0o644)
	waitFor(t, 15*time.Second, func() bool { return exists(filepath.Join(root, "a", "dir", "in", "z.txt")) }, "копирование новой папки обратно")

	os.Remove(filepath.Join(root, "a", "live.txt"))
	waitFor(t, 15*time.Second, func() bool { return !exists(filepath.Join(root, "b", "live.txt")) }, "удаление по событию")
}
