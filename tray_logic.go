package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Действия на клик по значку (настраиваются в [Settings]: TrayClick, TrayDoubleClick).
const (
	actNone    = "none"
	actStatus  = "status"  // показать сводку по правилам
	actSyncNow = "syncnow" // синхронизировать всё сейчас
	actPause   = "pause"   // пауза / продолжить всё
	actLog     = "log"     // открыть журнал
	actConfig  = "config"  // открыть ini
	actNotify  = "notify"  // включить / выключить уведомления
)

func parseAction(v, def string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case actNone, actStatus, actSyncNow, actPause, actLog, actConfig, actNotify:
		return v
	}
	return def
}

type trayState int

const (
	stIdle trayState = iota
	stBusy
	stError
	stPaused
)

func computeState(busy, allPaused bool, st []RuleStatus) trayState {
	if busy {
		return stBusy
	}
	for _, s := range st {
		if s.Status == "error" || s.Status == "config-error" {
			return stError
		}
	}
	if allPaused {
		return stPaused
	}
	return stIdle
}

func statusRU(s string) string {
	switch s {
	case "ok":
		return "ок"
	case "syncing":
		return "синхронизация"
	case "error":
		return "ошибка"
	case "paused":
		return "пауза"
	case "disabled":
		return "выключено"
	case "config-error":
		return "ошибка конфигурации"
	}
	return "ожидание"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func summaryText(st []RuleStatus) string {
	if len(st) == 0 {
		return "Правил нет"
	}
	parts := make([]string, 0, len(st))
	for _, s := range st {
		parts = append(parts, fmt.Sprintf("%s — %s", s.Name, statusRU(s.Status)))
	}
	return clip(strings.Join(parts, "\n"), 250)
}

func tooltipText(version string, st []RuleStatus, state trayState) string {
	errs := 0
	for _, s := range st {
		if s.Status == "error" || s.Status == "config-error" {
			errs++
		}
	}
	line := fmt.Sprintf("правил: %d", len(st))
	switch {
	case state == stBusy:
		line += ", синхронизация…"
	case errs > 0:
		line += fmt.Sprintf(", ошибок: %d", errs)
	case state == stPaused:
		line += ", пауза"
	default:
		line += ", всё в порядке"
	}
	return clip("LocalSyncExt "+version+"\n"+line, 127)
}

// ---------- уведомления с ограничением частоты ----------

const (
	levelInfo  = 1
	levelWarn  = 2
	levelError = 3
)

func levelFor(title string) int {
	switch {
	case strings.HasPrefix(title, "Ошибка"):
		return levelError
	case strings.HasPrefix(title, "Конфликт"):
		return levelWarn
	}
	return levelInfo
}

type notifier struct {
	enabled  atomic.Bool
	show     func(title, msg string, level int)
	cooldown time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	pending []string
}

func newNotifier(cooldown time.Duration, show func(title, msg string, level int)) *notifier {
	return &notifier{show: show, cooldown: cooldown}
}

// Push показывает уведомление сразу, а пока идёт пауза между показами, копит и выводит одной сводкой.
func (n *notifier) Push(rule, title, msg string) {
	if !n.enabled.Load() {
		return
	}
	text := "[" + rule + "] " + msg
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.timer != nil {
		n.pending = append(n.pending, text)
		return
	}
	n.show("LocalSyncExt — "+title, clip(text, 250), levelFor(title))
	n.timer = time.AfterFunc(n.cooldown, n.flush)
}

func (n *notifier) flush() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.pending) == 0 {
		n.timer = nil
		return
	}
	last := n.pending[len(n.pending)-1]
	msg := fmt.Sprintf("Ещё уведомлений: %d. Последнее: %s", len(n.pending), last)
	n.pending = nil
	n.show("LocalSyncExt", clip(msg, 250), levelInfo)
	n.timer = time.AfterFunc(n.cooldown, n.flush)
}

// ---------- переключатели, сохраняемые между запусками ----------

type uiState struct {
	Notifications *bool `json:"notifications,omitempty"`
}

func uiStatePath(stateDir string) string { return filepath.Join(stateDir, "ui.json") }

func loadUIState(stateDir string) uiState {
	var u uiState
	if b, err := os.ReadFile(uiStatePath(stateDir)); err == nil {
		_ = json.Unmarshal(b, &u)
	}
	return u
}

func saveUIState(stateDir string, u uiState) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	if b, err := json.Marshal(u); err == nil {
		_ = os.WriteFile(uiStatePath(stateDir), b, 0o644)
	}
}

// trayCtx — всё, что нужно значку в трее от остальной программы.
type trayCtx struct {
	en       *Engine
	version  string
	logPath  string
	iniPath  string
	stateDir string
	exePath  string
	click    string
	dblClick string
	notif    *notifier
	reload   func()
	quit     func()
}
