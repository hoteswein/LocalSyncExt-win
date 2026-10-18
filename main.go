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
	"time"
)

var version = "dev"

// guiBuild = "1" в сборке без консольного окна (-H=windowsgui): по умолчанию работает значок в трее.
var guiBuild = ""

func loadConfig(path string) (*Config, error) {
	cfg, err := ParseConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.Validate()
	return cfg, nil
}

// tee пишет в обе цели и не падает, если первой (консоли) нет.
type tee struct{ console, file io.Writer }

func (t tee) Write(p []byte) (int, error) {
	_, _ = t.console.Write(p)
	return t.file.Write(p)
}

func main() {
	cfgPath := flag.String("config", "", "путь к ini (по умолчанию LocalSyncExt.ini рядом с exe)")
	once := flag.Bool("once", false, "один проход и выход")
	dry := flag.Bool("dry-run", false, "только показать, что будет сделано")
	check := flag.Bool("check", false, "проверить конфиг и выйти")
	showVer := flag.Bool("version", false, "версия")
	trayFlag := flag.Bool("tray", false, "работать со значком в трее (в версии без консоли включено всегда)")
	webUser := flag.String("web-user", "", "задать логин веб-интерфейса (вместе с -web-pass) и выйти")
	webPass := flag.String("web-pass", "", "задать пароль веб-интерфейса (вместе с -web-user) и выйти")
	flag.Parse()

	isGUI := guiBuild == "1"
	setCreds := *webUser != "" || *webPass != ""
	oneShot := *once || *check || *showVer || setCreds
	if isGUI && oneShot {
		attachConsole() // версия без консоли: вывод в окно, из которого запущена
	}
	if *showVer {
		fmt.Println("LocalSyncExt", version)
		return
	}
	useTray := (isGUI || *trayFlag) && !oneShot

	fatal := func(code int, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, msg)
		if isGUI && !oneShot {
			showError(msg)
		}
		os.Exit(code)
	}

	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	if *cfgPath == "" {
		*cfgPath = filepath.Join(exeDir, "LocalSyncExt.ini")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal(2, "Не удалось прочитать конфиг %s:\n%v", *cfgPath, err)
	}

	if !oneShot {
		ok, release := acquireInstance(*cfgPath)
		if !ok {
			fatal(0, "LocalSyncExt уже запущен с этим конфигом.")
		}
		defer release()
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
		w = tee{os.Stderr, f}
	}

	stateDir := cfg.Settings["statedir"]
	if stateDir == "" {
		stateDir = filepath.Join(exeDir, "state")
	}
	if setCreds {
		if err := loadCreds(stateDir).Set(*webUser, *webPass); err != nil {
			fmt.Fprintln(os.Stderr, "не удалось задать логин и пароль:", err)
			os.Exit(1)
		}
		fmt.Println("логин и пароль веб-интерфейса сохранены")
		return
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

	// Уведомления: [Settings] Notifications=1|0, переключатель в меню трея запоминается в state\ui.json.
	nt := newNotifier(5*time.Second, trayBalloon)
	notifOn := true
	if b, ok := parseBool(cfg.Settings["notifications"]); ok {
		notifOn = b
	}
	if u := loadUIState(stateDir); u.Notifications != nil {
		notifOn = *u.Notifications
	}
	nt.enabled.Store(notifOn && useTray)
	en.OnNotify = nt.Push

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reload := func() {
		c, err := loadConfig(*cfgPath)
		if err != nil {
			en.logf("config", "ERROR", "не удалось перечитать конфиг: %v", err)
			return
		}
		en.logf("config", "INFO", "конфиг перечитан")
		en.report(c)
		en.Apply(c)
	}
	web := NewWebServer(en, *cfgPath, logPath, stateDir, version, reload)
	en.OnConfig = func(c *Config) {
		wc, errs := parseWebCfg(c.Settings)
		for _, e := range errs {
			en.logf("web", "ERROR", "%s", e)
		}
		web.Apply(wc)
	}
	defer web.Stop()
	en.logf("main", "INFO", "LocalSyncExt %s запущен, правил: %d. Изменения %s применяются на лету", version, len(cfg.Rules), filepath.Base(*cfgPath))
	en.Start(ctx)
	go en.WatchConfig(ctx, *cfgPath, loadConfig)

	if useTray {
		tc := &trayCtx{
			en: en, version: version, logPath: logPath, iniPath: *cfgPath, stateDir: stateDir, exePath: exe,
			click:    parseAction(cfg.Settings["trayclick"], actStatus),
			dblClick: parseAction(cfg.Settings["traydoubleclick"], actUI),
			notif:    nt,
			quit:     stop,
			webURL:   web.URL,
			reload:   reload,
		}
		go func() {
			<-ctx.Done()
			stopTray()
		}()
		if err := runTray(tc); err != nil {
			en.logf("tray", "ERROR", "значок в трее не запущен: %v", err)
			if isGUI {
				stop()
				en.Stop()
				fatal(1, "Не удалось создать значок в трее: %v", err)
			}
			<-ctx.Done() // консольная версия продолжает работать без значка
		}
		stop()
	} else {
		<-ctx.Done()
	}
	en.Stop()
	en.logf("main", "INFO", "остановлен")
}
