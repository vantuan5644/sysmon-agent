package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The script error banner: boot.js loads first (not deferred) so it can report
// app.js failing to load, parse or start; the banner starts hidden; the
// service worker precaches boot.js with the rest of the shell. The behaviour
// itself is driven by verify-dashboard.mjs.
func TestDashboardScriptErrorBanner(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := staticFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	index := read("static/index.html")
	boot := strings.Index(index, `<script src="/boot.js"></script>`)
	app := strings.Index(index, `<script src="/app.js" defer></script>`)
	if boot < 0 || app < 0 || boot > app {
		t.Fatalf("index.html must load /boot.js (not deferred) before /app.js: boot at %d, app at %d", boot, app)
	}
	if !strings.Contains(index, `<div id="scriptError" class="script-error" role="alert" hidden>`) {
		t.Fatal("index.html is missing the hidden #scriptError banner")
	}
	for _, id := range []string{"scriptErrorText", "scriptErrorReload", "scriptErrorMore"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Fatalf("index.html is missing #%s", id)
		}
	}
	if sw := read("static/sw.js"); !strings.Contains(sw, `"/boot.js"`) {
		t.Fatal("sw.js does not precache /boot.js")
	}
	bootJS := read("static/boot.js")
	for _, needle := range []string{
		`const bootTimeoutMS = 10000;`,
		`if (!window.sysmonDashboardStarted) {`,
		`window.sysmonScriptErrors`,
		`could not load`,
	} {
		if !strings.Contains(bootJS, needle) {
			t.Fatalf("boot.js is missing %q", needle)
		}
	}
	appJS := read("static/app.js")
	for _, needle := range []string{
		`window.sysmonScriptErrors = true;`,
		`window.addEventListener("unhandledrejection", (event) => {`,
		`window.sysmonDashboardStarted = true;`,
		"const message = `Dashboard script error: ${detail}`;",
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js is missing %q", needle)
		}
	}
	if !strings.Contains(read("static/styles.css"), ".script-error[hidden] {") {
		t.Fatal("styles.css must keep a hidden #scriptError out of the layout")
	}
}

func TestBootScriptIsServedUnderTheCSP(t *testing.T) {
	handler, err := newHTTPHandlerWithController(fakeCollector{}, staticFS, NewMemoryRuntimeState(), &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sysmonDashboardStarted") {
		t.Fatalf("GET /boot.js = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("GET /boot.js Content-Type = %q", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q, want script-src 'self'", csp)
	}
}
