//go:build windows

package main

import (
	"testing"
	"unsafe"
)

// Размеры структур Win32 должны совпадать с C (x64), иначе вызовы API молча не сработают.
func TestWin32StructSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("проверка для x64")
	}
	if s := unsafe.Sizeof(notifyIconData{}); s != 976 {
		t.Errorf("NOTIFYICONDATAW = %d, ожидалось 976", s)
	}
	if s := unsafe.Sizeof(wndClassEx{}); s != 80 {
		t.Errorf("WNDCLASSEXW = %d, ожидалось 80", s)
	}
	if s := unsafe.Sizeof(winMsg{}); s != 48 {
		t.Errorf("MSG = %d, ожидалось 48", s)
	}
}

// Настоящий CreateIconFromResourceEx должен принять все наши значки.
func TestIconsAcceptedByWindows(t *testing.T) {
	for _, want := range []int{16, 24, 32, 48} {
		set, err := loadIconSet(want)
		if err != nil {
			t.Fatal(err)
		}
		tr := &winTray{set: set}
		all := append([][]byte{set.idle, set.paused, set.err}, set.spin...)
		for i, data := range all {
			h, err := tr.makeIcon(data)
			if err != nil || h == 0 {
				t.Fatalf("размер %d, значок %d: %v", want, i, err)
			}
			pDestroyIcon.Call(h)
		}
	}
}

func TestAutostartCommand(t *testing.T) {
	if got := autostartCommand(`C:\Program Files\X\LocalSyncExt.exe`); got != `"C:\Program Files\X\LocalSyncExt.exe" --tray` {
		t.Fatal(got)
	}
}
