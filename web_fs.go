package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fsEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

type fsResp struct {
	Path      string    `json:"path"`
	Parent    string    `json:"parent"`
	Sep       string    `json:"sep"`
	Entries   []fsEntry `json:"entries"`
	Truncated bool      `json:"truncated"`
}

const fsMaxEntries = 4000

// fsList показывает содержимое папки на компьютере, где работает программа (для кнопки «Обзор»).
// Пустой путь — список дисков.
func fsList(p string) (*fsResp, error) {
	resp := &fsResp{Sep: string(filepath.Separator)}
	p = strings.TrimSpace(p)
	if p == "" {
		for _, d := range listRoots() {
			resp.Entries = append(resp.Entries, fsEntry{Name: d, Dir: true})
		}
		return resp, nil
	}
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("нужен полный путь")
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	resp.Path = p
	if par := filepath.Dir(p); par != p {
		resp.Parent = par
	} else {
		resp.Parent = "" // корень диска -> список дисков
	}
	for i, e := range ents {
		if i >= fsMaxEntries {
			resp.Truncated = true
			break
		}
		fe := fsEntry{Name: e.Name(), Dir: e.IsDir()}
		if info, err := e.Info(); err == nil {
			fe.MTime = info.ModTime().Unix()
			if !fe.Dir {
				fe.Size = info.Size()
			}
		}
		resp.Entries = append(resp.Entries, fe)
	}
	sort.SliceStable(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return resp, nil
}
