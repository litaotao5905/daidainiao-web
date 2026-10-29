package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func webSub(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestPWAManifestAndIcons(t *testing.T) {
	web := webSub(t)
	srv, _ := newTestServer(t)
	srv.web = web
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "localhost:8080"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/manifest.webmanifest")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m struct {
		Name, ShortName, StartURL, Scope, Display, Lang, ThemeColor, BackgroundColor string
		Icons                                                                        []struct{ Src, Sizes, Type, Purpose string }
	}
	raw := rec.Body.Bytes()
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	m.Name, _ = generic["name"].(string)
	m.ShortName, _ = generic["short_name"].(string)
	m.StartURL, _ = generic["start_url"].(string)
	m.Scope, _ = generic["scope"].(string)
	m.Display, _ = generic["display"].(string)
	m.Lang, _ = generic["lang"].(string)
	if m.Name != "网页呆呆鸟" || m.ShortName == "" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" || m.Lang != "zh-CN" {
		t.Fatalf("manifest fields: %v", generic)
	}
	if generic["theme_color"] == "" || generic["background_color"] == "" || generic["description"] == "" {
		t.Fatal("manifest colours/description missing")
	}
	icons, _ := generic["icons"].([]any)
	var any192, any512, maskable bool
	for _, ic := range icons {
		im := ic.(map[string]any)
		src, _ := im["src"].(string)
		rec := get(src)
		if rec.Code != 200 {
			t.Fatalf("icon %s: %d", src, rec.Code)
		}
		if im["type"] == "image/png" {
			pic, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			want := im["sizes"].(string)
			if got := strings.Join([]string{itoa(pic.Bounds().Dx()), itoa(pic.Bounds().Dy())}, "x"); got != want {
				t.Fatalf("%s is %s, manifest says %s", src, got, want)
			}
			switch {
			case im["purpose"] == "maskable" && want == "512x512":
				maskable = true
				// maskable icons must be opaque (full bleed)
				if _, _, _, a := pic.At(0, 0).RGBA(); a != 0xffff {
					t.Error("maskable icon has transparent corners")
				}
			case im["purpose"] == "any" && want == "192x192":
				any192 = true
			case im["purpose"] == "any" && want == "512x512":
				any512 = true
			}
		}
	}
	if !any192 || !any512 || !maskable {
		t.Fatalf("manifest needs 192, 512 and maskable 512 icons: %v %v %v", any192, any512, maskable)
	}

	// apple touch icon (180) and favicon.ico (16/32/48)
	rec = get("/apple-touch-icon.png")
	pic, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if rec.Code != 200 || err != nil || pic.Bounds().Dx() != 180 {
		t.Fatalf("apple-touch-icon: %d %v", rec.Code, err)
	}
	rec = get("/favicon.ico")
	b := rec.Body.Bytes()
	if rec.Code != 200 || len(b) < 6 || binary.LittleEndian.Uint16(b[2:]) != 1 || binary.LittleEndian.Uint16(b[4:]) != 3 {
		t.Fatalf("favicon.ico: %d", rec.Code)
	}
	var sizes []int
	for i := 0; i < 3; i++ {
		sizes = append(sizes, int(b[6+16*i]))
	}
	if sizes[0] != 16 || sizes[1] != 32 || sizes[2] != 48 {
		t.Fatalf("ico sizes %v", sizes)
	}
	if rec := get("/favicon.svg"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatal("favicon.svg")
	}

	// page links
	idx := get("/").Body.String()
	for _, want := range []string{`rel="manifest" href="/manifest.webmanifest"`, `rel="apple-touch-icon"`, `rel="icon" href="/favicon.svg"`, `name="theme-color"`, `src="pwa.js"`} {
		if !strings.Contains(idx, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestServiceWorkerIsMinimalAndNeverCachesData(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.web = webSub(t)
	req := httptest.NewRequest("GET", "/sw.js", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
	}
	sw := rec.Body.String()
	if !strings.Contains(sw, `addEventListener("fetch"`) {
		t.Fatal("no fetch handler: Chrome would not offer installation")
	}
	for _, bad := range []string{"cache.put", ".put(", "addAll", "caches.open(CACHE).then((c) => c.put"} {
		if strings.Contains(sw, bad) {
			t.Errorf("service worker stores responses (%s)", bad)
		}
	}
	for _, must := range []string{`pathname.startsWith("/api/")`, `req.mode !== "navigate"`, "OFFLINE"} {
		if !strings.Contains(sw, must) {
			t.Errorf("service worker lacks %s", must)
		}
	}
	// the only cached URL is the static offline page
	if strings.Count(sw, "c.add(") != 1 || !strings.Contains(sw, `const OFFLINE = "/offline.html"`) {
		t.Error("service worker should pre-cache only /offline.html")
	}
	if _, err := fs.Stat(srv.web, "offline.html"); err != nil {
		t.Fatal(err)
	}
	// the OAuth callback is never intercepted: it is a top-level navigation to the
	// same origin, which falls through to fetch() (network first); only a failed
	// fetch (server down) shows the offline page.
	if !strings.Contains(sw, "fetch(req).catch(") {
		t.Error("navigations must go to the network first")
	}
	_ = http.StatusOK
}
