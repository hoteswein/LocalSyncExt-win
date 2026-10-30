package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Attrs — права и владелец в стиле POSIX (есть у Linux/Android, на Windows их нет).
type Attrs struct {
	Mode  uint32 `json:"m"` // биты fs.FileMode: права, setuid/setgid/sticky
	UID   uint32 `json:"u"`
	GID   uint32 `json:"g"`
	Owner bool   `json:"o"` // UID/GID известны
}

// RFile и WFile — открытые файлы любой файловой системы.
type RFile interface {
	io.ReadCloser
}

type WFile interface {
	io.WriteCloser
	Sync() error
}

// FileFS — файловая система, в которой лежит путь правила: локальный диск или удалённый SFTP.
// Все пути передаются в нотации самой файловой системы (Windows: C:\a\b, SFTP: /a/b).
type FileFS interface {
	Local() bool
	CaseSensitive() bool
	HasAttrs() bool // есть ли права и владелец в стиле POSIX
	Join(elem ...string) string
	Dir(p string) string
	Base(p string) string
	FromRel(rel string) string               // относительный путь с '/' -> нотация ФС
	RelSlash(base, p string) (string, error) // путь p относительно base, с '/'
	Lstat(p string) (fs.FileInfo, error)
	Stat(p string) (fs.FileInfo, error)
	ReadDir(p string) ([]fs.DirEntry, error)
	Open(p string) (RFile, error)
	Create(p string, excl bool) (WFile, error)
	ReadFile(p string) ([]byte, error)
	WriteFile(p string, data []byte) error
	Rename(oldp, newp string) error
	Remove(p string) error
	MkdirAll(p string) error
	Chtimes(p string, t time.Time) error
	AttrsOf(info fs.FileInfo) *Attrs
	SetAttrs(p string, a Attrs) error
}

// ---------- локальный диск ----------

type localFS struct{}

var theLocalFS FileFS = localFS{}

func (localFS) Local() bool               { return true }
func (localFS) HasAttrs() bool            { return localHasAttrs }
func (localFS) CaseSensitive() bool       { return localCaseSensitive }
func (localFS) Join(e ...string) string   { return filepath.Join(e...) }
func (localFS) Dir(p string) string       { return filepath.Dir(p) }
func (localFS) Base(p string) string      { return filepath.Base(p) }
func (localFS) FromRel(rel string) string { return filepath.FromSlash(rel) }
func (localFS) RelSlash(base, p string) (string, error) {
	r, err := filepath.Rel(base, p)
	return filepath.ToSlash(r), err
}
func (localFS) Lstat(p string) (fs.FileInfo, error)     { return os.Lstat(p) }
func (localFS) Stat(p string) (fs.FileInfo, error)      { return os.Stat(p) }
func (localFS) ReadDir(p string) ([]fs.DirEntry, error) { return os.ReadDir(p) }
func (localFS) ReadFile(p string) ([]byte, error)       { return os.ReadFile(p) }
func (localFS) WriteFile(p string, d []byte) error      { return os.WriteFile(p, d, 0o644) }
func (localFS) Rename(a, b string) error                { return os.Rename(a, b) }
func (localFS) Remove(p string) error                   { return os.Remove(p) }
func (localFS) MkdirAll(p string) error                 { return os.MkdirAll(p, 0o755) }
func (localFS) Chtimes(p string, t time.Time) error     { return os.Chtimes(p, t, t) }
func (localFS) Open(p string) (RFile, error)            { return os.Open(p) }
func (localFS) Create(p string, excl bool) (WFile, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if excl {
		flags |= os.O_EXCL
	} else {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(p, flags, 0o644)
}
func (localFS) AttrsOf(info fs.FileInfo) *Attrs  { return localAttrsOf(info) }
func (localFS) SetAttrs(p string, a Attrs) error { return localSetAttrs(p, a) }

// ---------- общие помощники ----------

// walkFS — обход дерева (как filepath.WalkDir) для любой файловой системы.
func walkFS(f FileFS, root string, fn func(p string, d fs.DirEntry, err error) error) error {
	info, err := f.Lstat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = walkDirFS(f, root, fs.FileInfoToDirEntry(info), fn)
	}
	if err == fs.SkipDir || err == fs.SkipAll {
		return nil
	}
	return err
}

func walkDirFS(f FileFS, p string, d fs.DirEntry, fn func(string, fs.DirEntry, error) error) error {
	if err := fn(p, d, nil); err != nil || !d.IsDir() {
		if err == fs.SkipDir && d.IsDir() {
			err = nil
		}
		return err
	}
	ents, err := f.ReadDir(p)
	if err != nil {
		err = fn(p, d, err)
		if err != nil {
			if err == fs.SkipDir && d.IsDir() {
				err = nil
			}
			return err
		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		if err := walkDirFS(f, f.Join(p, e.Name()), e, fn); err != nil {
			if err == fs.SkipDir {
				break
			}
			return err
		}
	}
	return nil
}

// splitName делит имя на основу и расширение (с точкой).
func splitName(base string) (stem, ext string) {
	i := strings.LastIndexByte(base, '.')
	if i <= 0 {
		return base, ""
	}
	return base[:i], base[i:]
}

// moveBetween переносит файл между путями; на одной файловой системе — переименованием,
// иначе копированием с последующим удалением исходного.
func moveBetween(sf FileFS, src string, df FileFS, dst string) error {
	if sf == df {
		if err := sf.Rename(src, dst); err == nil {
			return nil
		} else if _, serr := sf.Stat(src); serr != nil {
			return err
		}
	}
	info, err := sf.Stat(src)
	if err != nil {
		return err
	}
	in, err := sf.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := df.Create(dst, true)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = df.Chtimes(dst, info.ModTime())
	}
	if err != nil {
		_ = df.Remove(dst)
		return err
	}
	in.Close()
	return sf.Remove(src)
}
