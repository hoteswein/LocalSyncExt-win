//go:build !windows

package main

import (
	"os"
	"testing"
)

// Локальные пути в тестах ведут себя как на Windows (имена без учёта регистра в логике программы);
// Linux-сторона в тестах — SFTP.
func TestMain(m *testing.M) {
	localCaseSensitive = false
	os.Exit(m.Run())
}
