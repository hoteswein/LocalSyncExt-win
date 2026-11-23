package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigCreatesDefaultWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LocalSyncExt.ini")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Rules) != 0 {
		t.Fatalf("ожидался конфиг без правил; получено %d", len(cfg.Rules))
	}
	if cfg.Settings["webui"] != "1" || cfg.Settings["webportopen"] != "20001" || cfg.Settings["webopenbind"] != "127.0.0.1" || cfg.Settings["webportauth"] != "0" {
		t.Fatalf("неожиданные настройки по умолчанию: %#v", cfg.Settings)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("конфиг не создан: %v", err)
	}

	const custom = "; preserve me\n[Custom]\n"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(path); err != nil {
		t.Fatalf("повторная проверка конфига: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom || !strings.Contains(string(data), "preserve me") {
		t.Fatalf("существующий конфиг был изменён: %q", data)
	}
}
