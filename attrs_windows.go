//go:build windows

package main

import "io/fs"

// Windows не хранит права и владельца в стиле POSIX.
const localCaseSensitive = false

func localAttrsOf(info fs.FileInfo) *Attrs  { return nil }
func localSetAttrs(p string, a Attrs) error { return nil }

const localHasAttrs = false
