package aggregate

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The public DOM key/code pair is converted to X11 keysyms here. Numeric DOM
// keyCode is deliberately not accepted: e.g. keyCode 65 is not keysym 'a'.
var namedKeysyms = map[string]uint32{
	"Backspace": 0xff08, "Tab": 0xff09, "Enter": 0xff0d, "Pause": 0xff13, "ScrollLock": 0xff14, "Escape": 0xff1b,
	"Home": 0xff50, "ArrowLeft": 0xff51, "ArrowUp": 0xff52, "ArrowRight": 0xff53, "ArrowDown": 0xff54, "PageUp": 0xff55, "PageDown": 0xff56, "End": 0xff57,
	"PrintScreen": 0xff61, "Insert": 0xff63, "ContextMenu": 0xff67, "NumLock": 0xff7f, "Delete": 0xffff,
	"Shift": 0xffe1, "Control": 0xffe3, "CapsLock": 0xffe5, "Meta": 0xffeb, "OS": 0xffeb, "Alt": 0xffe9, "AltGraph": 0xfe03,
	"Compose": 0xff20, "Help": 0xff6a, "Clear": 0xff0b, "Cancel": 0xff69,
}

func keysym(key, code string) (uint32, error) {
	if len(key) > 128 || len(code) > 128 {
		return 0, errors.New("invalid keyboard key")
	}
	if value, ok := namedKeysyms[key]; ok {
		if strings.HasSuffix(code, "Right") && (key == "Shift" || key == "Control" || key == "Alt" || key == "Meta" || key == "OS") {
			value++
		}
		if code == "NumpadEnter" {
			value = 0xff8d
		}
		return value, nil
	}
	if strings.HasPrefix(key, "F") {
		if n, err := strconv.Atoi(strings.TrimPrefix(key, "F")); err == nil && n >= 1 && n <= 35 {
			return 0xffbd + uint32(n), nil
		}
	}
	if utf8.ValidString(key) && utf8.RuneCountInString(key) == 1 {
		r, _ := utf8.DecodeRuneInString(key)
		if r >= 0x20 && r <= 0xff && r != 0x7f {
			return uint32(r), nil
		}
		if r > 0xff && r <= utf8.MaxRune {
			return 0x01000000 | uint32(r), nil
		}
	}
	return 0, errors.New("unsupported keyboard key; expected a DOM key, not a keyCode")
}

func dcFrame(op byte, body []byte) []byte {
	data := make([]byte, 3+len(body))
	data[0] = op
	binary.BigEndian.PutUint16(data[1:3], uint16(len(body)))
	copy(data[3:], body)
	return data
}
func dcUint32(op byte, value uint32) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, value)
	return dcFrame(op, body)
}
func dcMove(x, y int) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint16(body, uint16(x))
	binary.BigEndian.PutUint16(body[2:], uint16(y))
	return dcFrame(1, body)
}

func (v *viewer) clearInput() {
	v.inputMu.Lock()
	defer v.inputMu.Unlock()
	clear(v.keys)
	clear(v.buttons)
}

func (v *viewer) input(data []byte) error {
	var message struct {
		Type   string   `json:"type"`
		Action string   `json:"action"`
		X      *int     `json:"x"`
		Y      *int     `json:"y"`
		Button *int     `json:"button"`
		DeltaX *float64 `json:"deltaX"`
		DeltaY *float64 `json:"deltaY"`
		Key    string   `json:"key"`
		Code   string   `json:"code"`
		Repeat bool     `json:"repeat"`
		Alt    bool     `json:"altKey"`
		Ctrl   bool     `json:"ctrlKey"`
		Shift  bool     `json:"shiftKey"`
		Meta   bool     `json:"metaKey"`
		Text   string   `json:"text"`
	}
	if decodeJSON(data, &message) != nil {
		return errors.New("invalid viewer input JSON")
	}
	// Lock pressed-input state before taking a fresh ownership snapshot, so a
	// host-change clear cannot run first and then be repopulated by stale input.
	v.inputMu.Lock()
	defer v.inputMu.Unlock()
	v.stateMu.Lock()
	s := v.state
	v.stateMu.Unlock()
	if v.ctx.Err() != nil || !s.allowed() {
		return errors.New("viewer session is no longer authorized")
	}
	switch message.Type {
	case "control":
		if message.Action != "request" && message.Action != "release" {
			return errors.New("unsupported control action")
		}
		if message.Action == "request" && !s.Profile.CanHost {
			return errors.New("native room does not allow this viewer to control")
		}
		if message.Action == "release" {
			clear(v.keys)
			clear(v.buttons)
		}
		// Acquisition is explicit and native-authorized. Never manufacture a
		// host acknowledgement; wait for the authoritative control/host event.
		return v.sendNative("control/"+message.Action, nil)
	case "clipboard":
		if s.Host != s.ID || !s.Profile.CanAccessClipboard {
			return errors.New("clipboard requires native control and clipboard permission")
		}
		if len(message.Text) > 64<<10 {
			return errors.New("clipboard text too large")
		}
		return v.sendNative("clipboard/set", map[string]string{"text": message.Text})
	case "pointer", "key":
		// This local check avoids stale/inactive events, but is NOT the
		// authority: native DC checks IsHost atomically at delivery too.
		if s.Host != s.ID || !s.Profile.CanHost {
			return errors.New("input requires native control ownership")
		}
	default:
		// In particular do not accept the native envelope or public signal/*.
		return errors.New("unsupported viewer message")
	}
	if v.peer == nil {
		return ErrInputUnavailable
	}
	if message.Type == "key" {
		if message.Action != "down" && message.Action != "up" {
			return errors.New("unsupported key action")
		}
		value, err := keysym(message.Key, message.Code)
		if err != nil {
			return err
		}
		physical := message.Code
		if physical == "" {
			physical = message.Key
		}
		if held, ok := v.keys[physical]; ok {
			value = held
		} else if message.Action == "up" {
			return nil
		} else if len(v.keys) >= 256 {
			return errors.New("too many pressed keys")
		} // no stale replay
		op := byte(3)
		if message.Action == "up" {
			op = 4
		}
		if err := v.peer.SendData(dcUint32(op, value)); err != nil {
			clear(v.keys)
			clear(v.buttons)
			return ErrInputUnavailable
		}
		if op == 3 {
			v.keys[physical] = value
		} else {
			delete(v.keys, physical)
		}
		return nil
	}
	if message.Action != "move" && message.Action != "down" && message.Action != "up" && message.Action != "wheel" {
		return errors.New("unsupported pointer action")
	}
	if message.X == nil || message.Y == nil || *message.X < 0 || *message.X >= s.Width || *message.Y < 0 || *message.Y >= s.Height {
		return errors.New("pointer coordinates outside native desktop")
	}
	var following []byte
	switch message.Action {
	case "down", "up":
		if message.Button == nil || *message.Button < 0 || *message.Button > 4 {
			return errors.New("unsupported DOM mouse button")
		}
		// DOM left/middle/right are 0/1/2; X11 left/middle/right are 1/2/3.
		// DOM auxiliary buttons are 3/4, X11 8/9 (4..7 are wheel buttons).
		button := *message.Button + 1
		if button >= 4 {
			button += 4
		}
		op := byte(5)
		if message.Action == "up" {
			op = 6
			if !v.buttons[*message.Button] {
				return nil
			}
		}
		following = dcUint32(op, uint32(button))
	case "wheel":
		if message.DeltaX == nil || message.DeltaY == nil {
			return errors.New("wheel deltas required")
		}
		x, y := math.Round(*message.DeltaX), math.Round(*message.DeltaY)
		if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || x < math.MinInt16 || x > math.MaxInt16 || y < math.MinInt16 || y > math.MaxInt16 {
			return errors.New("wheel deltas out of range")
		}
		body := make([]byte, 5)
		binary.BigEndian.PutUint16(body, uint16(int16(x)))
		binary.BigEndian.PutUint16(body[2:], uint16(int16(y)))
		for _, key := range v.keys {
			if key == 0xffe3 || key == 0xffe4 {
				body[4] = 1
			}
		}
		following = dcFrame(2, body)
	}
	// Ordered native DC preserves move-before-button/scroll without invoking
	// the native WS handlers that implicitly acquire/steal host ownership.
	if err := v.peer.SendData(dcMove(*message.X, *message.Y)); err != nil {
		clear(v.keys)
		clear(v.buttons)
		return ErrInputUnavailable
	}
	if following != nil {
		if err := v.peer.SendData(following); err != nil {
			clear(v.keys)
			clear(v.buttons)
			return ErrInputUnavailable
		}
	}
	if message.Action == "down" {
		v.buttons[*message.Button] = true
	}
	if message.Action == "up" {
		delete(v.buttons, *message.Button)
	}
	return nil
}
