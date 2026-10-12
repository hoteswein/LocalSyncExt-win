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

func loadConfig(path string) (*Config, error) {
	cfg, err := ParseConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.Validate()
	return cfg, nil
}

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
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось прочитать конфиг:", err)
		os.Exit(2)
	}

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
	bad := en.report(cfg)
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
	en.logf("main", "INFO", "LocalSyncExt %s запущен, правил: %d. Изменения %s применяются на лету", version, len(cfg.Rules), filepath.Base(*cfgPath))
	en.Start(ctx)
	go en.WatchConfig(ctx, *cfgPath, loadConfig)
	<-ctx.Done()
	en.Stop()
	en.logf("main", "INFO", "остановлен")
}
