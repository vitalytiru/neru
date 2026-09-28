// XKB translation for evdev key codes: mapping captured codes to command
// names under the configured reference layout (automatically selected by default).
//
// These are methods on the capture rather than on a reader, because the answer
// belongs to the devices and their keymap and not to whoever is reading them.
// Both readers of `/dev/input` need it and must agree: the in-mode event tap
// (evdev_session_cgo.go) and the passive global-hotkey listener
// (global_hotkey_cgo.go). One naming for one physical key is what lets a chord a
// user wrote once match in both — the listener naming keys by raw scan code while
// the tap named them by keymap meant a `[hotkeys]` binding answered a different
// physical key inside a mode than out of it on any layout that is not us.

//go:build linux && cgo

package linux

/*
#include <stdlib.h>
#include "../../platform/linux/evdev.h"
#include "../../platform/linux/wayland_keymap.h"
*/
import "C"

import (
	"strings"
	"sync/atomic"
	"unsafe"

	"go.uber.org/zap"

	"github.com/y3owk1n/neru/internal/adapter/platform"
)

// The process-wide proxy is shared by the tap and global-hotkey listener.
// Keep their reference layout shared too, including before the proxy starts.
// Atomic publication lets config reload change it without touching XKB state
// from outside the proxy's reader goroutine.
var evdevKeyboardLayout referenceKeyboardLayout

type referenceKeyboardLayout struct {
	name      atomic.Pointer[string]
	available atomic.Pointer[[]string]
}

// SetKeyboardLayout publishes configuration without touching the proxy's state.
// Validation reads immutable metadata published by the capture; no compositor
// roundtrip or reader acknowledgement can stall configuration reload.
func (et *EventTap) SetKeyboardLayout(layoutID string) bool {
	layoutID = strings.TrimSpace(layoutID)
	if !platform.DetectLinuxBackend().IsWayland() {
		return layoutID == ""
	}
	return evdevKeyboardLayout.set(layoutID)
}

func (layout *referenceKeyboardLayout) set(layoutID string) bool {
	layoutID, resolved := layout.resolve(layoutID)
	layout.name.Store(&layoutID)
	return resolved
}

func (layout *referenceKeyboardLayout) resolve(layoutID string) (string, bool) {
	resolved := layoutID == ""
	if available := layout.available.Load(); available != nil {
		for _, name := range *available {
			if strings.EqualFold(name, layoutID) {
				layoutID = name
				resolved = true
				break
			}
		}
	} else {
		// Startup config precedes proxy warm-up. Accept the request now;
		// the owning reader resolves it and warns once its keymap arrives.
		resolved = true
	}
	return layoutID, resolved
}

func (capture *waylandEvdevCapture) publishKeyboardLayouts() {
	var names []string
	for index := C.uint32_t(0); ; index++ {
		name := C.neru_xkb_state_layout_name((*C.neru_xkb_state)(capture.xkbState), index)
		if name == nil {
			break
		}
		names = append(names, C.GoString(name))
	}
	evdevKeyboardLayout.available.Store(&names)
}

// keyName resolves a scan code under the configured reference keyboard
// layout, falling back to the built-in scan-code table when there is no
// keymap to ask.
//
// The fallback is a real answer rather than a failure: it is the name the code
// carries on a us layout, which is what the table holds.
func (capture *waylandEvdevCapture) keyName(code uint16) string {
	capture.applyKeyboardLayout()
	return capture.xkbKeyName(code, true)
}

func (capture *waylandEvdevCapture) applyKeyboardLayout() {
	if capture != nil && capture.xkbState != nil {
		if name := evdevKeyboardLayout.name.Load(); name != capture.appliedKeyboardLayout {
			if name != nil {
				canonical, _ := evdevKeyboardLayout.resolve(*name)
				cname := C.CString(canonical)
				resolved := C.neru_xkb_state_set_layout(
					(*C.neru_xkb_state)(capture.xkbState),
					cname,
				) != 0
				C.free(unsafe.Pointer(cname))
				if !resolved && capture.logger != nil {
					capture.logger.Warn(
						"Configured keyboard layout was not found; using automatic fallback",
						zap.String("layout_id", *name),
					)
				}
			}
			capture.appliedKeyboardLayout = name
		}
	}
}

func (capture *waylandEvdevCapture) xkbKeyName(code uint16, command bool) string {
	if capture == nil || capture.xkbState == nil {
		return evdevKeyName(code)
	}

	var buf [64]C.char
	var result C.int
	if command {
		result = C.neru_xkb_state_key_get_command_name(
			(*C.neru_xkb_state)(capture.xkbState), C.uint16_t(code), &buf[0], C.size_t(len(buf)),
		)
	} else {
		result = C.neru_xkb_state_key_get_name(
			(*C.neru_xkb_state)(capture.xkbState), C.uint16_t(code), &buf[0], C.size_t(len(buf)),
		)
	}
	if result == 0 {
		return C.GoString(&buf[0])
	}

	return evdevKeyName(code)
}

// xkbKeysymName names a state-resolved keysym by the rule keyName applies to a
// scan code, without needing a keymap: the character it types, else the folded
// keysym name. It exists so that rule can be pinned from a test.
func xkbKeysymName(keysym uint32) string {
	var buf [64]C.char
	if C.neru_xkb_keysym_name(C.uint32_t(keysym), &buf[0], C.size_t(len(buf))) != 0 {
		return ""
	}

	return C.GoString(&buf[0])
}

// modifierName returns the canonical evdev modifier name for the given scan code
// as resolved by the XKB keymap, or empty string when the key is not a modifier.
// When XKB remaps a physical modifier to a different function (e.g. ctrl:swapcaps
// makes Caps Lock act as Control), this returns the remapped modifier name so the
// reader tracks the correct modifier.
func (capture *waylandEvdevCapture) modifierName(code uint16) string {
	if capture == nil || capture.xkbState == nil {
		return evdevModifierName(code)
	}

	// Physical modifiers follow the live layout, including XKB remaps.
	key := capture.xkbKeyName(code, false)
	if key == "" {
		return evdevModifierName(code)
	}

	switch key {
	case "Shift_L", "Shift_R":
		return evdevModifierShift
	case "Control_L", "Control_R":
		return evdevModifierCtrl
	case "Alt_L", "Alt_R":
		return evdevModifierAlt
	case "Meta_L", "Meta_R", "Super_L", "Super_R", "Hyper_L", "Hyper_R":
		return evdevModifierCmd
	}

	return ""
}

// feedKey tells xkb_state a key went down or up, so its idea of the lock
// modifiers and the layout group keeps up with the keyboard. A reader that
// resolves names without feeding them gets the names the layout had when the
// state was built and never learns of a change.
func (capture *waylandEvdevCapture) feedKey(code uint16, isDown bool) {
	if capture == nil || capture.xkbState == nil {
		return
	}

	down := C.int(0)
	if isDown {
		down = C.int(1)
	}

	C.neru_xkb_state_key((*C.neru_xkb_state)(capture.xkbState), C.uint16_t(code), down)
}

// pollKeymap asks the compositor connection behind the xkb state for anything
// it sent since the last call, without blocking, and rebuilds the state when
// the keymap was replaced or the connection is gone. It is what keeps the
// names the proxy resolves following a layout the user changes mid-session,
// now that the state is built once rather than at every mode start.
//
// It reports whether the state is fresh. A fresh state has no key history: the
// caller, which knows what is held, feeds that back in.
func (capture *waylandEvdevCapture) pollKeymap() bool {
	// A state that never came up already said so once; asking again every
	// two seconds would say it again every two seconds.
	if capture == nil || capture.xkbState == nil {
		return false
	}

	switch C.neru_xkb_state_dispatch((*C.neru_xkb_state)(capture.xkbState)) {
	case 0:
		return false
	case 1:
		capture.publishKeyboardLayouts()
		capture.appliedKeyboardLayout = nil
		capture.applyKeyboardLayout()
		// The state under the keymap is fresh, so its lock modifiers are not:
		// read them off the devices again.
		capture.syncLeds()
	default:
		capture.refreshXkbState()
	}

	return true
}

// refreshXkbState rebuilds xkb_state from the compositor's keymap, then syncs
// lock modifiers from the devices' LEDs. It runs once when the proxy starts
// and again whenever pollKeymap finds the connection gone.
func (capture *waylandEvdevCapture) refreshXkbState() {
	if capture == nil {
		return
	}

	if capture.xkbState != nil {
		C.neru_xkb_state_destroy((*C.neru_xkb_state)(capture.xkbState))
	}

	xkb := C.neru_xkb_state_create()
	capture.xkbState = unsafe.Pointer(xkb)
	capture.appliedKeyboardLayout = nil
	capture.publishKeyboardLayouts()

	if xkb == nil {
		if capture.logger != nil {
			capture.logger.Error(
				"Failed to initialize Wayland xkb_state; XKB options will be ignored, " +
					"falling back to hardcoded evdev key names",
			)
		}

		return
	}

	capture.applyKeyboardLayout()
	capture.syncLeds()
}

// syncLeds sets the xkb state's lock modifiers from the devices' LEDs, which
// are the kernel's word on NumLock and CapsLock at a moment the state has no
// key history for.
func (capture *waylandEvdevCapture) syncLeds() {
	if capture == nil || capture.xkbState == nil {
		return
	}

	numLock := C.int(0)
	capsLock := C.int(0)

	capture.deviceMu.Lock()
	for _, file := range capture.files {
		fd := C.int(file.Fd())
		if C.neru_evdev_led_is_on(fd, C.uint(0)) != 0 {
			numLock = 1
		}

		if C.neru_evdev_led_is_on(fd, C.uint(1)) != 0 {
			capsLock = 1
		}
	}
	capture.deviceMu.Unlock()

	C.neru_xkb_state_sync_leds((*C.neru_xkb_state)(capture.xkbState), numLock, capsLock)
}
