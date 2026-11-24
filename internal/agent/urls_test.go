package agent

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicRoomURLAndDiscoveryUseTheSamePrefix(t *testing.T) {
	for _, test := range []struct {
		name, configured, forwarded, proto, host, want string
		tls                                            bool
	}{
		{name: "request", want: "http://internal.example/rooms/stable/"},
		{name: "TLS", tls: true, want: "https://internal.example/rooms/stable/"},
		{name: "x-forwarded", proto: "https, http", host: "cdp.example.test:9443, internal", want: "https://cdp.example.test:9443/rooms/stable/"},
		{name: "Forwarded", forwarded: `for=192.0.2.1;proto=https;host="cdp.example.test:9443"`, want: "https://cdp.example.test:9443/rooms/stable/"},
		{name: "configured", configured: "http://cdp.example.test/rooms/", proto: "https", host: "ignored.example", want: "http://cdp.example.test/rooms/stable/"},
		{name: "configured proxy prefix", configured: "https://cdp.example.test/cdp/rooms", want: "https://cdp.example.test/cdp/rooms/stable/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := New(nil, test.configured)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Shutdown()
			r := httptest.NewRequest("GET", "http://internal.example/rooms/stable/json/version", nil)
			if test.tls {
				r.TLS = &tls.ConnectionState{}
			}
			r.Header.Set("Forwarded", test.forwarded)
			r.Header.Set("X-Forwarded-Proto", test.proto)
			r.Header.Set("X-Forwarded-Host", test.host)
			base, err := manager.roomURL(r, "stable")
			if err != nil || base.String() != test.want {
				t.Fatalf("room URL %v: %v, want %s", base, err, test.want)
			}
			version := map[string]any{"webSocketDebuggerUrl": "ws://internal-container:9222/devtools/browser/existing"}
			if err := rewriteDebuggerURLs(version, base); err != nil {
				t.Fatal(err)
			}
			wantWS := "ws" + strings.TrimPrefix(test.want, "http") + "devtools/browser/existing"
			if version["webSocketDebuggerUrl"] != wantWS {
				t.Fatalf("HTTP/WS public routes differ: %v, want %s", version, wantWS)
			}
		})
	}
}

func TestInvalidPublicURLFailsExplicitly(t *testing.T) {
	for _, value := range []string{"cdp.example/rooms/", "ftp://cdp.example/rooms/", "http://", "http://user:password@cdp.example/rooms/", "http://cdp.example/rooms/?key=secret", "http://cdp.example/rooms/#secret", "://bad"} {
		if manager, err := New(nil, value); err == nil {
			manager.Shutdown()
			t.Errorf("accepted invalid public prefix %q", value)
		}
	}
}
