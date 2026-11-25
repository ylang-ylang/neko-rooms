package types

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/m1k1o/neko-rooms/internal/config"
)

type RoomsConfig struct {
	NetworkMode    string   `json:"network_mode"`
	Connections    uint16   `json:"connections"`
	NekoImages     []string `json:"neko_images"`
	StorageEnabled bool     `json:"storage_enabled"`
	UsesMux        bool     `json:"uses_mux"`
}

type BrowserProfile struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	RoomID   string    `json:"room_id,omitempty"`
	RoomName string    `json:"room_name,omitempty"`
}

type ProfileCreateRequest struct {
	Name string `json:"name"`
}

var ErrProfileNotFound = fmt.Errorf("profile not found")
var ErrProfileInUse = fmt.Errorf("profile is bound to a deployment; remove that deployment before reusing or deleting the profile")

type RoomEntry struct {
	ProfileID      string            `json:"profile_id,omitempty"`
	ProfileName    string            `json:"profile_name,omitempty"`
	ID             string            `json:"id"`
	URL            string            `json:"url"`
	Name           string            `json:"name"`
	NekoImage      string            `json:"neko_image"`
	NetworkMode    string            `json:"network_mode"`
	IsOutdated     bool              `json:"is_outdated"`
	MaxConnections uint16            `json:"max_connections"` // 0 when using mux
	Running        bool              `json:"running"`
	Paused         bool              `json:"paused"`
	IsReady        bool              `json:"is_ready"`
	Status         string            `json:"status"`
	Created        time.Time         `json:"created"`
	Labels         map[string]string `json:"labels,omitempty"`

	ContainerLabels map[string]string `json:"-"` // for internal use
}

type MountType string

const (
	MountPrivate   MountType = "private"
	MountTemplate  MountType = "template"
	MountProtected MountType = "protected"
	MountPublic    MountType = "public"
)

type RoomMount struct {
	Type          MountType `json:"type"`
	HostPath      string    `json:"host_path"`
	ContainerPath string    `json:"container_path"`
}

type RoomResources struct {
	CPUShares int64    `json:"cpu_shares"` // relative weight vs. other containers
	NanoCPUs  int64    `json:"nano_cpus"`  // in units of 10^-9 CPUs
	ShmSize   int64    `json:"shm_size"`   // in bytes
	Memory    int64    `json:"memory"`     // in bytes
	Gpus      []string `json:"gpus"`       // gpu opts
	Devices   []string `json:"devices"`
}

type RoomSettings struct {
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"` // used only when creating a new profile

	ApiVersion int `json:"api_version"`

	Name           string `json:"name"`
	NekoImage      string `json:"neko_image"`
	NetworkMode    string `json:"network_mode"`    // read-only; creation uses the manager's policy
	MaxConnections uint16 `json:"max_connections"` // 0 when using mux

	ControlProtection bool `json:"control_protection"`
	ImplicitControl   bool `json:"implicit_control"`

	UserPass  string `json:"user_pass"`
	AdminPass string `json:"admin_pass"`

	Screen        string `json:"screen"`
	VideoCodec    string `json:"video_codec,omitempty"`
	VideoBitrate  int    `json:"video_bitrate,omitempty"`
	VideoPipeline string `json:"video_pipeline,omitempty"`
	VideoMaxFPS   int    `json:"video_max_fps"`

	AudioCodec    string `json:"audio_codec,omitempty"`
	AudioBitrate  int    `json:"audio_bitrate,omitempty"`
	AudioPipeline string `json:"audio_pipeline,omitempty"`

	BroadcastPipeline string `json:"broadcast_pipeline,omitempty"`

	Envs      map[string]string `json:"envs"`
	Labels    map[string]string `json:"labels"`
	Mounts    []RoomMount       `json:"mounts"`
	Resources RoomResources     `json:"resources"`

	Hostname string   `json:"hostname,omitempty"`
	DNS      []string `json:"dns,omitempty"`

	BrowserPolicy *BrowserPolicy `json:"browser_policy,omitempty"`
}

func (settings *RoomSettings) ValidateNetworkPolicy(cfg *config.Room) error {
	if err := cfg.ValidateNetworking(); err != nil {
		return err
	}
	mode := cfg.EffectiveNetworkMode()
	if mode == config.NetworkModeRelay || mode == config.NetworkModeHTTP {
		if settings.ApiVersion != 3 {
			return fmt.Errorf("%s networking requires Neko API version 3", mode)
		}
		for key := range settings.Envs {
			if name, _, _ := strings.Cut(key, "="); name == "NEKO_CONFIG" {
				return fmt.Errorf("%s networking manages NEKO_CONFIG; custom native Neko config files are unsupported (use direct mode to retain one)", mode)
			}
		}
	}
	if mode == config.NetworkModeHTTP {
		if settings.VideoCodec != "" && !strings.EqualFold(settings.VideoCodec, "VP8") {
			return fmt.Errorf("http aggregation requires VP8 video")
		}
		if settings.AudioCodec != "" && !strings.EqualFold(settings.AudioCodec, "OPUS") {
			return fmt.Errorf("http aggregation requires Opus audio")
		}
		for key, value := range settings.Envs {
			name, _, _ := strings.Cut(key, "=")
			switch name {
			case "NEKO_SESSION_COOKIE_ENABLED":
				if value != "" && value != "false" {
					return fmt.Errorf("http aggregation requires native token sessions; cookie mode is unsupported")
				}
			case "NEKO_CAPTURE_VIDEO_CODEC":
				if value != "" && !strings.EqualFold(value, "vp8") {
					return fmt.Errorf("http aggregation requires VP8 video")
				}
			case "NEKO_CAPTURE_AUDIO_CODEC":
				if value != "" && !strings.EqualFold(value, "opus") {
					return fmt.Errorf("http aggregation requires Opus audio")
				}
			}
		}
	}
	return nil
}

func (settings *RoomSettings) ToEnv(config *config.Room, ports PortSettings) ([]string, error) {
	if err := settings.ValidateNetworkPolicy(config); err != nil {
		return nil, err
	}
	switch settings.ApiVersion {
	case 2:
		return settings.toEnvV2(config, ports), nil
	case 3:
		return settings.toEnvV3(config, ports)
	default:
		return nil, fmt.Errorf("unsupported API version: %d", settings.ApiVersion)
	}
}

func (settings *RoomSettings) FromEnv(apiVersion int, envs []string) error {
	switch apiVersion {
	case 2:
		return settings.fromEnvV2(envs)
	case 3:
		return settings.fromEnvV3(envs)
	default:
		return fmt.Errorf("unsupported API version: %d", apiVersion)
	}
}

type PortSettings struct {
	FrontendPort   uint16
	EprMin, EprMax uint16
}

type RoomStats struct {
	Connections uint32        `json:"connections"`
	Host        string        `json:"host"`
	Members     []*RoomMember `json:"members"`

	Banned map[string]string `json:"banned"` // IP -> session ID (that banned it)
	Locked map[string]string `json:"locked"` // resource name -> session ID (that locked it)

	ServerStartedAt time.Time  `json:"server_started_at"`
	LastAdminLeftAt *time.Time `json:"last_admin_left_at"`
	LastUserLeftAt  *time.Time `json:"last_user_left_at"`

	ControlProtection bool `json:"control_protection"`
	ImplicitControl   bool `json:"implicit_control"`
}

type RoomMember struct {
	ID    string `json:"id"`
	Name  string `json:"displayname"`
	Admin bool   `json:"admin"`
	Muted bool   `json:"muted"`
}

type RoomEventAction string

const (
	RoomEventCreated   RoomEventAction = "created"
	RoomEventStarted   RoomEventAction = "started"
	RoomEventReady     RoomEventAction = "ready"
	RoomEventStopped   RoomEventAction = "stopped"
	RoomEventDestroyed RoomEventAction = "destroyed"
	RoomEventPaused    RoomEventAction = "paused"
)

type RoomEvent struct {
	ID     string          `json:"id"`
	Action RoomEventAction `json:"action"`

	ContainerLabels map[string]string `json:"-"` // for internal use
}

var ErrRoomNotFound = fmt.Errorf("room not found")
var ErrRoomNameConflict = fmt.Errorf("room name or storage already exists")

// RoomCloneRequest deliberately excludes settings and credentials: the manager
// reads the authoritative source configuration, never a frontend round trip.
type RoomCloneRequest struct {
	Name string `json:"name"`
}

// ErrInvalidRoomSettings identifies rejected client settings, not Docker or
// operator-configuration failures. HTTP handlers may map only this error to 400.
var ErrInvalidRoomSettings = fmt.Errorf("invalid room settings")

type RoomManager interface {
	ListProfiles(ctx context.Context) ([]BrowserProfile, error)
	CreateProfile(ctx context.Context, name string) (*BrowserProfile, error)
	DeleteProfile(ctx context.Context, id string) error
	Config() RoomsConfig
	List(ctx context.Context, labels map[string]string) ([]RoomEntry, error)
	ExportAsDockerCompose(ctx context.Context) ([]byte, error)

	ValidateSettings(ctx context.Context, settings RoomSettings) error
	Create(ctx context.Context, settings RoomSettings) (string, error)
	Clone(ctx context.Context, id string, name string) (*RoomEntry, error)
	Recreate(ctx context.Context, id string, settings RoomSettings, start bool) (*RoomEntry, error)
	GetEntry(ctx context.Context, id string) (*RoomEntry, error)
	GetEntryByName(ctx context.Context, name string) (*RoomEntry, error)
	GetSettings(ctx context.Context, id string) (*RoomSettings, error)
	GetStats(ctx context.Context, id string) (*RoomStats, error)
	Remove(ctx context.Context, id string) error

	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error

	EventsLoopStart()
	EventsLoopStop() error
	Events(ctx context.Context) (<-chan RoomEvent, <-chan error)
}
