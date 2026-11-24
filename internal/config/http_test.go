package config

import (
	"net/url"
	"testing"
)

func TestHTTPEnvironmentNeedsNoTURNAndUsesSameOriginRoomURLs(t *testing.T) {
	cfg, _ := newRoomConfig(t)
	t.Setenv("NEKO_ROOMS_NETWORK_MODE", "http")
	t.Setenv("NEKO_ROOMS_PATH_PREFIX", "/room")
	t.Setenv("NEKO_ROOMS_INSTANCE_URL", "http://192.0.2.1:28322/")
	t.Setenv("NEKO_ROOMS_RELAY_ICE_SERVERS_FRONTEND", "")
	cfg.Set()
	if cfg.EffectiveNetworkMode() != NetworkModeHTTP || cfg.GetRoomUrl("test") != "/room/test/" {
		t.Fatal("HTTP mode requires TURN or retained a permanent LAN URL")
	}
	cfg.RelayICEServersFrontend = "obsolete-invalid-relay-config"
	if err := cfg.ValidateNetworking(); err != nil {
		t.Fatalf("unused relay settings block HTTP aggregation: %v", err)
	}
	cfg.NetworkMode = NetworkModeDirect
	cfg.InstanceUrl, _ = url.Parse("http://192.0.2.1:28322/")
	if cfg.GetRoomUrl("test") != "http://192.0.2.1:28322/room/test/" {
		t.Fatal("direct URL compatibility changed")
	}
}
