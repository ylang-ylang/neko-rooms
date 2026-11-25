package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestFormLoginSelfContainedNativeMarkup(t *testing.T) {
	for _, prefix := range []string{"/manage", "/nested/owner", "/"} {
		t.Run(prefix, func(t *testing.T) {
			h := newAdminHarness(t, prefix, nil, false)
			// The harness has no rooms and no public manager assets. The login
			// must still supply its own styling/brand without authenticating.
			res, body := h.request(t, "GET", h.loginURL, "", nil, nil)
			expectStatus(t, res, http.StatusOK, body)
			assertNativeForm(t, body, h.loginURL)
			if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'" {
				t.Fatal("self-contained login changed cache or content security policy")
			}

			var brand, heading bool
			visitLoginMarkup(t, body, func(node *html.Node, attrs map[string]string) {
				switch node.Data {
				case "svg":
					brand = attrs["viewBox"] == "0 0 100 100" && attrs["aria-hidden"] == "true" && attrs["focusable"] == "false"
				case "h2":
					heading = attrs["id"] == "login-title" && node.FirstChild != nil && node.FirstChild.Data == "neko-rooms · Owner sign in"
				case "script", "link", "img", "image", "use", "iframe", "object", "embed", "audio", "video", "source":
					t.Fatalf("login requires an external or executable resource: %s", node.Data)
				case "style":
					if node.FirstChild == nil || strings.Contains(node.FirstChild.Data, "url(") || strings.Contains(node.FirstChild.Data, "@import") {
						t.Fatal("login styling must not request fonts, room assets or CDN resources")
					}
				}
			})
			if !brand || !heading {
				t.Fatal("native Neko brand or distinct Owner management heading missing")
			}
		})
	}
}

func TestFormLoginErrorAccessibleAndPreservesSafeFields(t *testing.T) {
	h := newAdminHarness(t, "/manage", nil, false)
	username := `"><script>alert(1)</script>`
	next := `/manage/index.html?tab=%22%3E%3Cscript%3E&view=rooms`
	password := "never-render-this-submitted-password"
	res, body := h.request(t, "POST", h.loginURL, url.Values{
		"username": {username}, "password": {password}, "next": {next},
	}.Encode(), http.Header{"Origin": {h.server.URL}, "Content-Type": {"application/x-www-form-urlencoded"}}, nil)
	expectStatus(t, res, http.StatusUnauthorized, body)
	assertNativeForm(t, body, h.loginURL)
	if strings.Contains(body, password) || len(res.Cookies()) != 0 {
		t.Fatal("failed login echoed the password or issued a session")
	}

	var alert, keptUsername, keptNext, describedPassword bool
	visitLoginMarkup(t, body, func(node *html.Node, attrs map[string]string) {
		if node.Data == "p" && attrs["id"] == "login-error" {
			alert = attrs["role"] == "alert" && node.FirstChild != nil && node.FirstChild.Data == "Invalid username or password."
		}
		if node.Data == "input" {
			switch attrs["name"] {
			case "username":
				keptUsername = attrs["value"] == username && attrs["aria-describedby"] == "login-error"
			case "next":
				keptNext = attrs["type"] == "hidden" && attrs["value"] == next
			case "password":
				describedPassword = attrs["aria-describedby"] == "login-error" && attrs["value"] == ""
			}
		}
		if node.Data == "script" {
			t.Fatal("submitted fields escaped their HTML attributes")
		}
	})
	if !alert || !keptUsername || !keptNext || !describedPassword {
		t.Fatal("error is not accessible or safe form fields were not preserved")
	}
}

func visitLoginMarkup(t *testing.T, body string, inspect func(*html.Node, map[string]string)) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode {
			attrs := make(map[string]string, len(node.Attr))
			for _, attr := range node.Attr {
				attrs[attr.Key] = attr.Val
			}
			inspect(node, attrs)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
}
