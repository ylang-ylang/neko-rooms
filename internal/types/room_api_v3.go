package types

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/m1k1o/neko-rooms/internal/config"
)

//
// demodesk/neko v3 envs API
//

var blacklistedEnvsV3 = []string{
	// ignore bunch of default envs
	"DEBIAN_FRONTEND",
	"PULSE_SERVER",
	"XDG_RUNTIME_DIR",
	"DISPLAY",
	"USER",
	"PATH",

	// ignore bunch of envs managed by neko
	"NEKO_PLUGINS_ENABLED",
	"NEKO_PLUGINS_DIR",

	// ignore bunch of envs managed by neko-rooms
	"NEKO_SERVER_BIND",
	"NEKO_SERVER_PROXY",
	"NEKO_SESSION_API_TOKEN",
	"NEKO_MEMBER_PROVIDER",
	"NEKO_WEBRTC_EPR",
	"NEKO_WEBRTC_UDPMUX",
	"NEKO_WEBRTC_TCPMUX",
	"NEKO_WEBRTC_NAT1TO1",
	"NEKO_WEBRTC_ICELITE",
}

// Neko v3 still recognizes these v2 network aliases. In relay mode they must
// not override native v3 settings after Neko has loaded the authoritative policy.
var relayNetworkEnvsV3 = []string{
	"NEKO_WEBRTC_ICESERVERS",
	"NEKO_WEBRTC_ICESERVERS_FRONTEND",
	"NEKO_WEBRTC_ICESERVERS_BACKEND",
	"NEKO_WEBRTC_IP_RETRIEVAL_URL",
	"NEKO_EPR",
	"NEKO_UDPMUX",
	"NEKO_TCPMUX",
	"NEKO_NAT1TO1",
	"NEKO_ICELITE",
	"NEKO_ICESERVER",
	"NEKO_ICESERVERS",
	"NEKO_IPFETCH",
	"NEKO_CONFIG",
	"NEKO_LEGACY",
}

// IsManagedEnvV3 identifies configuration owned by the manager rather than a
// custom room override. The same policy guards native image CLI arguments.
func IsManagedEnvV3(key string, relay bool) bool {
	return slices.Contains(blacklistedEnvsV3, key) || (relay && slices.Contains(relayNetworkEnvsV3, key))
}

func (settings *RoomSettings) toEnvV3(cfg *config.Room, ports PortSettings) ([]string, error) {
	env := []string{
		fmt.Sprintf("NEKO_SERVER_BIND=:%d", ports.FrontendPort),
		"NEKO_SERVER_PROXY=true",

		// from settings
		"NEKO_MEMBER_PROVIDER=multiuser",
		fmt.Sprintf("NEKO_MEMBER_MULTIUSER_USER_PASSWORD=%s", settings.UserPass),
		fmt.Sprintf("NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD=%s", settings.AdminPass),
		fmt.Sprintf("NEKO_SESSION_API_TOKEN=%s", settings.AdminPass), // TODO: should be random and saved somewhere
		fmt.Sprintf("NEKO_DESKTOP_SCREEN=%s", settings.Screen),
		//fmt.Sprintf("NEKO_MAX_FPS=%d", settings.VideoMaxFPS), // TODO: not supported yet
	}

	relay := cfg.EffectiveNetworkMode() == config.NetworkModeRelay
	if relay {
		frontendICE, err := cfg.RelayFrontendICEJSON()
		if err != nil {
			return nil, err
		}
		env = append(env,
			"NEKO_WEBRTC_ICELITE=false",
			"NEKO_WEBRTC_NAT1TO1=",
			"NEKO_WEBRTC_IP_RETRIEVAL_URL=",
			"NEKO_WEBRTC_ICESERVERS_FRONTEND="+frontendICE,
			"NEKO_WEBRTC_ICESERVERS_BACKEND=[]",
			"NEKO_LEGACY=false",
			// Pinned Neko logs TURN credentials at info/debug. An explicit
			// per-room log level may opt in using normal custom precedence.
			"NEKO_LOG_LEVEL=warn",
		)
		// Empty envs also clear image defaults. The startup adapter's native
		// config supplies the empty values that pinned Neko's Viper ignores.
		if cfg.Mux {
			env = append(env, "NEKO_WEBRTC_EPR=")
		} else {
			env = append(env, "NEKO_WEBRTC_UDPMUX=0", "NEKO_WEBRTC_TCPMUX=0")
		}
	} else {
		env = append(env, "NEKO_WEBRTC_ICELITE=true")
	}

	if cfg.Mux {
		env = append(env,
			fmt.Sprintf("NEKO_WEBRTC_UDPMUX=%d", ports.EprMin),
			fmt.Sprintf("NEKO_WEBRTC_TCPMUX=%d", ports.EprMin),
		)
	} else {
		env = append(env,
			fmt.Sprintf("NEKO_WEBRTC_EPR=%d-%d", ports.EprMin, ports.EprMax),
		)
	}

	// Direct NAT mapping is deliberately not inherited by relay rooms.
	if !relay && len(cfg.NAT1To1IPs) > 0 {
		env = append(env, fmt.Sprintf("NEKO_WEBRTC_NAT1TO1=%s", strings.Join(cfg.NAT1To1IPs, ",")))
	}

	if settings.ControlProtection {
		env = append(env, "NEKO_SESSION_CONTROL_PROTECTION=true")
	}

	// implicit control - enabled by default but in legacy mode disabled by default
	// so we need to set it explicitly until legacy mode is removed
	if !settings.ImplicitControl {
		env = append(env, "NEKO_SESSION_IMPLICIT_HOSTING=false")
	} else {
		env = append(env, "NEKO_SESSION_IMPLICIT_HOSTING=true")
	}

	if settings.VideoCodec != "VP8" { // VP8 is default
		env = append(env, fmt.Sprintf("NEKO_CAPTURE_VIDEO_CODEC=%s", strings.ToLower(settings.VideoCodec)))
	}

	//if settings.VideoBitrate != 0 {
	//	env = append(env, fmt.Sprintf("NEKO_VIDEO_BITRATE=%d", settings.VideoBitrate)) // TODO: not supported yet
	//}

	if settings.VideoPipeline != "" {
		env = append(env, fmt.Sprintf("NEKO_CAPTURE_VIDEO_PIPELINE=%s", settings.VideoPipeline)) // TOOD: allow simulcast pipelines
	}

	if settings.AudioCodec != "OPUS" { // OPUS is default
		env = append(env, fmt.Sprintf("NEKO_CAPTURE_AUDIO_CODEC=%s", strings.ToLower(settings.AudioCodec)))
	}

	//if settings.AudioBitrate != 0 {
	//	env = append(env, fmt.Sprintf("NEKO_AUDIO_BITRATE=%d", settings.AudioBitrate)) // TODO: not supported yet
	//}

	if settings.AudioPipeline != "" {
		env = append(env, fmt.Sprintf("NEKO_CAPTURE_AUDIO_PIPELINE=%s", settings.AudioPipeline))
	}

	if settings.BroadcastPipeline != "" {
		env = append(env, fmt.Sprintf("NEKO_CAPTURE_BROADCAST_PIPELINE=%s", settings.BroadcastPipeline))
	}

	// Preserve existing custom-setting precedence without sending duplicate names
	// to Docker. Network policy keys are always controlled by the manager.
	indices := make(map[string]int, len(env))
	for i, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		indices[key] = i
	}
	for key, val := range settings.Envs {
		// Docker interprets everything before the first '=' as the name, even
		// when a caller incorrectly includes '=' in a custom map key.
		envKey, _, _ := strings.Cut(key, "=")
		if IsManagedEnvV3(envKey, relay) {
			continue
		}
		entry := fmt.Sprintf("%s=%s", key, val)
		if i, exists := indices[envKey]; exists {
			env[i] = entry
		} else {
			indices[envKey] = len(env)
			env = append(env, entry)
		}
	}

	return env, nil
}

func (settings *RoomSettings) fromEnvV3(envs []string) error {
	settings.Envs = map[string]string{}
	// enabled implicit control by default
	settings.ImplicitControl = true
	settings.VideoCodec = "VP8"  // default
	settings.AudioCodec = "OPUS" // default

	var err error
	for _, env := range envs {
		r := strings.SplitN(env, "=", 2)
		key, val := r[0], r[1]

		switch key {
		case "NEKO_MEMBER_MULTIUSER_USER_PASSWORD":
			settings.UserPass = val
		case "NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD":
			settings.AdminPass = val
		case "NEKO_SESSION_CONTROL_PROTECTION":
			settings.ControlProtection, err = strconv.ParseBool(val)
		case "NEKO_SESSION_IMPLICIT_HOSTING":
			settings.ImplicitControl, err = strconv.ParseBool(val)
		case "NEKO_DESKTOP_SCREEN":
			settings.Screen = val
		case "NEKO_LOG_LEVEL":
			// The relay default is policy, not a custom room setting. Keep
			// explicit nondefault levels so recreation preserves the opt-in.
			if settings.NetworkMode != config.NetworkModeRelay || val != "warn" {
				settings.Envs[key] = val
			}
		//case "NEKO_MAX_FPS": // TODO: not supported yet
		//	settings.VideoMaxFPS, err = strconv.Atoi(val)
		case "NEKO_CAPTURE_BROADCAST_PIPELINE":
			settings.BroadcastPipeline = val
		case "NEKO_CAPTURE_VIDEO_CODEC":
			settings.VideoCodec = strings.ToUpper(val)
		//case "NEKO_VIDEO_BITRATE": // TODO: not supported yet
		//	settings.VideoBitrate, err = strconv.Atoi(val)
		case "NEKO_CAPTURE_VIDEO_PIPELINE": // TOOD: allow simulcast pipelines
			settings.VideoPipeline = val
		case "NEKO_CAPTURE_AUDIO_CODEC":
			settings.AudioCodec = strings.ToUpper(val)
		//case "NEKO_AUDIO_BITRATE": // TODO: not supported yet
		//	settings.AudioBitrate, err = strconv.Atoi(val)
		case "NEKO_CAPTURE_AUDIO_PIPELINE":
			settings.AudioPipeline = val
		default:
			if !IsManagedEnvV3(key, settings.NetworkMode == config.NetworkModeRelay) {
				settings.Envs[key] = val
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}
