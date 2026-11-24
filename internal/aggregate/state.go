package aggregate

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image/png"
	"sort"
	"strings"

	"github.com/gorilla/websocket"
)

type nativeMember struct {
	ID      string        `json:"id"`
	Profile nativeProfile `json:"profile"`
	State   struct {
		IsConnected bool `json:"is_connected"`
		IsWatching  bool `json:"is_watching"`
	} `json:"state"`
}

type viewerState struct {
	ID            string
	Profile       nativeProfile
	Width, Height int
	Host          string
	PrivateMode   bool
	Members       map[string]nativeMember
}

func (s viewerState) allowed() bool {
	return s.ID != "" && s.Profile.CanLogin && s.Profile.CanConnect && s.Profile.CanWatch && (!s.PrivateMode || s.Profile.IsAdmin)
}

func (v *viewer) initialize(identity nativeSession, payload json.RawMessage) error {
	var init struct {
		ID     string `json:"session_id"`
		Screen struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"screen_size"`
		Host struct {
			HasHost bool   `json:"has_host"`
			ID      string `json:"host_id"`
		} `json:"control_host"`
		Sessions map[string]nativeMember `json:"sessions"`
		Settings struct {
			PrivateMode bool `json:"private_mode"`
		} `json:"settings"`
	}
	if decodeJSON(payload, &init) != nil || init.ID != identity.ID || init.ID == "" {
		return errors.New("native system/init identity mismatch")
	}
	self, ok := init.Sessions[init.ID]
	if !ok || self.ID != init.ID {
		return errors.New("native system/init lacks viewer identity")
	}
	if init.Screen.Width <= 0 || init.Screen.Width > 65535 || init.Screen.Height <= 0 || init.Screen.Height > 65535 {
		return errors.New("invalid native desktop dimensions")
	}
	s := viewerState{ID: init.ID, Profile: self.Profile, Width: init.Screen.Width, Height: init.Screen.Height, PrivateMode: init.Settings.PrivateMode, Members: init.Sessions}
	if init.Host.HasHost {
		s.Host = init.Host.ID
	}
	if !s.allowed() {
		return errors.New("native viewing permission or private mode denied")
	}
	v.stateMu.Lock()
	v.state = s
	v.stateMu.Unlock()
	return v.emitState()
}

func (v *viewer) emitState() error {
	v.stateMu.Lock()
	s := v.state
	members := make([]map[string]any, 0, len(s.Members))
	ids := make([]string, 0, len(s.Members))
	for id := range s.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		member := s.Members[id]
		members = append(members, map[string]any{"id": id, "username": member.Profile.Name, "connected": member.State.IsConnected, "is_admin": member.Profile.IsAdmin})
	}
	state := map[string]any{"type": "state", "controller_id": s.Host, "member_id": s.ID, "width": s.Width, "height": s.Height, "members": members, "can_host": s.Profile.CanHost, "can_watch": s.allowed(), "can_access_clipboard": s.Profile.CanAccessClipboard}
	v.stateMu.Unlock()
	return v.emit(state)
}

func (v *viewer) handleNative(message nativeMessage) error {
	if message.Event == "system/heartbeat" {
		return v.sendNative("client/heartbeat", nil)
	}
	if message.Event == "system/disconnect" {
		v.stop(websocket.ClosePolicyViolation, "native room disconnected viewer")
		return ErrNotReady
	}
	if strings.HasPrefix(message.Event, "signal/") {
		if message.Event == "signal/close" {
			v.stop(websocket.CloseTryAgainLater, "native media peer closed; reconnect required")
			return ErrNotReady
		}
		if err := v.peer.HandleSignal(message.Event, message.Payload); err != nil {
			v.stop(websocket.CloseTryAgainLater, "private media signaling failed")
			return err
		}
		return nil
	}
	if message.Event == "clipboard/updated" {
		var clipboard struct {
			Text string `json:"text"`
		}
		if decodeJSON(message.Payload, &clipboard) != nil || len(clipboard.Text) > 64<<10 {
			return nil
		}
		v.stateMu.Lock()
		allowed := v.state.allowed() && v.state.Profile.CanAccessClipboard
		v.stateMu.Unlock()
		if allowed {
			return v.emit(map[string]string{"type": "clipboard", "text": clipboard.Text})
		}
		return nil
	}
	v.stateMu.Lock()
	oldHost := v.state.Host
	resized := false
	var err error
	changed := true
	switch message.Event {
	case "control/host":
		var host struct {
			HasHost bool   `json:"has_host"`
			ID      string `json:"host_id"`
		}
		err = decodeJSON(message.Payload, &host)
		if err == nil {
			v.state.Host = ""
			if host.HasHost {
				v.state.Host = host.ID
			}
		}
	case "screen/updated":
		var screen struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		err = decodeJSON(message.Payload, &screen)
		if err == nil && (screen.Width <= 0 || screen.Width > 65535 || screen.Height <= 0 || screen.Height > 65535) {
			err = errors.New("invalid desktop dimensions")
		}
		if err == nil {
			resized = v.state.Width != screen.Width || v.state.Height != screen.Height
			v.state.Width = screen.Width
			v.state.Height = screen.Height
		}
	case "system/settings":
		var settings struct {
			PrivateMode bool `json:"private_mode"`
		}
		err = decodeJSON(message.Payload, &settings)
		if err == nil {
			v.state.PrivateMode = settings.PrivateMode
		}
	case "session/created":
		var member nativeMember
		err = decodeJSON(message.Payload, &member)
		if err == nil {
			v.state.Members[member.ID] = member
		}
	case "session/profile":
		var profile struct {
			ID string `json:"id"`
			nativeProfile
		}
		err = decodeJSON(message.Payload, &profile)
		if err == nil {
			member := v.state.Members[profile.ID]
			member.ID = profile.ID
			member.Profile = profile.nativeProfile
			v.state.Members[profile.ID] = member
			if profile.ID == v.state.ID {
				v.state.Profile = profile.nativeProfile
			}
		}
	case "session/deleted":
		var member struct {
			ID string `json:"id"`
		}
		err = decodeJSON(message.Payload, &member)
		if err == nil {
			delete(v.state.Members, member.ID)
			if member.ID == v.state.ID {
				v.state.Profile.CanLogin = false
			}
		}
	case "session/state":
		var state struct {
			ID          string `json:"id"`
			IsConnected bool   `json:"is_connected"`
			IsWatching  bool   `json:"is_watching"`
		}
		err = decodeJSON(message.Payload, &state)
		if err == nil {
			member := v.state.Members[state.ID]
			member.State.IsConnected = state.IsConnected
			member.State.IsWatching = state.IsWatching
			v.state.Members[state.ID] = member
			if state.ID == v.state.ID && !state.IsConnected {
				v.state.Profile.CanConnect = false
			}
		}
	default:
		changed = false
	}
	allowed := v.state.allowed()
	lostHost := oldHost != v.state.Host
	v.stateMu.Unlock()
	if lostHost {
		v.clearInput()
	}
	if err != nil {
		v.stop(websocket.CloseGoingAway, "invalid native room state")
		return err
	}
	if !allowed {
		// Closing discards Manager's queued bytes, stops private RTP/PC, and
		// causes the owned frontend to destroy its MediaSource/append queue.
		v.stop(websocket.ClosePolicyViolation, "native viewing permission revoked or private mode enabled")
		return ErrNotReady
	}
	if resized {
		// A fresh connection supplies a new hello/init/keyframe and replays
		// cursor state. Never append a second init or silently change desktop
		// coordinates within the old decoder epoch. 1013 retains room token.
		v.stop(websocket.CloseTryAgainLater, "desktop resized; reconnect required")
		return ErrNotReady
	}
	if changed {
		return v.emitState()
	}
	return nil
}

func (v *viewer) rememberCursor(cursor map[string]any) {
	v.cursorMu.Lock()
	defer v.cursorMu.Unlock()
	if cursor["action"] == "image" {
		v.cursorImage = cursor
	} else {
		v.cursorPosition = cursor
	}
	v.stateMu.Lock()
	ready := v.ready
	v.stateMu.Unlock()
	if ready {
		_ = v.emit(cursor)
	}
}

func (v *viewer) cursor(data []byte) {
	if v.ctx.Err() != nil || len(data) < 3 || len(data) > maxNativeJSON {
		return
	}
	// Native outgoing length includes its header, unlike input's body length.
	// Decode by opcode/actual frame boundary; never slice PNG using that u16.
	switch data[0] {
	case 1:
		if len(data) != 7 {
			return
		}
		v.rememberCursor(map[string]any{"type": "cursor", "action": "position", "x": binary.BigEndian.Uint16(data[3:5]), "y": binary.BigEndian.Uint16(data[5:7])})
	case 2:
		if len(data) < 11 {
			return
		}
		width, height := binary.BigEndian.Uint16(data[3:5]), binary.BigEndian.Uint16(data[5:7])
		x, y := binary.BigEndian.Uint16(data[7:9]), binary.BigEndian.Uint16(data[9:11])
		if len(data) == 11 && width == 0 && height == 0 {
			v.rememberCursor(map[string]any{"type": "cursor", "action": "image", "width": 0, "height": 0, "hotspotX": 0, "hotspotY": 0, "png": ""})
			return
		}
		config, err := png.DecodeConfig(bytes.NewReader(data[11:]))
		if err != nil || config.Width != int(width) || config.Height != int(height) || width > 512 || height > 512 || x >= width || y >= height || len(data)-11 > 288<<10 {
			return
		}
		v.rememberCursor(map[string]any{"type": "cursor", "action": "image", "width": width, "height": height, "hotspotX": x, "hotspotY": y, "png": base64.StdEncoding.EncodeToString(data[11:])})
	}
}
