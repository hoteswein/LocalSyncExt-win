package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wr(t *testing.T, p, content string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func rd(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("нет файла %s: %v", p, err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func setup(t *testing.T, ini string, dirs ...string) (*Engine, *Config) {
	t.Helper()
	root := t.TempDir()
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	f := filepath.Join(root, "test.ini")
	if err := os.WriteFile(f, []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Validate()
	for _, r := range cfg.Rules {
		if r.Err != "" {
			t.Fatalf("ошибка правила %s: %s", r.Name, r.Err)
		}
	}
	return NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard), cfg
}


func TestTwoWayBasic(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\n{R}/a\n{R}/b\n{R}/c\n")
	r := cfg.Rules[0]
	a, b, c := r.Endpoints[0].Raw, r.Endpoints[1].Raw, r.Endpoints[2].Raw
	wr(t, filepath.Join(a, "x.txt"), "one", time.Minute)
	wr(t, filepath.Join(a, "sub", "y.txt"), "two", time.Minute)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	for _, d := range []string{b, c} {
		if rd(t, filepath.Join(d, "x.txt")) != "one" || rd(t, filepath.Join(d, "sub", "y.txt")) != "two" {
			t.Fatalf("не скопировано в %s", d)
		}
	}
	// изменение в b
	wr(t, filepath.Join(b, "x.txt"), "one-new", 10*time.Second)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(a, "x.txt")) != "one-new" || rd(t, filepath.Join(c, "x.txt")) != "one-new" {
		t.Fatal("изменение не разошлось")
	}
	// удаление в c -> корзина в a и b
	os.Remove(filepath.Join(c, "sub", "y.txt"))
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	for _, d := range []string{a, b} {
		if exists(filepath.Join(d, "sub", "y.txt")) {
			t.Fatalf("файл не удалён в %s", d)
		}
		ents, err := os.ReadDir(filepath.Join(d, trashDir, "sub"))
		if err != nil || len(ents) == 0 {
			t.Fatalf("нет файла в корзине %s: %v", d, err)
		}
	}
	// удалённый файл не воскресает
	en.RunOnce()
	if exists(filepath.Join(c, "sub", "y.txt")) {
		t.Fatal("удалённый файл воскрес")
	}
}

func TestConflict(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "doc.txt"), "base", time.Hour)
	en.RunOnce()
	wr(t, filepath.Join(a, "doc.txt"), "from-A", 10*time.Minute)
	wr(t, filepath.Join(b, "doc.txt"), "from-B", 5*time.Minute) // новее — победитель
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(a, "doc.txt")) != "from-B" {
		t.Fatal("победитель не применён")
	}
	ents, _ := filepath.Glob(filepath.Join(a, "doc.sync-conflict-*.txt"))
	if len(ents) != 1 || rd(t, ents[0]) != "from-A" {
		t.Fatalf("конфликтная копия не найдена: %v", ents)
	}
	en.RunOnce() // конфликтная копия расходится на все папки
	ents, _ = filepath.Glob(filepath.Join(b, "doc.sync-conflict-*.txt"))
	if len(ents) != 1 {
		t.Fatalf("конфликтная копия не синхронизирована: %v", ents)
	}
}

func TestOfflineProtection(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	for i := 0; i < 8; i++ {
		wr(t, filepath.Join(a, string(rune('a'+i))+".txt"), "data", time.Minute)
	}
	en.RunOnce()
	// «диск отключили»
	os.RemoveAll(b)
	if en.RunOnce() {
		t.Fatal("ожидалась ошибка при отсутствии папки")
	}
	if !exists(filepath.Join(a, "a.txt")) {
		t.Fatal("файлы в a пропали при отключённом b")
	}
	// папка вернулась пустой, но без маркера
	os.MkdirAll(b, 0o755)
	if en.RunOnce() {
		t.Fatal("ожидалась ошибка при потерянном маркере")
	}
	if !exists(filepath.Join(a, "a.txt")) {
		t.Fatal("файлы в a пропали при пустой b без маркера")
	}
}

func TestMassDeleteProtection(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	for i := 0; i < 10; i++ {
		wr(t, filepath.Join(a, string(rune('a'+i))+".txt"), "data", time.Minute)
	}
	en.RunOnce()
	for i := 0; i < 8; i++ {
		os.Remove(filepath.Join(b, string(rune('a'+i))+".txt"))
	}
	if en.RunOnce() {
		t.Fatal("ожидалась защита от массового удаления")
	}
	if !exists(filepath.Join(a, "a.txt")) {
		t.Fatal("защита не сработала: файлы удалены")
	}
}

func TestOneWay(t *testing.T) {
	en, cfg := setup(t, "[R1]\nmode=oneway\nmaster=1\nscan=0\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "m.txt"), "master", time.Minute)
	wr(t, filepath.Join(b, "extra.txt"), "extra", time.Minute)
	wr(t, filepath.Join(b, "m.txt"), "old", time.Hour)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(b, "m.txt")) != "master" {
		t.Fatal("мастер не применён")
	}
	if exists(filepath.Join(b, "extra.txt")) {
		t.Fatal("лишний файл не удалён")
	}
	if !exists(filepath.Join(b, trashDir)) {
		t.Fatal("лишний файл должен уйти в корзину")
	}
	if exists(filepath.Join(a, "extra.txt")) {
		t.Fatal("односторонний режим изменил мастер")
	}
}

func TestNoTrash(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\nTrashDays=\n{R}/a\n{R}/b\n")
	r := cfg.Rules[0]
	a, b := r.Endpoints[0].Raw, r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "x.txt"), "x", time.Minute)
	en.RunOnce()
	os.Remove(filepath.Join(a, "x.txt"))
	en.RunOnce()
	if exists(filepath.Join(b, "x.txt")) || exists(filepath.Join(b, trashDir)) {
		t.Fatal("при TrashDays= файл должен удаляться без корзины")
	}
}

func TestMask(t *testing.T) {
	en, cfg := setup(t, "[R1]\nscan=0\n{R}/a/*.pdf\n{R}/b\n")
	r := cfg.Rules[0]
	a := filepath.Dir(r.Endpoints[0].Raw)
	b := r.Endpoints[1].Raw
	wr(t, filepath.Join(a, "one.pdf"), "pdf", time.Minute)
	wr(t, filepath.Join(a, "two.txt"), "txt", time.Minute)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if !exists(filepath.Join(b, "one.pdf")) || exists(filepath.Join(b, "two.txt")) {
		t.Fatal("маска применена неверно")
	}
}

func TestSingleFiles(t *testing.T) {
	root := t.TempDir()
	fa, fb := filepath.Join(root, "x", "config.ini"), filepath.Join(root, "y", "settings.ini")
	wr(t, fa, "v1", time.Minute)
	os.MkdirAll(filepath.Dir(fb), 0o755)
	ini := "[F]\nscan=0\n" + filepath.ToSlash(fa) + "\n" + filepath.ToSlash(fb) + "\n"
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini), 0o644)
	cfg, _ := ParseConfig(f)
	cfg.Validate()
	if cfg.Rules[0].Err != "" {
		t.Fatal(cfg.Rules[0].Err)
	}
	en := NewEngine(cfg, filepath.Join(root, "state"), false, io.Discard)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, fb) != "v1" {
		t.Fatal("файл не скопирован")
	}
	wr(t, fb, "v2", 10*time.Second)
	en.RunOnce()
	if rd(t, fa) != "v2" {
		t.Fatal("изменение файла не вернулось")
	}
}

func TestOverlapValidation(t *testing.T) {
	root := t.TempDir()
	ini := "[A]\n{R}/a\n{R}/b\n[B]\n{R}/a/sub\n{R}/c\n[C]\n{R}/a\n{R}/a/inner\n"
	ini = strings.ReplaceAll(ini, "{R}", filepath.ToSlash(root))
	f := filepath.Join(root, "t.ini")
	os.WriteFile(f, []byte(ini), 0o644)
	cfg, _ := ParseConfig(f)
	cfg.Validate()
	if cfg.Rules[0].Err != "" || cfg.Rules[1].Err == "" || cfg.Rules[2].Err == "" {
		t.Fatalf("валидация пересечений: %q %q %q", cfg.Rules[0].Err, cfg.Rules[1].Err, cfg.Rules[2].Err)
	}
}
