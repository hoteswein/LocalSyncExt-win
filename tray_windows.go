//go:build windows

package main

import (
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	pRegisterClassEx       = user32.NewProc("RegisterClassExW")
	pCreateWindowEx        = user32.NewProc("CreateWindowExW")
	pDefWindowProc         = user32.NewProc("DefWindowProcW")
	pGetMessage            = user32.NewProc("GetMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessage       = user32.NewProc("DispatchMessageW")
	pPostMessage           = user32.NewProc("PostMessageW")
	pPostQuitMessage       = user32.NewProc("PostQuitMessage")
	pCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	pAppendMenu            = user32.NewProc("AppendMenuW")
	pTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	pDestroyMenu           = user32.NewProc("DestroyMenu")
	pGetCursorPos          = user32.NewProc("GetCursorPos")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	pCreateIconFromRes     = user32.NewProc("CreateIconFromResourceEx")
	pDestroyIcon           = user32.NewProc("DestroyIcon")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pGetDoubleClickTime    = user32.NewProc("GetDoubleClickTime")
	pMessageBox            = user32.NewProc("MessageBoxW")
	pShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute          = shell32.NewProc("ShellExecuteW")
	pGetModuleHandle       = kernel32.NewProc("GetModuleHandleW")
	pCreateMutex           = kernel32.NewProc("CreateMutexW")
	pCloseHandle           = kernel32.NewProc("CloseHandle")
	pAttachConsole         = kernel32.NewProc("AttachConsole")
	pRegDeleteValue        = advapi32.NewProc("RegDeleteValueW")
	pRegCreateKeyEx        = advapi32.NewProc("RegCreateKeyExW")
	pRegSetValueEx         = advapi32.NewProc("RegSetValueExW")
)

const (
	wmNull       = 0x0000
	wmDestroy    = 0x0002
	wmLButtonUp  = 0x0202
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205
	wmApp        = 0x8000

	wmTray    = wmApp + 1 // сообщения от значка
	wmSetIcon = wmApp + 2 // сменить значок (lParam — индекс)
	wmBalloon = wmApp + 3 // показать уведомление

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	niifInfo    = 1
	niifWarning = 2
	niifError   = 3
	niifNoSound = 0x10

	mfString   = 0x0
	mfGrayed   = 0x1
	mfChecked  = 0x8
	mfPopup    = 0x10
	mfSeparate = 0x800

	tpmRightButton = 0x2
	tpmReturnCmd   = 0x100
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct{ x, y int32 }

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

// ---------- прочее: один экземпляр, консоль, сообщения ----------

// acquireInstance: false, если программа с таким ключом уже запущена.
func acquireInstance(key string) (bool, func()) {
	name := fmt.Sprintf("Local\\LocalSyncExt-%08x", crc32.ChecksumIEEE([]byte(strings.ToLower(key))))
	h, _, err := pCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(utf16(name))))
	if h == 0 {
		return true, func() {} // не смогли создать — не мешаем запуску
	}
	if err == syscall.ERROR_ALREADY_EXISTS {
		pCloseHandle.Call(h)
		return false, func() {}
	}
	return true, func() { pCloseHandle.Call(h) }
}

// attachConsole подключает окно-GUI версию к консоли, из которой она запущена (для -once/-check/-version).
func attachConsole() {
	const attachParent = ^uintptr(0)
	if r, _, _ := pAttachConsole.Call(attachParent); r == 0 {
		return
	}
	h, err := syscall.CreateFile(utf16("CONOUT$"), syscall.GENERIC_WRITE, syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(h), "CONOUT$")
	os.Stdout, os.Stderr = f, f
}

func showError(msg string) {
	pMessageBox.Call(0, uintptr(unsafe.Pointer(utf16(msg))), uintptr(unsafe.Pointer(utf16("LocalSyncExt"))), 0x10)
}

func openPath(p string) {
	r, _, _ := pShellExecute.Call(0, uintptr(unsafe.Pointer(utf16("open"))), uintptr(unsafe.Pointer(utf16(p))), 0, 0, 1)
	if r <= 32 {
		_ = exec.Command("notepad.exe", p).Start()
	}
}

// ---------- автозапуск ----------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "LocalSyncExt"

func autostartCommand(exe string) string { return `"` + exe + `" --tray` }

func autostartEnabled() bool {
	var k syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, utf16(runKey), 0, syscall.KEY_QUERY_VALUE, &k) != nil {
		return false
	}
	defer syscall.RegCloseKey(k)
	var typ, n uint32
	return syscall.RegQueryValueEx(k, utf16(runValue), nil, &typ, nil, &n) == nil
}

func setAutostart(on bool, exe string) error {
	var k syscall.Handle
	r, _, _ := pRegCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(utf16(runKey))), 0, 0, 0,
		uintptr(syscall.KEY_SET_VALUE), 0, uintptr(unsafe.Pointer(&k)), 0)
	if r != 0 {
		return syscall.Errno(r)
	}
	defer syscall.RegCloseKey(k)
	if !on {
		r, _, _ := pRegDeleteValue.Call(uintptr(k), uintptr(unsafe.Pointer(utf16(runValue))))
		if r != 0 && r != 2 { // 2 — значения и так нет
			return syscall.Errno(r)
		}
		return nil
	}
	v, _ := syscall.UTF16FromString(autostartCommand(exe))
	r, _, _ = pRegSetValueEx.Call(uintptr(k), uintptr(unsafe.Pointer(utf16(runValue))), 0, uintptr(syscall.REG_SZ),
		uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// ---------- значок в трее ----------

type winTray struct {
	tc     *trayCtx
	hwnd   uintptr
	set    *iconSet
	on     uintptr
	active uintptr
	pause  uintptr
	errI   uintptr

	taskbarCreated uint32

	mu       sync.Mutex
	clickT   *time.Timer
	lastDbl  time.Time
	cur      uintptr
	balloons []balloon
}

type balloon struct {
	title, msg string
	level      int
}

var theTray *winTray

func runTray(tc *trayCtx) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	dpiSize, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	set, err := loadIconSet(int(dpiSize))
	if err != nil {
		return err
	}
	t := &winTray{tc: tc, set: set}
	theTray = t
	if t.on, err = t.makeIcon(set.on); err != nil {
		return err
	}
	if t.active, err = t.makeIcon(set.active); err != nil {
		return err
	}
	if t.pause, err = t.makeIcon(set.paused); err != nil {
		return err
	}
	if t.errI, err = t.makeIcon(set.err); err != nil {
		return err
	}
	defer func() {
		for _, h := range []uintptr{t.on, t.active, t.pause, t.errI} {
			pDestroyIcon.Call(h)
		}
	}()

	r, _, _ := pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
	t.taskbarCreated = uint32(r)

	inst, _, _ := pGetModuleHandle.Call(0)
	className := utf16("LocalSyncExtTrayWnd")
	wc := wndClassEx{
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     inst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if a, _, e := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); a == 0 {
		return fmt.Errorf("RegisterClassEx: %v", e)
	}
	t.hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16("LocalSyncExt"))), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if t.hwnd == 0 {
		return fmt.Errorf("не удалось создать окно сообщений")
	}
	if err := t.notifyIcon(nimAdd, t.on, t.tooltip()); err != nil {
		return err
	}
	defer t.notifyIcon(nimDelete, 0, "")

	done := make(chan struct{})
	go t.animate(done)
	defer close(done)

	var m winMsg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

// makeIcon создаёт HICON из данных значка (см. iconImage).
func (t *winTray) makeIcon(data []byte) (uintptr, error) {
	h, _, e := pCreateIconFromRes.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, 0x00030000, uintptr(t.set.size), uintptr(t.set.size), 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx: %v", e)
	}
	return h, nil
}

func (t *winTray) notifyIcon(op uint32, icon uintptr, tip string) error {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = t.hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTray
	nid.hIcon = icon
	copyUTF16(nid.szTip[:], tip)
	if r, _, e := pShellNotifyIcon.Call(uintptr(op), uintptr(unsafe.Pointer(&nid))); r == 0 && op != nimDelete {
		return fmt.Errorf("Shell_NotifyIcon: %v", e)
	}
	return nil
}

func (t *winTray) tooltip() string {
	st := t.tc.en.Statuses()
	return tooltipText(t.tc.version, st, computeState(t.tc.en.Busy(), t.tc.en.AllPaused(), st))
}

func (t *winTray) post(msg uint32, wp, lp uintptr) {
	pPostMessage.Call(t.hwnd, uintptr(msg), wp, lp)
}

// Коды значков для сообщения wmSetIcon.
const (
	icoOn     = 0
	icoActive = 1
	icoPaused = 2
	icoError  = 3
)

// animate: выбирает значок по состоянию. Во время синхронизации значки on и active мигают
// с периодом TrayBlinkMs (по умолчанию 200 мс); в покое — on, на паузе — paused, при ошибке — on с бейджем.
func (t *winTray) animate(done chan struct{}) {
	blink := t.tc.blink
	if blink < 50 {
		blink = 200
	}
	tk := time.NewTicker(blink)
	defer tk.Stop()
	phase := false
	last := -1
	lastTip := ""
	lastTipAt := time.Time{}
	for {
		select {
		case <-done:
			return
		case <-tk.C:
		}
		code := icoOn
		if t.tc.en.Busy() {
			phase = !phase
			if phase {
				code = icoActive
			}
		} else {
			phase = false
			st := t.tc.en.Statuses()
			switch computeState(false, t.tc.en.AllPaused(), st) {
			case stError:
				code = icoError
			case stPaused:
				code = icoPaused
			}
		}
		tipChanged := false
		if time.Since(lastTipAt) > time.Second { // подсказка обновляется реже значка
			lastTipAt = time.Now()
			if tip := t.tooltip(); tip != lastTip {
				lastTip, tipChanged = tip, true
			}
		}
		if code != last || tipChanged {
			last = code
			t.post(wmSetIcon, 0, uintptr(code))
		}
	}
}

func (t *winTray) iconFor(code int32) uintptr {
	switch code {
	case icoActive:
		return t.active
	case icoPaused:
		return t.pause
	case icoError:
		return t.errI
	}
	return t.on
}

func (t *winTray) showBalloon(b balloon) {
	var nid notifyIconData
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = t.hwnd
	nid.uID = 1
	nid.uFlags = nifInfo
	copyUTF16(nid.szInfoTitle[:], clip(b.title, 63))
	copyUTF16(nid.szInfo[:], clip(b.msg, 255))
	switch b.level {
	case levelError:
		nid.dwInfoFlags = niifError
	case levelWarn:
		nid.dwInfoFlags = niifWarning
	default:
		nid.dwInfoFlags = niifInfo | niifNoSound
	}
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// Balloon ставит уведомление в очередь окна (можно вызывать из любого потока).
func (t *winTray) Balloon(title, msg string, level int) {
	t.mu.Lock()
	t.balloons = append(t.balloons, balloon{title, msg, level})
	t.mu.Unlock()
	t.post(wmBalloon, 0, 0)
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := theTray
	if t == nil {
		r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
		return r
	}
	switch uint32(msg) {
	case wmTray:
		switch uint32(lParam) {
		case wmLButtonUp:
			t.onLeftClick()
		case wmLButtonDbl:
			t.onDoubleClick()
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	case wmSetIcon:
		ic := t.iconFor(int32(lParam))
		t.cur = ic
		t.notifyIcon(nimModify, ic, t.tooltip())
		return 0
	case wmBalloon:
		t.mu.Lock()
		list := t.balloons
		t.balloons = nil
		t.mu.Unlock()
		for _, b := range list {
			t.showBalloon(b)
		}
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	if uint32(msg) == t.taskbarCreated && t.taskbarCreated != 0 { // перезапуск проводника
		ic := t.cur
		if ic == 0 {
			ic = t.on
		}
		t.notifyIcon(nimAdd, ic, t.tooltip())
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return r
}

// ---------- клики ----------

func (t *winTray) onLeftClick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.lastDbl) < 400*time.Millisecond {
		return // второй «up» двойного щелчка
	}
	dct, _, _ := pGetDoubleClickTime.Call()
	if t.clickT != nil {
		t.clickT.Stop()
	}
	t.clickT = time.AfterFunc(time.Duration(dct)*time.Millisecond, func() { go t.doAction(t.tc.click) })
}

func (t *winTray) onDoubleClick() {
	t.mu.Lock()
	if t.clickT != nil {
		t.clickT.Stop()
		t.clickT = nil
	}
	t.lastDbl = time.Now()
	t.mu.Unlock()
	go t.doAction(t.tc.dblClick)
}

func (t *winTray) doAction(a string) {
	tc := t.tc
	switch a {
	case actStatus:
		t.Balloon("LocalSyncExt", summaryText(tc.en.Statuses()), levelInfo)
	case actSyncNow:
		tc.en.TriggerAll()
	case actPause:
		tc.en.PauseAll(!tc.en.AllPaused())
	case actLog:
		openPath(tc.logPath)
	case actConfig:
		openPath(tc.iniPath)
	case actNotify:
		t.toggleNotifications()
	case actUI:
		t.openUI()
	}
}

func (t *winTray) openUI() {
	if t.tc.webURL != nil {
		if u := t.tc.webURL(); u != "" {
			openPath(u)
			return
		}
	}
	t.Balloon("LocalSyncExt", "Веб-интерфейс выключен (WebUI=0) или порты не открыты", levelWarn)
}

func (t *winTray) toggleNotifications() {
	on := !t.tc.notif.enabled.Load()
	t.tc.notif.enabled.Store(on)
	saveUIState(t.tc.stateDir, uiState{Notifications: &on})
}

// ---------- контекстное меню ----------

const (
	idTitle      = 1
	idSyncAll    = 2
	idPauseAll   = 3
	idOpenLog    = 4
	idOpenConfig = 5
	idReload     = 6
	idNotify     = 7
	idAutostart  = 8
	idExit       = 9
	idOpenUI     = 10
	idRuleBase   = 1000 // + номер правила*10 + (0 — синхронизировать, 1 — пауза)
)

func appendMenu(menu uintptr, flags uint32, id uintptr, text string) {
	var p uintptr
	if flags&mfSeparate == 0 {
		p = uintptr(unsafe.Pointer(utf16(text)))
	}
	pAppendMenu.Call(menu, uintptr(flags), id, p)
}

func (t *winTray) showMenu() {
	en := t.tc.en
	menu, _, _ := pCreatePopupMenu.Call()
	defer pDestroyMenu.Call(menu)

	appendMenu(menu, mfString|mfGrayed, idTitle, "LocalSyncExt "+t.tc.version)
	appendMenu(menu, mfSeparate, 0, "")

	st := en.Statuses()
	for i, s := range st {
		sub, _, _ := pCreatePopupMenu.Call()
		usable := s.Status != "config-error" && s.Status != "disabled"
		fl := uint32(mfString)
		if !usable {
			fl |= mfGrayed
		}
		paused := en.IsPaused(s.Name)
		appendMenu(sub, fl, uintptr(idRuleBase+i*10), "Синхронизировать сейчас")
		label := "Приостановить"
		if paused {
			label = "Продолжить"
		}
		appendMenu(sub, fl, uintptr(idRuleBase+i*10+1), label)
		if s.LastErr != "" {
			appendMenu(sub, mfSeparate, 0, "")
			appendMenu(sub, mfString|mfGrayed, 0, clip(s.LastErr, 90))
		}
		appendMenu(menu, mfString|mfPopup, sub, fmt.Sprintf("● %s — %s", clip(s.Name, 40), statusRU(s.Status)))
	}
	if len(st) == 0 {
		appendMenu(menu, mfString|mfGrayed, 0, "Правил нет")
	}
	appendMenu(menu, mfSeparate, 0, "")
	appendMenu(menu, mfString, idSyncAll, "Синхронизировать всё сейчас")
	if en.AllPaused() {
		appendMenu(menu, mfString, idPauseAll, "Продолжить всё")
	} else {
		appendMenu(menu, mfString, idPauseAll, "Приостановить всё")
	}
	appendMenu(menu, mfSeparate, 0, "")
	if t.tc.webURL != nil && t.tc.webURL() != "" {
		appendMenu(menu, mfString, idOpenUI, "Открыть веб-интерфейс")
	}
	appendMenu(menu, mfString, idOpenLog, "Открыть журнал")
	appendMenu(menu, mfString, idOpenConfig, "Открыть конфиг (ini)")
	appendMenu(menu, mfString, idReload, "Перечитать конфиг сейчас")
	appendMenu(menu, mfSeparate, 0, "")
	nf := uint32(mfString)
	if t.tc.notif.enabled.Load() {
		nf |= mfChecked
	}
	appendMenu(menu, nf, idNotify, "Уведомления")
	af := uint32(mfString)
	if autostartEnabled() {
		af |= mfChecked
	}
	appendMenu(menu, af, idAutostart, "Запускать вместе с Windows (в трей)")
	appendMenu(menu, mfSeparate, 0, "")
	appendMenu(menu, mfString, idExit, "Выход")

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd, uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	pPostMessage.Call(t.hwnd, wmNull, 0, 0)
	if cmd == 0 {
		return
	}
	go t.menuCommand(int(cmd), st)
}

func (t *winTray) menuCommand(cmd int, st []RuleStatus) {
	en := t.tc.en
	switch {
	case cmd >= idRuleBase:
		i, act := (cmd-idRuleBase)/10, (cmd-idRuleBase)%10
		if i >= len(st) {
			return
		}
		name := st[i].Name
		if act == 0 {
			en.Trigger(name)
		} else {
			en.SetPaused(name, !en.IsPaused(name))
		}
	case cmd == idSyncAll:
		en.TriggerAll()
	case cmd == idPauseAll:
		en.PauseAll(!en.AllPaused())
	case cmd == idOpenUI:
		t.openUI()
	case cmd == idOpenLog:
		openPath(t.tc.logPath)
	case cmd == idOpenConfig:
		openPath(t.tc.iniPath)
	case cmd == idReload:
		if t.tc.reload != nil {
			t.tc.reload()
		}
	case cmd == idNotify:
		t.toggleNotifications()
	case cmd == idAutostart:
		if err := setAutostart(!autostartEnabled(), t.tc.exePath); err != nil {
			t.Balloon("LocalSyncExt — Ошибка", "Не удалось изменить автозапуск: "+err.Error(), levelError)
		}
	case cmd == idExit:
		t.tc.quit()
		stopTray() // WM_CLOSE: окно закроется в своём потоке
	}
}

// trayBalloon — для уведомлений движка (безопасно вызывать до запуска трея).
func trayBalloon(title, msg string, level int) {
	if t := theTray; t != nil && t.hwnd != 0 {
		t.Balloon(title, msg, level)
	}
}

// stopTray просит окно закрыться (выход по Ctrl+C или сигналу).
func stopTray() {
	if t := theTray; t != nil && t.hwnd != 0 {
		pPostMessage.Call(t.hwnd, 0x0010, 0, 0) // WM_CLOSE
	}
}
