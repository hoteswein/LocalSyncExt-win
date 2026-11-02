//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func sameNames(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

// На «Windows-стороне» (локальный путь в тестах нечувствителен к регистру) не должно быть имён, совпадающих без учёта регистра.
func assertNoFoldDup(t *testing.T, dir string) {
	t.Helper()
	seen := map[string]string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			k := strings.ToLower(p)
			if prev, dup := seen[k]; dup {
				t.Fatalf("на стороне Windows совпадающие без учёта регистра имена: %s и %s", prev, p)
			}
			seen[k] = p
		}
		return nil
	})
}

func TestLinuxCaseMarksBothWays(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, cfg, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	r := cfg.Rules[0]
	local, remote := filepath.Join(root, "local"), filepath.Join(root, "remote")
	old := time.Hour
	for n, c := range map[string]string{"file.ext": "lower", "File.ext": "Title", "FILE.ext": "UPPER", "fIlE.ext": "mixed",
		"another_file.ext": "e1", "another_file.EXT": "e2", "another_file.Ext": "e3", "plain.txt": "plain"} {
		wr(t, filepath.Join(remote, n), c, old)
	}
	if !en.RunOnce() {
		t.Fatalf("ошибка прохода: %+v", en.Events())
	}
	if r.caseMode != "mapped" {
		t.Fatalf("режим регистра: %q", r.caseMode)
	}
	want := []string{"file.ext", "File[linux_case].ext", "fIlE[linux_case2].ext", "FILE[linux_case3].ext",
		"another_file.ext", "another_file.Ext.ext", "another_file[linux_case].EXT", "plain.txt"}
	if got := listNames(t, local); !sameNames(got, want) {
		t.Fatalf("имена на стороне Windows:\n%v\nожидалось:\n%v", got, want)
	}
	for phys, content := range map[string]string{"file.ext": "lower", "File[linux_case].ext": "Title", "FILE[linux_case3].ext": "UPPER", "fIlE[linux_case2].ext": "mixed",
		"another_file[linux_case].EXT": "e2", "another_file.Ext.ext": "e3"} {
		if rd(t, filepath.Join(local, phys)) != content {
			t.Errorf("%s: содержимое %q", phys, rd(t, filepath.Join(local, phys)))
		}
	}
	assertNoFoldDup(t, local)
	if got := listNames(t, remote); len(got) != 8 {
		t.Fatalf("на Linux-стороне имён должно остаться 8: %v", got)
	}

	// правка метки-файла на Windows уходит в настоящий файл на Linux
	wr(t, filepath.Join(local, "File[linux_case].ext"), "Title-v2", 30*time.Second)
	wr(t, filepath.Join(local, "another_file.Ext.ext"), "e3-new", 30*time.Second)
	if !en.RunOnce() {
		t.Fatal("ошибка прохода")
	}
	if rd(t, filepath.Join(remote, "File.ext")) != "Title-v2" || rd(t, filepath.Join(remote, "another_file.Ext")) != "e3-new" {
		t.Fatal("правка не дошла до настоящих имён на Linux")
	}
	if rd(t, filepath.Join(remote, "file.ext")) != "lower" {
		t.Fatal("соседний файл не должен меняться")
	}
	if got := listNames(t, remote); len(got) != 8 {
		t.Fatalf("метки не должны появляться на Linux: %v", got)
	}
	// новый файл на Windows с меткой в имени получает на Linux настоящее имя
	wr(t, filepath.Join(local, "Zed[linux_case].txt"), "zed", 30*time.Second)
	wr(t, filepath.Join(local, "Report.TXT"), "rep", 30*time.Second)
	en.RunOnce()
	if rd(t, filepath.Join(remote, "Zed.txt")) != "zed" || rd(t, filepath.Join(remote, "Report.TXT")) != "rep" {
		t.Fatalf("имена на Linux: %v", listNames(t, remote))
	}
	// исчез основной файл группы — имена на Windows перестраиваются
	os.Remove(filepath.Join(remote, "file.ext"))
	for i := 0; i < 2; i++ {
		if !en.RunOnce() {
			t.Fatal("ошибка прохода")
		}
	}
	got := listNames(t, local)
	for _, w := range []string{"FILE.ext", "File[linux_case].ext", "fIlE[linux_case2].ext"} {
		if !sameNames([]string{w}, filterName(got, w)) {
			t.Fatalf("после удаления file.ext ожидалось имя %s, есть: %v", w, got)
		}
	}
	if exists(filepath.Join(local, "file.ext")) || exists(filepath.Join(local, "FILE[linux_case3].ext")) {
		t.Fatalf("старые имена остались: %v", got)
	}
	assertNoFoldDup(t, local)
	if rd(t, filepath.Join(local, "FILE.ext")) != "UPPER" {
		t.Fatal("содержимое после переименования потеряно")
	}
	// появился новый «двойник» на Linux — существующий файл на Windows получает метку, а новый — своё имя
	wr(t, filepath.Join(remote, "plain.TXT"), "second", old)
	en.RunOnce()
	en.RunOnce()
	if rd(t, filepath.Join(local, "plain.txt")) != "plain" || rd(t, filepath.Join(local, "plain.TXT.txt")) != "second" {
		t.Fatalf("двойник plain.TXT: %v", listNames(t, local))
	}
	assertNoFoldDup(t, local)
}

func filterName(names []string, want string) []string {
	for _, n := range names {
		if n == want {
			return []string{n}
		}
	}
	return nil
}

func TestCaseDirCollisionsSkipped(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, cfg, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\n{R}/local\nsftp://srv{R}/remote\n")
	r := cfg.Rules[0]
	remote := filepath.Join(root, "remote")
	wr(t, filepath.Join(remote, "x", "Docs", "a.txt"), "a", time.Hour)
	wr(t, filepath.Join(remote, "x", "docs", "b.txt"), "b", time.Hour)
	wr(t, filepath.Join(remote, "ok.txt"), "ok", time.Hour)
	en.RunOnce()
	if rd(t, filepath.Join(root, "local", "ok.txt")) != "ok" {
		t.Fatal("обычный файл должен синхронизироваться")
	}
	if exists(filepath.Join(root, "local", "x")) {
		t.Fatal("содержимое папок, отличающихся регистром, копироваться не должно")
	}
	if !en.rtFor(r).notified["cased:x/docs"] {
		t.Fatal("нужно предупреждение о папках")
	}
}

func TestExactModeBetweenLinuxPaths(t *testing.T) {
	sshd := startSSHD(t, "tester", "pw123456")
	en, cfg, root := sftpEnv(t, sshd, sshd.fp, "[R1]\nscan=0\nwatch=0\nsftp://srv{R}/a\nsftp://srv{R}/b\n")
	if cfg.Rules[0].Err != "" {
		t.Fatal(cfg.Rules[0].Err)
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	wr(t, filepath.Join(a, "File.ext"), "T", time.Hour)
	wr(t, filepath.Join(a, "file.ext"), "l", time.Hour)
	wr(t, filepath.Join(a, "Dir", "x.txt"), "1", time.Hour)
	wr(t, filepath.Join(a, "dir", "y.txt"), "2", time.Hour)
	en.RunOnce()
	if got := listNames(t, b); !sameNames(got, []string{"File.ext", "file.ext", "Dir", "dir"}) {
		t.Fatalf("между Linux-путями имена должны сохраняться как есть: %v", got)
	}
	if cfg.Rules[0].caseMode != "exact" {
		t.Fatalf("режим: %s", cfg.Rules[0].caseMode)
	}
}
