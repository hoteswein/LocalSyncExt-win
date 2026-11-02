package main

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestCaseCodecExamples(t *testing.T) {
	enc, sk := encodeGroup([]string{"file.ext", "File.ext", "FILE.ext", "fIlE.ext"})
	want := map[string]string{"file.ext": "file.ext", "File.ext": "File[linux_case].ext", "fIlE.ext": "fIlE[linux_case2].ext", "FILE.ext": "FILE[linux_case3].ext"}
	if len(sk) != 0 || len(enc) != 4 {
		t.Fatalf("encodeGroup: %v %v", enc, sk)
	}
	for k, v := range want {
		if enc[k] != v {
			t.Errorf("%s -> %s, ожидалось %s", k, enc[k], v)
		}
	}
	// пара «основной + один двойник» кодируется ровно по схеме
	enc, _ = encodeGroup([]string{"file.ext", "File.ext"})
	if enc["File.ext"] != "File[linux_case].ext" {
		t.Fatalf("пара: %v", enc)
	}
	enc, _ = encodeGroup([]string{"another_file.ext", "another_file.Ext"})
	if enc["another_file.Ext"] != "another_file.Ext.ext" {
		t.Fatalf("расширение: %v", enc)
	}
	// у второго двойника по расширению имя с .ext совпало бы без учёта регистра — берём метку
	enc, _ = encodeGroup([]string{"another_file.ext", "another_file.Ext", "another_file.EXT"})
	if enc["another_file.Ext"] != "another_file.Ext.ext" || enc["another_file.EXT"] != "another_file[linux_case].EXT" {
		t.Fatalf("три расширения: %v", enc)
	}
	// без полностью строчного имени основным становится первое по порядку байт
	enc, _ = encodeGroup([]string{"File.ext", "FILE.ext"})
	if enc["FILE.ext"] != "FILE.ext" || enc["File.ext"] != "File[linux_case].ext" {
		t.Fatalf("основной файл: %v", enc)
	}
	for phys, logical := range map[string]string{
		"File[linux_case].ext": "File.ext", "FILE[linux_case3].ext": "FILE.ext", "FILE[linux_case]": "FILE",
		"another_file.Ext.ext": "another_file.Ext", "another_file[linux_case].EXT": "another_file.EXT",
		"README.md": "README.md", "report.PDF.pdf.bak": "report.PDF.pdf.bak", "a.b.c": "a.b.c", ".bashrc": ".bashrc",
	} {
		if got := decodeFileName(phys); got != logical {
			t.Errorf("decode(%q) = %q, ожидалось %q", phys, got, logical)
		}
	}
}

// Любая группа имён кодируется однозначно (без совпадений без учёта регистра) и разбирается обратно.
func TestCaseCodecProperty(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	letters := []string{"a", "A", "b", "B", "Ab", "aB"}
	exts := []string{"", ".txt", ".TXT", ".Txt", ".tar.gz", ".GZ"}
	for iter := 0; iter < 3000; iter++ {
		seen := map[string]bool{}
		var names []string
		base := letters[rnd.Intn(len(letters))] + letters[rnd.Intn(len(letters))]
		for k := 0; k < 1+rnd.Intn(6); k++ {
			stem := base
			if rnd.Intn(2) == 0 {
				stem = strings.ToUpper(base)
			}
			if rnd.Intn(3) == 0 {
				stem = strings.ToLower(base)
			}
			n := stem + exts[rnd.Intn(len(exts))]
			if strings.ToLower(n) != strings.ToLower(base+".txt") && strings.ToLower(n) != strings.ToLower(base) && !strings.HasPrefix(strings.ToLower(n), strings.ToLower(base)) {
				continue
			}
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		// оставляем только имена одной группы (совпадающие без учёта регистра)
		if len(names) == 0 {
			continue
		}
		first := strings.ToLower(names[0])
		var group []string
		for _, n := range names {
			if strings.ToLower(n) == first {
				group = append(group, n)
			}
		}
		enc, skipped := encodeGroup(group)
		used := map[string]string{}
		for n, e := range enc {
			if prev, dup := used[strings.ToLower(e)]; dup {
				t.Fatalf("имена %q и %q кодируются в совпадающие %q (группа %v)", prev, n, e, group)
			}
			used[strings.ToLower(e)] = n
			if got := decodeFileName(e); got != n {
				t.Fatalf("decode(encode(%q)=%q) = %q (группа %v)", n, e, got, group)
			}
		}
		if len(enc)+len(skipped) != len(group) {
			t.Fatalf("потеряны имена: %v %v %v", group, enc, skipped)
		}
		sort.Strings(skipped)
	}
}
