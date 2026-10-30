//go:build !windows

package main

import (
	"io/fs"
	"os"
	"runtime"
	"syscall"
)

var localCaseSensitive = runtime.GOOS != "darwin"

func localAttrsOf(info fs.FileInfo) *Attrs {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return &Attrs{Mode: uint32(info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))}
	}
	return &Attrs{Mode: uint32(info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)), UID: st.Uid, GID: st.Gid, Owner: true}
}

func localSetAttrs(p string, a Attrs) error {
	if err := os.Chmod(p, fs.FileMode(a.Mode)); err != nil {
		return err
	}
	if a.Owner {
		return os.Chown(p, int(a.UID), int(a.GID))
	}
	return nil
}

const localHasAttrs = true
