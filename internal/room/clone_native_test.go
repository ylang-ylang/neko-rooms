package room

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"
)

// Explicitly opt in against an OWNED diagnostic room's private CDP address.
// No cookies or values are logged, no source pages/contexts are created,
// navigated or closed, and no source lifecycle/storage writes are performed.
func TestCloneNativeStorageCaptureWithExistingAttachment(t *testing.T) {
	address := os.Getenv("NEKO_CLONE_TEST_NATIVE_CDP")
	if address == "" {
		t.Skip("set NEKO_CLONE_TEST_NATIVE_CDP to an owned diagnostic room's private CDP address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager := &RoomManagerCtx{}
	browser, err := manager.connectBrowser(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.close() // Disconnect our diagnostic attachment only.
	var targets struct {
		Infos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := browser.call(ctx, "Target.getTargets", nil, &targets); err != nil {
		t.Fatal(err)
	}
	var webTarget string
	for _, target := range targets.Infos {
		parsed, _ := url.Parse(target.URL)
		if target.Type == "page" && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			webTarget = target.ID
			break
		}
	}
	if webTarget == "" {
		t.Fatal("owned diagnostic room has no existing HTTP(S) page")
	}
	var attached struct {
		ID string `json:"sessionId"`
	}
	if err := browser.call(ctx, "Target.attachToTarget", map[string]any{"targetId": webTarget, "flatten": true}, &attached); err != nil || attached.ID == "" {
		t.Fatal("cannot establish read-only diagnostic attachment to owned web page")
	}
	// Keep the first attachment alive while production capture attaches using
	// a second browser connection, as with a concurrently attached CDP client.
	pages, err := manager.snapshotLocalStorage(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	webPages := 0
	for _, page := range pages {
		if validStorageOrigin(page.Frame.Origin) {
			webPages++
		}
	}
	if webPages == 0 {
		t.Fatal("native capture omitted existing web-page storage")
	}
	t.Logf("native capture passed with %d web pages while an existing CDP attachment remained open; no origins or values logged", webPages)
}
