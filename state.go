package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Base — «согласованная» версия файла на момент последней успешной синхронизации.
type Base struct {
	Size  int64 `json:"s"`
	MTime int64 `json:"m"`
}

type State struct {
	IDs  map[string]string `json:"ids"`  // путь -> id маркера (защита от отключённого/подменённого диска)
	Base map[string]Base   `json:"base"` // ключ файла -> согласованная версия
}

func loadState(path string) (*State, error) {
	st := &State{IDs: map[string]string{}, Base: map[string]Base{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		// Молча начать с нуля нельзя: удалённые файлы «воскреснут».
		return nil, fmt.Errorf("повреждён файл состояния %s: %w (удалите его вручную, если уверены)", path, err)
	}
	if st.IDs == nil {
		st.IDs = map[string]string{}
	}
	if st.Base == nil {
		st.Base = map[string]Base{}
	}
	return st, nil
}

func (s *State) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
