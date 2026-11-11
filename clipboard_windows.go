//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"claudeclip/internal/logic"
)

var pGlobalSize = kernel32.NewProc("GlobalSize")

// The clipboard is a shared resource: another app may hold it for a moment, so retry briefly.
func openClipboard() bool {
	for i := 0; i < 20; i++ {
		if r, _, _ := pOpenClipboard.Call(mainHwnd); r != 0 { // our window owns what we put on it
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// clipboardText returns the clipboard's text with \n line endings ("" if there is none).
func clipboardText() string {
	if r, _, _ := pIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return ""
	}
	if !openClipboard() {
		return ""
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer pGlobalUnlock.Call(h)
	bytes, _, _ := pGlobalSize.Call(h)
	units := unsafe.Slice((*uint16)(fromPtr(p)), int(bytes)/2)
	n := 0
	for n < len(units) && units[n] != 0 {
		n++
	}
	return logic.NormalizeNewlines(string(utf16.Decode(units[:n])))
}

func setClipboardText(s string) error {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(logic.NormalizeNewlines(s), "\n", "\r\n") // Windows apps expect CRLF
	units := append(utf16.Encode([]rune(s)), 0)
	if !openClipboard() {
		return errors.New("clipboard is busy")
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	h, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(units)*2))
	if h == 0 {
		return errors.New("out of memory")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return errors.New("could not lock clipboard memory")
	}
	copy(unsafe.Slice((*uint16)(fromPtr(p)), len(units)), units)
	pGlobalUnlock.Call(h)
	if r, _, _ := pSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		pGlobalFree.Call(h) // the system only takes ownership on success
		return errors.New("could not set clipboard data")
	}
	return nil
}

func clearClipboard() {
	if !openClipboard() {
		logf("clear clipboard: could not open it (another app holds it)")
		return
	}
	r, _, _ := pEmptyClipboard.Call()
	pCloseClipboard.Call()
	logf("clear clipboard: ok=%v", r != 0)
}

// clearClipboardHistory also wipes Windows' clipboard history (the Win+V list), which keeps every copy
// until cleared and is not touched by emptying the clipboard itself. The only supported way is the WinRT
// Clipboard.ClearHistory(), reached through Windows PowerShell 5.1 (no elevation needed). Pinned items stay.
func clearClipboardHistory() {
	go func() {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		ps := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command",
			"[Windows.ApplicationModel.DataTransfer.Clipboard, Windows, ContentType = WindowsRuntime]::ClearHistory()")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		out, err := cmd.CombinedOutput()
		logf("clear clipboard history: err=%v output=%q", err, strings.TrimSpace(string(out)))
	}()
}

// clipboardConcealed: password managers tag copied secrets so clipboard history/monitors skip them.
func clipboardConcealed() bool {
	f, _, _ := pRegisterClipboardFormatW.Call(ptr(unsafe.Pointer(u16("ExcludeClipboardContentFromMonitorProcessing"))))
	if f == 0 {
		return false
	}
	r, _, _ := pIsClipboardFormatAvailable.Call(f)
	return r != 0
}
