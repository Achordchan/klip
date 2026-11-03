//go:build windows

package shelf

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	whMouseLL     = 14
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmQuit        = 0x0012
	vkShift       = 0x10
	vkControl     = 0x11
	vkMenu        = 0x12
)

const vkLButton = 0x01 // VK_LBUTTON

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	ntdll             = syscall.NewLazyDLL("ntdll.dll")
	procSetHook       = user32.NewProc("SetWindowsHookExW")
	procUnhook        = user32.NewProc("UnhookWindowsHookEx")
	procCallNext      = user32.NewProc("CallNextHookEx")
	procPeekMessage   = user32.NewProc("PeekMessageW")
	procGetMessage    = user32.NewProc("GetMessageW")
	procTranslateMsg  = user32.NewProc("TranslateMessage")
	procDispatchMsg   = user32.NewProc("DispatchMessageW")
	procPostThreadMsg = user32.NewProc("PostThreadMessageW")
	procAsyncKey      = user32.NewProc("GetAsyncKeyState")
	procThreadID      = kernel32.NewProc("GetCurrentThreadId")
	procGetTickCount  = kernel32.NewProc("GetTickCount")
	procMoveMemory    = ntdll.NewProc("RtlMoveMemory")
)

// mouseEvent is what the hook callback hands over: it must return at once,
// so the work is done elsewhere.
type mouseEvent struct {
	kind int // wm* message
	x, y int32
	tick uint32
}

var (
	hookEvents chan mouseEvent
	// hookOverflow is set when a button event did not fit in the queue.
	hookOverflow atomic.Bool
	hookHandle   uintptr
	hookOnce     sync.Once
	hookProc     uintptr
)

type msllhookstruct struct {
	X, Y        int32
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

func lowLevelMouseProc(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) >= 0 {
		switch wParam {
		case wmMouseMove:
			var st msllhookstruct
			procMoveMemory.Call(uintptr(unsafe.Pointer(&st)), lParam, unsafe.Sizeof(st))
			select {
			case hookEvents <- mouseEvent{wmMouseMove, st.X, st.Y, st.Time}:
			default: // the consumer is behind: a move is not worth waiting for
			}
		case wmLButtonDown, wmLButtonUp:
			var st msllhookstruct
			procMoveMemory.Call(uintptr(unsafe.Pointer(&st)), lParam, unsafe.Sizeof(st))
			// A press or a release must not be lost, or the gesture would
			// think the button is still down; a full queue is not waited for
			// here, but 4096 events are a lot.
			select {
			case hookEvents <- mouseEvent{kind: int(wParam), tick: st.Time}:
			default:
				// It could not be queued: say so, and the consumer will not
				// trust what it believes about the button (see below).
				hookOverflow.Store(true)
			}
		}
	}
	r, _, _ := procCallNext.Call(hookHandle, nCode, wParam, lParam)
	return r
}

// StartHook installs a global low-level mouse hook on a thread of its own
// (the hook needs a message loop) and calls ev for each move and for the left
// button going down and up. The returned function removes it.
func StartHook(ev Events) (stop func(), err error) {
	hookOnce.Do(func() { hookProc = syscall.NewCallback(lowLevelMouseProc) })
	hookEvents = make(chan mouseEvent, 4096)

	ready := make(chan error, 1)
	tid := make(chan uint32, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		h, _, e := procSetHook.Call(whMouseLL, hookProc, 0, 0)
		if h == 0 {
			ready <- e
			return
		}
		hookHandle = h
		var queueMsg [48]byte
		procPeekMessage.Call(uintptr(unsafe.Pointer(&queueMsg[0])), 0, 0, 0, 0)
		id, _, _ := procThreadID.Call()
		tid <- uint32(id)
		ready <- nil
		var msg [48]byte // MSG: large enough for both 32 and 64 bits
		for {
			r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
			if int32(r) <= 0 { // WM_QUIT (0) or an error (-1)
				break
			}
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
		}
		procUnhook.Call(h)
		hookHandle = 0
	}()
	if err := <-ready; err != nil {
		<-done
		return nil, err
	}
	threadID := <-tid

	quit := make(chan struct{})
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		var clock mouseClock
		for {
			select {
			case <-quit:
				return
			case m := <-hookEvents:
				if hookOverflow.Swap(false) {
					// A press or release was lost when the queue was full. What
					// is queued may be older than that, so it is dropped, and the
					// button is asked for as it is now.
				drain:
					for {
						select {
						case <-hookEvents:
						default:
							break drain
						}
					}
					tick, _, _ := procGetTickCount.Call()
					at := clock.at(uint32(tick))
					if down, _, _ := procAsyncKey.Call(vkLButton); down&0x8000 != 0 {
						if ev.LeftDown != nil {
							ev.LeftDown(at)
						}
					} else if ev.LeftUp != nil {
						ev.LeftUp(at)
					}
					continue
				}
				at := clock.at(m.tick)
				switch m.kind {
				case wmMouseMove:
					if ev.Move != nil {
						ev.Move(int(m.x), int(m.y), at)
					}
				case wmLButtonDown:
					if ev.LeftDown != nil {
						ev.LeftDown(at)
					}
				case wmLButtonUp:
					if ev.LeftUp != nil {
						ev.LeftUp(at)
					}
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			posted, _, _ := procPostThreadMsg.Call(uintptr(threadID), wmQuit, 0, 0)
			if posted == 0 {
				procUnhook.Call(hookHandle)
				close(quit)
				<-consumed
				return
			}
			<-done
			close(quit)
			<-consumed
		})
	}, nil
}

// ModifierDown says whether the trigger key ("Ctrl", "Alt" or "Shift") is
// down right now, as ShelfService.IsModifierPressed asks GetAsyncKeyState.
func ModifierDown(modifier string) bool {
	vk := uintptr(vkControl)
	switch modifier {
	case "Alt":
		vk = vkMenu
	case "Shift":
		vk = vkShift
	case "Disabled":
		return false
	}
	r, _, _ := procAsyncKey.Call(vk)
	return uint16(r)&0x8000 != 0
}
