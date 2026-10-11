package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "путь к ini (по умолчанию LocalSyncExt.ini рядом с exe)")
	once := flag.Bool("once", false, "один проход и выход")
	dry := flag.Bool("dry-run", false, "только показать, что будет сделано")
	check := flag.Bool("check", false, "проверить конфиг и выйти")
	showVer := flag.Bool("version", false, "версия")
	flag.Parse()
	if *showVer {
		fmt.Println("LocalSyncExt", version)
		return
	}

	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	if *cfgPath == "" {
		*cfgPath = filepath.Join(exeDir, "LocalSyncExt.ini")
	}
	cfg, err := ParseConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось прочитать конфиг:", err)
		os.Exit(2)
	}
	cfg.Validate()

	var w io.Writer = os.Stderr
	logPath := cfg.Settings["logfile"]
	if logPath == "" {
		logPath = filepath.Join(exeDir, "LocalSyncExt.log")
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(logPath, logPath+".old")
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		defer f.Close()
		w = io.MultiWriter(os.Stderr, f)
	}

	stateDir := cfg.Settings["statedir"]
	if stateDir == "" {
		stateDir = filepath.Join(exeDir, "state")
	}
	en := NewEngine(cfg, stateDir, *dry, w)
	for _, s := range cfg.Warnings {
		en.logf("config", "WARN", "%s", s)
	}
	bad := false
	for _, r := range cfg.Rules {
		if r.Err != "" {
			en.logf(r.Name, "ERROR", "ошибка конфигурации: %s", r.Err)
			bad = true
		}
	}
	if *check {
		if bad {
			os.Exit(1)
		}
		fmt.Printf("конфиг в порядке: правил %d\n", len(cfg.Rules))
		return
	}
	if *once {
		ok := en.RunOnce()
		if !ok || bad {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	en.logf("main", "INFO", "LocalSyncExt %s запущен, правил: %d", version, len(cfg.Rules))
	en.Run(ctx)
	en.logf("main", "INFO", "остановлен")
}
