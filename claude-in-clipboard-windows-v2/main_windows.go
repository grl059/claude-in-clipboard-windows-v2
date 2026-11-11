//go:build windows

// ClaudeClip for Windows: tray app. Tap the trigger key (Right Ctrl by default), type a prompt blind,
// press Enter. Claude Code reads your current VS Code project (or the file open in Notepad) and puts the result on the clipboard.
// Double-tap clears the clipboard; holding the key previews the clipboard.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"claudeclip/internal/logic"
)

const (
	wmTray    = wmApp + 0 // tray icon mouse events
	wmDone    = wmApp + 1 // a claude run finished
	wmTap     = wmApp + 2 // the trigger key was tapped
	wmStarted = wmApp + 3 // a job found its project
	wmRefresh = wmApp + 4 // redraw the tray icon (done later so key events are never kept waiting)

	timerGhostShow  = 1
	timerGhostWatch = 2
	timerRevert     = 3
)

var (
	mainHwnd           uintptr
	taskbarCreatedMsg  uintptr
	hook               uintptr
	mouseHook          uintptr
	debugPrompt        bool
	cfg                settings
	tap                logic.TapDetector
	triggerDown        bool
	activeVK           uint32 // which trigger key is currently held
	ghostPending       bool
	lastTapEvt         float64 // event time of the previous tap, for double-tap detection
	tickHi             uint64
	lastTick           uint32
	task               *job
	taskID             int
	infoText           = "Ready"
	startedAt          = time.Now()
	hookCallback       uintptr
	singletonMutexName = u16(`Local\ClaudeClipSingleton`)
)

// appVersion is the ClaudeClip release (version 2: Left Ctrl is the trigger).
const appVersion = "2"

func init() { runtime.LockOSThread() } // window and hook callbacks must stay on one thread

func nowSec() float64 { return time.Since(startedAt).Seconds() }

func main() {
	if _, _, err := pCreateMutexW.Call(0, 0, ptr(unsafe.Pointer(singletonMutexName))); err == syscall.Errno(183) {
		return // already running
	}
	pSetProcessDpiAwarenessCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	for _, a := range os.Args[1:] {
		if a == "--show-prompt" {
			debugPrompt = true
		}
	}
	if os.Getenv("CLAUDECLIP_DEBUG") == "1" {
		debugPrompt = true
	}
	initLog()
	cfg = loadSettings()
	infoText = readyText()
	logf("start: ClaudeClip v%s model=%s trigger=%s debugPrompt=%v args=%v", appVersion, cfg.Model, cfg.Trigger, debugPrompt, os.Args[1:])

	class := u16("ClaudeClipMain")
	registerClass(class, mainProc)
	mainHwnd, _, _ = pCreateWindowExW.Call(wsExToolWindow, ptr(unsafe.Pointer(class)),
		ptr(unsafe.Pointer(u16("ClaudeClip"))), 0, 0, 0, 0, 0, 0, 0, getModuleHandle(), 0)
	createGhost()
	createPrompt()
	taskbarCreatedMsg, _, _ = pRegisterWindowMessageW.Call(ptr(unsafe.Pointer(u16("TaskbarCreated"))))
	lastLightMode = taskbarIsLight()
	trayNotify(nimAdd)

	// Watching the trigger key system-wide needs a low-level keyboard hook (no special permission).
	hookCallback = syscall.NewCallback(keyboardProc)
	hook, _, _ = pSetWindowsHookExW.Call(whKeyboardLL, hookCallback, getModuleHandle(), 0)
	// Ctrl+click and Ctrl+scroll (zoom) are shortcuts too: a mouse hook cancels a pending tap.
	mouseHook, _, _ = pSetWindowsHookExW.Call(whMouseLL, syscall.NewCallback(mouseProc), getModuleHandle(), 0)
	logf("keyboard hook installed: %v, mouse hook: %v", hook != 0, mouseHook != 0)
	if hook == 0 {
		fail("Could not install the keyboard hook")
	}
	refreshAccount()

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(ptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(ptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(ptr(unsafe.Pointer(&m)))
	}
}

// MARK: keyboard

func keyboardProc(nCode, wp, lp uintptr) uintptr {
	defer guard("keyboardProc")
	if int32(nCode) == 0 { // HC_ACTION
		k := (*kbdLLHook)(fromPtr(lp))
		down := wp == wmKeyDown || wp == wmSysKeyDown
		up := wp == wmKeyUp || wp == wmSysKeyUp
		if (down || up) && handleKey(k.vkCode, down, k.time) {
			return 1 // swallow (only ever the trigger key itself, and only for Caps Lock)
		}
	}
	r, _, _ := pCallNextHookEx.Call(hook, nCode, wp, lp)
	return r
}

func mouseProc(nCode, wp, lp uintptr) uintptr {
	defer guard("mouseProc")
	if int32(nCode) == 0 {
		switch wp {
		case 0x201, 0x204, 0x207, 0x20A, 0x20B, 0x20E: // any button press or wheel turn (not plain movement)
			tap.OtherKey() // the ghost, if showing, stays: you scroll it with the wheel while holding the key
		}
	}
	r, _, _ := pCallNextHookEx.Call(mouseHook, nCode, wp, lp)
	return r
}

// eventSec converts the key event's own timestamp (ms since boot) to seconds. Using the event time, not
// the time we got around to handling it, keeps tap/double-tap timing right even if this thread was busy.
func eventSec(tick uint32) float64 {
	if tick < lastTick && lastTick-tick > 1<<31 { // 49-day counter wrap
		tickHi += 1 << 32
	}
	lastTick = tick
	return float64(tickHi+uint64(tick)) / 1000
}

// handleKey runs inside the hook, so it only does cheap work; taps are posted to the message loop.
// It returns true when the key event must not reach other apps.
func handleKey(vk uint32, down bool, tick uint32) bool {
	trig := currentTrigger(cfg)
	if !trig.has(vk) { // Ctrl+C etc. is a shortcut, not a tap or a hold
		if down {
			tap.OtherKey()
			hideGhost()
		}
		return false
	}
	t := eventSec(tick)
	swallow := trig.vks[0] == vkCapital // Caps Lock must not also toggle caps
	if down {
		if triggerDown && (swallow || keyPhysicallyDown(activeVK)) { // the trigger is still held
			if vk != activeVK { // e.g. left and right Ctrl together: a chord, not a tap
				tap.OtherKey()
				hideGhost()
			}
			return swallow // otherwise: auto-repeat
		}
		logf("trigger down  vk=%#x t=%.3f", vk, t)
		triggerDown, activeVK = true, vk
		scheduleGhost()
		tap.Key(true, t)
		return swallow
	}
	if !triggerDown || vk != activeVK {
		return swallow // release of a key we weren't tracking
	}
	triggerDown = false
	hideGhost()
	if tap.Key(false, t) {
		double := t-lastTapEvt < logic.DoubleTapWindow // decided here, from event times
		if double {
			lastTapEvt = 0
		} else {
			lastTapEvt = t
		}
		logf("trigger tap   t=%.3f double=%v", t, double)
		postMessage(mainHwnd, wmTap, boolInt(double), 0)
	} else {
		logf("trigger up    t=%.3f (not a tap)", t)
	}
	return swallow
}

func keyPhysicallyDown(vk uint32) bool {
	r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
	return uint16(r)&0x8000 != 0
}

// MARK: main window

func mainProc(hwnd, m, wp, lp uintptr) uintptr {
	defer guard("mainProc")
	if m == taskbarCreatedMsg && m != 0 { // Explorer restarted: put the icon back
		trayNotify(nimAdd)
		return 0
	}
	switch uint32(m) {
	case wmTray:
		if e := loword(lp); e == wmLButtonUp || e == wmRButtonUp {
			showMenu()
		}
		return 0
	case wmTap:
		onTap(wp != 0)
		return 0
	case wmRefresh:
		trayNotify(nimModify)
		return 0
	case wmDone:
		handleDone()
		return 0
	case wmStarted:
		if task != nil && task.target().found() {
			infoText = fmt.Sprintf("Working in %s with %s…", task.target().name, modelName())
			trayNotify(nimModify)
		}
		return 0
	case wmCommand:
		onCommand(int(loword(wp)))
		return 0
	case wmSettingChg:
		if l := taskbarIsLight(); l != lastLightMode { // taskbar theme changed: redraw the glyph in the right colour
			lastLightMode = l
			trayNotify(nimModify)
		}
		return 0
	case wmTimer:
		switch wp {
		case timerGhostShow:
			killTimer(hwnd, timerGhostShow)
			ghostPending = false
			if triggerDown {
				showGhost()
			}
		case timerGhostWatch:
			if gh.visible && currentTrigger(cfg).vks[0] != vkCapital && !keyPhysicallyDown(activeVK) {
				triggerDown = false
				hideGhost()
			}
		case timerRevert:
			killTimer(hwnd, timerRevert)
			setIcon(idleIcon())
		}
		return 0
	case wmDestroy:
		logf("quit (window destroyed)")
		if hook != 0 {
			pUnhookWindowsHookEx.Call(hook)
		}
		if mouseHook != 0 {
			pUnhookWindowsHookEx.Call(mouseHook)
		}
		if task != nil {
			task.kill()
		}
		trayNotify(nimDelete)
		pPostQuitMessage.Call(0)
		return 0
	}
	return defWindowProc(hwnd, m, wp, lp)
}

// MARK: actions

func onTap(double bool) {
	logf("tap handled: double=%v prompt visible=%v", double, prompt.visible)
	if double { // the first tap opened the prompt, so close it and clear
		hidePrompt()
		clearClipboard()
		clearClipboardHistory() // Win+V history keeps old copies otherwise
		messageBeep(0)          // MB_OK
		flash("trash", "Clipboard cleared")
		return
	}
	if prompt.visible {
		hidePrompt()
	} else {
		showPrompt()
	}
}

func modelName() string { return strings.ToUpper(cfg.Model[:1]) + cfg.Model[1:] }

func runPrompt(text string) {
	logf("run prompt (%d chars)", len([]rune(text)))
	if task != nil {
		task.kill() // a new prompt replaces a running one
	}
	taskID++
	task = startJob(taskID, text, cfg.Model)
	killTimer(mainHwnd, timerRevert)
	infoText = "Working with " + modelName() + "…"
	setIcon("hourglass")
}

func handleDone() {
	for _, r := range takeResults() {
		logf("claude finished: id=%d current=%d code=%d stdout=%d bytes stderr=%q", r.id, taskID, r.code, len(r.out), r.err)
		if r.id != taskID { // replaced by a newer prompt
			continue
		}
		task = nil
		code := logic.StripFences(r.out)
		if r.code != 0 || code == "" {
			msg := r.err
			if strings.TrimSpace(msg) == "" {
				msg = r.out
			}
			if strings.TrimSpace(msg) == "" {
				msg = fmt.Sprintf("claude exited with code %d", r.code)
			}
			fail(msg)
			continue
		}
		if err := setClipboardText(code); err != nil {
			fail(err.Error())
			continue
		}
		messageBeep(0x40) // MB_ICONASTERISK
		flash("checkmark.circle", fmt.Sprintf("Copied %d lines", strings.Count(code, "\n")+1))
	}
}
