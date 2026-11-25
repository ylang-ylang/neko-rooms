package room

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestLocalStorageCaptureScopesOutProtectedExtensionTargets(t *testing.T) {
	for _, failWebAttach := range []bool{false, true} {
		t.Run(map[bool]string{false: "native multi-page mixture", true: "web attach failure remains fatal"}[failWebAttach], func(t *testing.T) {
			attached := []string{}
			streamClosed := make(chan struct{})
			manager := &RoomManagerCtx{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/json/version" {
					json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://localhost/devtools/browser/default"})
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				defer close(streamClosed)
				for {
					var request struct {
						ID      int             `json:"id"`
						Method  string          `json:"method"`
						Session string          `json:"sessionId"`
						Params  json.RawMessage `json:"params"`
					}
					if conn.ReadJSON(&request) != nil {
						return
					}
					result := any(map[string]any{})
					failure := false
					switch request.Method {
					case "Target.getBrowserContexts":
						result = map[string]any{"browserContextIds": []string{}}
					case "Target.getTargets":
						result = map[string]any{"targetInfos": []any{
							map[string]any{"targetId": "extension", "type": "page", "url": "chrome-extension://protected-extension/options.html", "attached": false},
							map[string]any{"targetId": "blank", "type": "page", "url": "about:blank", "attached": false},
							map[string]any{"targetId": "chrome", "type": "page", "url": "chrome://newtab/", "attached": false},
							map[string]any{"targetId": "web", "type": "page", "url": "https://private.example/account?private-query", "attached": true},
						}}
					case "Target.attachToTarget":
						var params struct {
							ID string `json:"targetId"`
						}
						json.Unmarshal(request.Params, &params)
						attached = append(attached, params.ID)
						failure = params.ID == "extension" || (params.ID == "web" && failWebAttach)
						result = map[string]string{"sessionId": params.ID}
					case "Page.getFrameTree":
						origin := "https://private.example"
						if request.Session == "blank" {
							origin = "https://inherited-popup.example"
						}
						if request.Session == "chrome" {
							origin = "://"
						}
						result = map[string]any{"frameTree": map[string]any{"frame": map[string]string{"id": request.Session, "securityOrigin": origin}}}
					case "Storage.getStorageKeyForFrame":
						result = map[string]string{"storageKey": request.Session + "-storage-key"}
					case "DOMStorage.getDOMStorageItems":
						result = map[string]any{"entries": [][]string{{"identity", "buffered-marker"}}}
					}
					response := map[string]any{"id": request.ID, "result": result}
					if failure {
						response = map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": "Not allowed: private-query must not appear in API errors"}}
					}
					if conn.WriteJSON(response) != nil {
						return
					}
				}
			}))
			defer server.Close()
			manager.cdpDial = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			pages, err := manager.snapshotLocalStorage(context.Background(), "owned-room:9223")
			<-streamClosed // Synchronize observations with the websocket handler.
			if failWebAttach {
				if err == nil || !strings.Contains(err.Error(), "Target.attachToTarget") || strings.Contains(err.Error(), "private-query") {
					t.Fatal("a required web attach failure was hidden or exposed private protocol details")
				}
			} else {
				if err != nil || len(pages) != 3 {
					t.Fatalf("protected extension page prevented web storage capture: %v", err)
				}
				if len(pages[0].Entries) == 0 || len(pages[2].Entries) == 0 || len(pages[1].Entries) != 0 {
					t.Fatal("inherited about:blank or existing-attached web page storage was lost")
				}
			}
			for _, id := range attached {
				if id == "extension" {
					t.Fatal("capture attempted debugger attachment outside its web-storage scope")
				}
			}
		})
	}
}
