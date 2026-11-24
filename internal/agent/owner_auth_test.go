package agent

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestOwnerSessionNeverAuthenticatesOrReachesChromium(t *testing.T) {
	chromium := newFakeChromium(t, "original")
	h := newHarness(t, map[string]*fakeChromium{"aaaaaaaaaaaa:9223": chromium})
	key := h.issue(t, "stable", `{}`)
	ownerCookie := "neko_rooms_admin_session=opaque-owner-session; neko_viewer=viewer-cookie"

	for _, authorization := range []string{"", "Basic b3duZXI6cGFzc3dvcmQ="} {
		req, err := http.NewRequest("GET", h.server.URL+"/rooms/stable/json/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", ownerCookie)
		req.Header.Set("Authorization", authorization)
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("owner cookie/Basic authenticated Agent CDP")
		}
	}

	req, err := http.NewRequest("GET", h.server.URL+"/rooms/stable/json/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Cookie", ownerCookie)
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Bearer discovery failed: %d", res.StatusCode)
	}

	conn, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/rooms/stable/devtools/browser/original", http.Header{
		"Authorization": {"Bearer " + key.Key}, "Cookie": {ownerCookie},
	})
	if err != nil {
		t.Fatalf("Bearer WebSocket failed: %v, %+v", err, res)
	}
	defer conn.Close()
	command(t, conn, "original")

	for i := 0; i < 2; i++ {
		seen := <-chromium.seen
		if seen.Authorization != "" || seen.Cookie != "" || seen.Host != "127.0.0.1:9222" {
			t.Fatalf("owner/Agent/viewer credentials reached Chromium: %+v", seen)
		}
	}
}
