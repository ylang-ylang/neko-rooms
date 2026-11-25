package room

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/m1k1o/neko-rooms/internal/utils"
)

type cloneFrame struct {
	Frame struct {
		ID     string `json:"id"`
		Origin string `json:"securityOrigin"`
	} `json:"frame"`
	Children []*cloneFrame `json:"childFrames"`
	Entries  json.RawMessage
}

// LocalStorage is persistent by API semantics but Chrome batches its disk
// writes. Capture only active existing frames' localStorage via CDP; opaque
// profile data and all IndexedDB databases still come from the full snapshot.
func (manager *RoomManagerCtx) snapshotLocalStorage(ctx context.Context, address string) ([]*cloneFrame, error) {
	browser, err := manager.connectBrowser(ctx, address)
	if err != nil {
		return nil, err
	}
	defer browser.close()
	var contexts struct {
		IDs []string `json:"browserContextIds"`
	}
	if err := browser.call(ctx, "Target.getBrowserContexts", nil, &contexts); err != nil {
		return nil, err
	}
	var targets struct {
		Infos []struct {
			ID      string `json:"targetId"`
			Type    string `json:"type"`
			Context string `json:"browserContextId"`
		} `json:"targetInfos"`
	}
	if err := browser.call(ctx, "Target.getTargets", nil, &targets); err != nil {
		return nil, err
	}
	sessions := map[string]string{}
	pages := []*cloneFrame{}
	for _, target := range targets.Infos {
		if target.Type != "page" && target.Type != "iframe" {
			continue
		}
		incognito := false
		for _, id := range contexts.IDs {
			incognito = incognito || id == target.Context
		}
		if incognito {
			continue
		}
		var attached struct {
			ID string `json:"sessionId"`
		}
		if err := browser.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached); err != nil {
			return nil, err
		}
		var result struct {
			Tree *cloneFrame `json:"frameTree"`
		}
		if err := browser.callSession(ctx, attached.ID, "Page.getFrameTree", nil, &result); err != nil || result.Tree == nil {
			return nil, errors.New("cannot inspect active browser storage frames")
		}
		sessions[result.Tree.Frame.ID] = attached.ID
		if target.Type == "page" {
			pages = append(pages, result.Tree)
		}
	}
	var capture func(*cloneFrame, string) error
	capture = func(frame *cloneFrame, session string) error {
		if own, exists := sessions[frame.Frame.ID]; exists {
			session = own // Out-of-process iframes need their own attached session.
		}
		if validStorageOrigin(frame.Frame.Origin) {
			var key struct {
				Key string `json:"storageKey"`
			}
			if err := browser.callSession(ctx, session, "Storage.getStorageKeyForFrame", map[string]string{"frameId": frame.Frame.ID}, &key); err != nil {
				return err
			}
			var items struct {
				Entries json.RawMessage `json:"entries"`
			}
			if err := browser.callSession(ctx, session, "DOMStorage.getDOMStorageItems", map[string]any{"storageId": map[string]any{"storageKey": key.Key, "isLocalStorage": true}}, &items); err != nil {
				return err
			}
			frame.Entries = items.Entries
		}
		for _, child := range frame.Children {
			if err := capture(child, session); err != nil {
				return err
			}
		}
		return nil
	}
	for _, page := range pages {
		if err := capture(page, ""); err != nil {
			return nil, err
		}
	}
	return pages, nil
}

func validStorageOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

// Restore in disposable TARGET pages with the source frame-origin topology,
// preserving partitioning. Fetch interception serves inert in-memory documents:
// no website is contacted, no source page/context is created or navigated, and
// no private values are written to temporary files. Only these target pages are
// closed afterward. Persisted IndexedDB/opaque browser state is never exported.
func (manager *RoomManagerCtx) restoreLocalStorage(ctx context.Context, address string, pages []*cloneFrame) error {
	if len(pages) == 0 {
		return nil
	}
	browser, err := manager.connectBrowser(ctx, address)
	if err != nil {
		return err
	}
	defer browser.close()
	for _, page := range pages {
		if !validStorageOrigin(page.Frame.Origin) {
			continue // about:blank/new-tab frames cannot have HTTP localStorage.
		}
		if err := browser.restoreStoragePage(ctx, page); err != nil {
			return err
		}
	}
	return nil
}

func (browser *browserCDP) restoreStoragePage(ctx context.Context, page *cloneFrame) error {
	token, err := utils.NewUID(24)
	if err != nil {
		return errors.New("cannot reserve controlled clone storage page")
	}
	documents := map[string]string{}
	var document func(*cloneFrame) (string, error)
	document = func(frame *cloneFrame) (string, error) {
		if !validStorageOrigin(frame.Frame.Origin) {
			if len(frame.Children) != 0 {
				return "", errors.New("active opaque frame storage topology cannot be cloned")
			}
			return "", nil
		}
		address := strings.TrimSuffix(frame.Frame.Origin, "/") + "/.neko-clone/" + token + "/" + strconv.Itoa(len(documents))
		documents[address] = "" // Reserve before recursively rendering children.
		children := ""
		for _, child := range frame.Children {
			childAddress, err := document(child)
			if err != nil {
				return "", err
			}
			if childAddress != "" {
				children += `<iframe src="` + html.EscapeString(childAddress) + `"></iframe>`
			}
		}
		entries := frame.Entries
		if entries == nil {
			entries = json.RawMessage(`[]`)
		}
		// Marshal escapes HTML delimiters even inside RawMessage, retaining
		// arbitrary JSON/UTF-16 storage strings without script injection.
		encoded, err := json.Marshal(entries)
		if err != nil {
			return "", errors.New("invalid active browser storage snapshot")
		}
		documents[address] = `<!doctype html><script>let ok=true;const entries=` + string(encoded) + `;try{localStorage.clear();for(const [k,v] of entries)localStorage.setItem(k,v);ok=entries.every(([k,v])=>localStorage.getItem(k)===v)}catch(e){ok=false}window.__nekoCloneDone=new Promise(resolve=>{let remaining=` + strconv.Itoa(strings.Count(children, "<iframe ")) + `;function done(){if(remaining===0){if(parent!==window)parent.postMessage({clone:'` + token + `',ok},'*');resolve(ok)}}addEventListener('message',e=>{if(e.data&&e.data.clone==='` + token + `'){ok=ok&&e.data.ok;remaining--;done()}});done()});</script>` + children
		return address, nil
	}
	address, err := document(page)
	if err != nil {
		return err
	}
	var target struct {
		ID string `json:"targetId"`
	}
	if err := browser.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &target); err != nil {
		return err
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		browser.call(cleanup, "Target.closeTarget", map[string]string{"targetId": target.ID}, nil)
	}()
	var attached struct {
		ID string `json:"sessionId"`
	}
	if err := browser.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached); err != nil {
		return err
	}
	for _, method := range []string{"Page.enable", "Fetch.enable"} {
		if err := browser.callSession(ctx, attached.ID, method, nil, nil); err != nil {
			return err
		}
	}
	if _, err := browser.send(ctx, attached.ID, "Page.navigate", map[string]string{"url": address}); err != nil {
		return err
	}
	for {
		var event struct {
			Method  string          `json:"method"`
			Error   json.RawMessage `json:"error"`
			Session string          `json:"sessionId"`
			Params  struct {
				ID      string `json:"requestId"`
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			} `json:"params"`
		}
		if browser.conn.ReadJSON(&event) != nil || len(event.Error) != 0 {
			return errors.New("controlled target storage document failed")
		}
		if event.Method == "Fetch.requestPaused" {
			body, exists := documents[event.Params.Request.URL]
			if !exists {
				body = "<!doctype html>" // Also intercept favicon/unexpected fetches.
			}
			if _, err := browser.send(ctx, event.Session, "Fetch.fulfillRequest", map[string]any{"requestId": event.Params.ID, "responseCode": 200, "responseHeaders": []any{map[string]string{"name": "Content-Type", "value": "text/html; charset=utf-8"}}, "body": base64.StdEncoding.EncodeToString([]byte(body))}); err != nil {
				return err
			}
		}
		if event.Method == "Page.loadEventFired" && event.Session == attached.ID {
			break
		}
	}
	var evaluation struct {
		Result struct {
			Value bool `json:"value"`
		} `json:"result"`
	}
	if err := browser.callSession(ctx, attached.ID, "Runtime.evaluate", map[string]any{"expression": "window.__nekoCloneDone", "awaitPromise": true, "returnByValue": true}, &evaluation); err != nil || !evaluation.Result.Value {
		return errors.New("target rejected active localStorage restoration")
	}
	return nil
}
