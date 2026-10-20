package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIconImageFormat(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	img.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 200}) // верхний левый
	img.SetNRGBA(0, 15, color.NRGBA{R: 1, G: 2, B: 3, A: 255})   // нижний левый
	var buf bytes.Buffer
	png.Encode(&buf, img)
	data, err := iconImage(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if want := 40 + 16*16*4 + ((16+31)/32*4)*16; len(data) != want {
		t.Fatalf("размер данных %d, ожидалось %d", len(data), want)
	}
	var hdr struct {
		Size         uint32
		W, H         int32
		Planes, Bits uint16
	}
	binary.Read(bytes.NewReader(data), binary.LittleEndian, &hdr)
	if hdr.Size != 40 || hdr.W != 16 || hdr.H != 32 || hdr.Bits != 32 || hdr.Planes != 1 {
		t.Fatalf("заголовок: %+v", hdr)
	}
	// строки хранятся снизу вверх: первой идёт нижняя строка, порядок байтов BGRA
	if px := data[40 : 40+4]; !bytes.Equal(px, []byte{3, 2, 1, 255}) {
		t.Fatalf("нижний левый пиксель: %v", px)
	}
	last := data[40+15*16*4 : 40+15*16*4+4]
	if !bytes.Equal(last, []byte{30, 20, 10, 200}) {
		t.Fatalf("верхний левый пиксель: %v", last)
	}
}

func TestEmbeddedIconSets(t *testing.T) {
	for _, s := range iconSizes {
		set, err := loadIconSet(s)
		if err != nil {
			t.Fatalf("размер %d: %v", s, err)
		}
		if set.size != s || len(set.on) == 0 || len(set.active) == 0 || len(set.paused) == 0 || len(set.err) == 0 {
			t.Fatalf("набор %d неполный: %+v", s, set.size)
		}
		if bytes.Equal(set.on, set.active) || bytes.Equal(set.on, set.paused) || bytes.Equal(set.on, set.err) {
			t.Fatalf("значки состояний должны отличаться друг от друга (размер %d)", s)
		}
	}
	if pickSize(17) != 20 || pickSize(100) != 48 || pickSize(1) != 16 {
		t.Fatal("pickSize")
	}
}

func TestComputeState(t *testing.T) {
	ok := []RuleStatus{{Name: "a", Status: "ok"}}
	bad := []RuleStatus{{Name: "a", Status: "ok"}, {Name: "b", Status: "error"}}
	if computeState(true, false, bad) != stBusy {
		t.Fatal("занят важнее ошибки")
	}
	if computeState(false, true, bad) != stError {
		t.Fatal("ошибка важнее паузы")
	}
	if computeState(false, true, ok) != stPaused || computeState(false, false, ok) != stIdle {
		t.Fatal("пауза/покой")
	}
}

func TestActionsAndTexts(t *testing.T) {
	if parseAction("SyncNow", actStatus) != actSyncNow || parseAction("bogus", actStatus) != actStatus || parseAction("", actNone) != actNone {
		t.Fatal("parseAction")
	}
	st := []RuleStatus{{Name: "Rule 1", Status: "ok"}, {Name: "R2", Status: "paused"}}
	if got := summaryText(st); got != "Rule 1 — ок\nR2 — пауза" {
		t.Fatalf("summary: %q", got)
	}
	if n := len([]rune(tooltipText("v0.4.0", st, stIdle))); n > 127 {
		t.Fatalf("подсказка слишком длинная: %d", n)
	}
	long := make([]RuleStatus, 60)
	for i := range long {
		long[i] = RuleStatus{Name: "Очень длинное имя правила", Status: "ok"}
	}
	if n := len([]rune(summaryText(long))); n > 250 {
		t.Fatalf("сводка слишком длинная: %d", n)
	}
}

func TestNotifierThrottle(t *testing.T) {
	var mu sync.Mutex
	var shown []string
	n := newNotifier(80*time.Millisecond, func(title, msg string, level int) {
		mu.Lock()
		shown = append(shown, title+"|"+msg)
		mu.Unlock()
	})
	n.Push("r", "Конфликт файлов", "off") // выключено — не показываем
	n.enabled.Store(true)
	n.Push("r", "Конфликт файлов", "a.txt")
	n.Push("r", "Конфликт файлов", "b.txt")
	n.Push("r", "Ошибка синхронизации", "диск")
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if len(shown) != 1 {
		t.Fatalf("сразу должно быть показано одно уведомление, показано %d", len(shown))
	}
	mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(shown) != 2 || shown[1] != "LocalSyncExt|Ещё уведомлений: 2. Последнее: [r] диск" {
		t.Fatalf("сводка: %q", shown)
	}
	if levelFor("Ошибка синхронизации") != levelError || levelFor("Конфликт файлов") != levelWarn || levelFor("x") != levelInfo {
		t.Fatal("levelFor")
	}
}

func TestUIStateRoundTrip(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if u := loadUIState(d); u.Notifications != nil {
		t.Fatal("пустое состояние")
	}
	off := false
	saveUIState(d, uiState{Notifications: &off})
	if u := loadUIState(d); u.Notifications == nil || *u.Notifications {
		t.Fatal("сохранённое значение потеряно")
	}
	if _, err := os.Stat(uiStatePath(d)); err != nil {
		t.Fatal(err)
	}
}

func TestPauseResume(t *testing.T) {
	cfgPoll = time.Hour
	en, cfg := setup(t, "[R1]\nscan=1\nwatch=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	ctx, cancel := context.WithCancel(context.Background())
	en.Start(ctx)
	defer func() { cancel(); en.Stop() }()
	wr(t, filepath.Join(a, "first.txt"), "1", time.Minute)
	waitFor(t, 8*time.Second, func() bool { return exists(filepath.Join(b, "first.txt")) }, "начальная синхронизация")

	en.PauseAll(true)
	if !en.AllPaused() || !en.IsPaused("R1") {
		t.Fatal("правило должно быть на паузе")
	}
	wr(t, filepath.Join(a, "paused.txt"), "2", time.Minute)
	time.Sleep(2500 * time.Millisecond)
	if exists(filepath.Join(b, "paused.txt")) {
		t.Fatal("на паузе синхронизация идти не должна")
	}
	if st := en.Statuses(); st[0].Status != "paused" {
		t.Fatalf("статус %q", st[0].Status)
	}
	en.PauseAll(false)
	waitFor(t, 8*time.Second, func() bool { return exists(filepath.Join(b, "paused.txt")) }, "синхронизация после снятия паузы")

	// «синхронизировать сейчас» не требует ожидания периода сканирования
	wr(t, filepath.Join(a, "now.txt"), "3", time.Minute)
	en.TriggerAll()
	waitFor(t, 3*time.Second, func() bool { return exists(filepath.Join(b, "now.txt")) }, "синхронизация по кнопке")
}
