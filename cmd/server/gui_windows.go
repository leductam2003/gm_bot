//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pSetWindowLong     = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProc    = user32.NewProc("CallWindowProcW")
	pSetWindowPos      = user32.NewProc("SetWindowPos")
	pShowWindow        = user32.NewProc("ShowWindow")
	pSendMessage       = user32.NewProc("SendMessageW")
	pReleaseCapture    = user32.NewProc("ReleaseCapture")
	pGetWindowRect     = user32.NewProc("GetWindowRect")
	pMonitorFromWindow = user32.NewProc("MonitorFromWindow")
	pGetMonitorInfo    = user32.NewProc("GetMonitorInfoW")
	pGetConsoleWin     = kernel32.NewProc("GetConsoleWindow")
)

const (
	gwlpWndProc = ^uintptr(3) // -4

	swpNoMove       = 0x0002
	swpNoSize       = 0x0001
	swpNoZorder     = 0x0004
	swpFrameChanged = 0x0020
	swHide          = 0
	swMinimize      = 6

	wmEraseBkgnd    = 0x0014
	wmNCCalcSize    = 0x0083
	wmNCHitTest     = 0x0084
	wmNCLButtonDown = 0x00A1

	htClient    = 1
	htCaption   = 2
	htLeft      = 10
	htRight     = 11
	htTop       = 12
	htTopLeft   = 13
	htTopRight  = 14
	htBottom    = 15
	htBotLeft   = 16
	htBotRight  = 17
	resizeBand  = 6
)

type winRect struct{ left, top, right, bottom int32 }

type monitorInfo struct {
	cbSize    uint32
	rcMonitor winRect
	rcWork    winRect
	dwFlags   uint32
}

var (
	origWndProc uintptr
	savedRect   winRect
	maximized   bool
)

// launchGUI opens a frameless, resizable native window (WebView2) at url and blocks
// until it closes. The page's yellow/green/red controls drive the window via the
// bound win* functions. Returns false if no window can be created.
func launchGUI(url, title string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	// Paint the WebView2 control's own default background dark (ARGB) so there is no
	// white flash before/while the page repaints on tab switches. Read by the WebView2
	// runtime at environment creation — must be set before NewWithOptions.
	if os.Getenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR") == "" {
		_ = os.Setenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR", "FF070A0B")
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{Title: title, Width: 1320, Height: 880, Center: true},
	})
	if w == nil {
		return false
	}
	defer w.Destroy()
	hwnd := uintptr(w.Window())

	// Borderless: subclass the window proc so WM_NCCALCSIZE removes the non-client
	// frame (no white border / title bar) while WM_NCHITTEST keeps edge-resize.
	cb := syscall.NewCallback(func(h, msg, wparam, lparam uintptr) uintptr {
		switch msg {
		case wmEraseBkgnd:
			// Skip the default (white) background erase — the WebView2 child paints the
			// whole client area, so erasing first only causes a white flash on repaint.
			return 1
		case wmNCCalcSize:
			if wparam != 0 {
				return 0 // client area = whole window (frame removed)
			}
		case wmNCHitTest:
			return hitTest(h, lparam)
		}
		ret, _, _ := pCallWindowProc.Call(origWndProc, h, msg, wparam, lparam)
		return ret
	})
	origWndProc, _, _ = pSetWindowLong.Call(hwnd, gwlpWndProc, cb)
	pSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZorder|swpFrameChanged)

	w.Bind("winMinimize", func() { pShowWindow.Call(hwnd, swMinimize) })
	w.Bind("winMaximize", func() { toggleMaximize(hwnd) })
	w.Bind("winClose", func() { w.Terminate() })
	w.Bind("winDrag", func() {
		pReleaseCapture.Call()
		pSendMessage.Call(hwnd, wmNCLButtonDown, htCaption, 0)
	})

	hideConsole()
	w.Navigate(url)
	w.Run()
	return true
}

// hitTest reports resize edges/corners near the window border, else client area.
func hitTest(hwnd, lparam uintptr) uintptr {
	x := int32(int16(lparam & 0xFFFF))
	y := int32(int16((lparam >> 16) & 0xFFFF))
	r := getWindowRect(hwnd)
	left := x < r.left+resizeBand
	right := x >= r.right-resizeBand
	top := y < r.top+resizeBand
	bottom := y >= r.bottom-resizeBand
	switch {
	case top && left:
		return htTopLeft
	case top && right:
		return htTopRight
	case bottom && left:
		return htBotLeft
	case bottom && right:
		return htBotRight
	case left:
		return htLeft
	case right:
		return htRight
	case top:
		return htTop
	case bottom:
		return htBottom
	}
	return htClient
}

func getWindowRect(hwnd uintptr) winRect {
	var r winRect
	pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// toggleMaximize fills the current monitor's work area (or restores). A manual
// resize avoids the off-screen clipping a real SW_MAXIMIZE causes with a frame
// removed by WM_NCCALCSIZE.
func toggleMaximize(hwnd uintptr) {
	if maximized {
		pSetWindowPos.Call(hwnd, 0, uintptr(savedRect.left), uintptr(savedRect.top),
			uintptr(savedRect.right-savedRect.left), uintptr(savedRect.bottom-savedRect.top),
			swpNoZorder|swpFrameChanged)
		maximized = false
		return
	}
	savedRect = getWindowRect(hwnd)
	wa := workArea(hwnd)
	pSetWindowPos.Call(hwnd, 0, uintptr(wa.left), uintptr(wa.top),
		uintptr(wa.right-wa.left), uintptr(wa.bottom-wa.top), swpNoZorder|swpFrameChanged)
	maximized = true
}

func workArea(hwnd uintptr) winRect {
	const monitorDefaultToNearest = 2
	hmon, _, _ := pMonitorFromWindow.Call(hwnd, monitorDefaultToNearest)
	mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	pGetMonitorInfo.Call(hmon, uintptr(unsafe.Pointer(&mi)))
	if mi.rcWork.right == 0 {
		return winRect{0, 0, 1280, 800}
	}
	return mi.rcWork
}

// hideConsole hides the console window so a double-clicked exe shows only the GUI.
func hideConsole() {
	hwnd, _, _ := pGetConsoleWin.Call()
	if hwnd != 0 {
		pShowWindow.Call(hwnd, swHide)
	}
}
