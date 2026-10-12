//go:build windows

package main

import (
	"context"
	"path/filepath"
	"syscall"
	"unsafe"
)

var procCancelIoEx = syscall.NewLazyDLL("kernel32.dll").NewProc("CancelIoEx")

// имя файла | имя папки | размер | время записи | создание
const watchMask = 0x1 | 0x2 | 0x8 | 0x10 | 0x40

// watchDir следит за папкой через ReadDirectoryChangesW: один дескриптор на весь корень
// (рекурсивно, средствами ядра), без обхода дерева — нагрузка на диск нулевая.
// Блокируется до отмены ctx или фатальной ошибки.
func watchDir(ctx context.Context, dir string, recursive bool, onStart func(), emit func(rel string, overflow bool)) error {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(p, 0x0001, // FILE_LIST_DIRECTORY
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			procCancelIoEx.Call(uintptr(h), 0)
		case <-done:
		}
	}()
	onStart()
	buf := make([]byte, 64*1024)
	for {
		var n uint32
		if err := syscall.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), recursive, watchMask, &n, nil, 0); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n == 0 { // переполнение буфера ядра — события потеряны
			emit("", true)
			continue
		}
		off := uint32(0)
		for off < n {
			info := (*syscall.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			cnt := info.FileNameLength / 2
			name := syscall.UTF16ToString(unsafe.Slice(&info.FileName, cnt))
			emit(filepath.FromSlash(name), false)
			if info.NextEntryOffset == 0 {
				break
			}
			off += info.NextEntryOffset
		}
	}
}
