package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// requestCheckTestChecker returns a release checker on a clock the test moves,
// whose fetches are counted and answered by answer.
func requestCheckTestChecker(t *testing.T, current string, answer func() (ReleaseInfo, error)) (*UpdateChecker, *time.Time, *atomic.Int32) {
	t.Helper()
	checker := newUpdateChecker(UpdateCheckerOptions{CurrentVersion: current, Enabled: true})
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	checker.clock = func() time.Time { return now }
	fetches := &atomic.Int32{}
	checker.fetchRelease = func(context.Context, string) (ReleaseInfo, error) {
		fetches.Add(1)
		return answer()
	}
	return checker, &now, fetches
}

func releaseAnswer(tag string) func() (ReleaseInfo, error) {
	return func() (ReleaseInfo, error) {
		return ReleaseInfo{Tag: tag, URL: "https://example.com/" + tag}, nil
	}
}

func TestCheckOnRequestAsksAtMostOncePerGap(t *testing.T) {
	checker, now, fetches := requestCheckTestChecker(t, "v1.0.0", releaseAnswer("v1.2.0"))

	status, asked, err := checker.CheckOnRequest(context.Background())
	if err != nil || !asked || !status.Available || status.LatestVersion != "v1.2.0" {
		t.Fatalf("first tap = %+v, asked %v, %v; want an ask that finds v1.2.0", status, asked, err)
	}
	*now = now.Add(updateCheckRequestGap - time.Second)
	status, asked, err = checker.CheckOnRequest(context.Background())
	if err != nil || asked || !status.Available || status.LatestVersion != "v1.2.0" {
		t.Fatalf("tap inside the gap = %+v, asked %v, %v; want the last answer without an ask", status, asked, err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("GitHub was asked %d times inside the gap, want 1", got)
	}
	*now = now.Add(2 * time.Second)
	if _, asked, err = checker.CheckOnRequest(context.Background()); err != nil || !asked {
		t.Fatalf("tap after the gap: asked %v, %v; want a new ask", asked, err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("GitHub was asked %d times, want 2", got)
	}
}

func TestCheckOnRequestCountsTheBackgroundCheck(t *testing.T) {
	checker, now, fetches := requestCheckTestChecker(t, "v1.0.0", releaseAnswer("v1.0.0"))
	checker.checkAndCache(context.Background())
	*now = now.Add(10 * time.Second)
	status, asked, err := checker.CheckOnRequest(context.Background())
	if err != nil || asked || status.Available || status.LatestVersion != "v1.0.0" {
		t.Fatalf("tap after the background check = %+v, asked %v, %v; want its answer", status, asked, err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("GitHub was asked %d times, want 1", got)
	}
}

func TestCheckOnRequestSharesOneAskBetweenConcurrentTaps(t *testing.T) {
	inFetch := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	checker, _, fetches := requestCheckTestChecker(t, "v1.0.0", func() (ReleaseInfo, error) {
		once.Do(func() { close(inFetch) })
		<-release
		return ReleaseInfo{Tag: "v1.2.0"}, nil
	})

	type result struct {
		status UpdateStatus
		asked  bool
		err    error
	}
	results := make(chan result, 2)
	tap := func() {
		status, asked, err := checker.CheckOnRequest(context.Background())
		results <- result{status, asked, err}
	}
	go tap()
	<-inFetch
	go tap()
	// The second tap is now waiting behind the first one's ask, or about to.
	time.Sleep(20 * time.Millisecond)
	close(release)

	asks := 0
	for range 2 {
		got := <-results
		if got.err != nil || got.status.LatestVersion != "v1.2.0" || !got.status.Available {
			t.Fatalf("tap = %+v, %v; want v1.2.0 available", got.status, got.err)
		}
		if got.asked {
			asks++
		}
	}
	if asks != 1 || fetches.Load() != 1 {
		t.Fatalf("%d taps asked and GitHub saw %d fetches, want 1 and 1", asks, fetches.Load())
	}
}

func TestCheckOnRequestRefusesWithoutAsking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		enabled bool
		want    error
	}{
		{"turned off", "v1.0.0", false, errUpdateDisabled},
		{"dev build", "dev", true, errUpdateUnsupported},
		{"no version", "", true, errUpdateUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker, _, fetches := requestCheckTestChecker(t, tc.current, releaseAnswer("v9.9.9"))
			checker.SetEnabled(tc.enabled)
			status, asked, err := checker.CheckOnRequest(context.Background())
			if !errors.Is(err, tc.want) || asked || status.Available {
				t.Fatalf("tap = %+v, asked %v, %v; want %v and no ask", status, asked, err, tc.want)
			}
			if got := fetches.Load(); got != 0 {
				t.Fatalf("GitHub was asked %d times, want 0", got)
			}
		})
	}
}

func TestCheckOnRequestKeepsTheLastAnswerWhenGitHubFails(t *testing.T) {
	fail := false
	checker, now, fetches := requestCheckTestChecker(t, "v1.0.0", func() (ReleaseInfo, error) {
		if fail {
			return ReleaseInfo{}, errors.New("dial tcp: no route to host")
		}
		return ReleaseInfo{Tag: "v1.2.0"}, nil
	})
	if _, err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail = true
	*now = now.Add(updateCheckRequestGap)
	status, asked, err := checker.CheckOnRequest(context.Background())
	if err == nil || !asked || !status.Available || status.LatestVersion != "v1.2.0" {
		t.Fatalf("failed tap = %+v, asked %v, %v; want the error beside the last answer", status, asked, err)
	}
	if cached := checker.Status(); !cached.Available || cached.LatestVersion != "v1.2.0" {
		t.Fatalf("a failed ask replaced the cache with %+v", cached)
	}
	// A tap right after it reports the same failure without asking again.
	*now = now.Add(time.Second)
	if _, asked, err = checker.CheckOnRequest(context.Background()); err == nil || asked {
		t.Fatalf("tap after a failure: asked %v, %v; want the failure, no ask", asked, err)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("GitHub was asked %d times, want 2", got)
	}
}

func TestCheckOnRequestLeavesAnUpdateInProgressAlone(t *testing.T) {
	checker, _, fetches := requestCheckTestChecker(t, "v1.0.0", releaseAnswer("v1.2.0"))
	if !checker.beginApply() {
		t.Fatal("beginApply refused")
	}
	if _, asked, err := checker.CheckOnRequest(context.Background()); err != nil || asked {
		t.Fatalf("tap during an update: asked %v, %v; want no ask", asked, err)
	}
	if got := fetches.Load(); got != 0 {
		t.Fatalf("GitHub was asked %d times during an update, want 0", got)
	}
	// The background check still runs then, and must not offer the release
	// being installed.
	if status, err := checker.CheckNow(context.Background()); err != nil || status.Available {
		t.Fatalf("check during an update = %+v, %v; want not available", status, err)
	}
	if checker.Status().Available {
		t.Fatal("the cache offers the release being installed")
	}
}

func serveUpdateCheck(t *testing.T, checker *UpdateChecker, method, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	state := NewMemoryRuntimeState()
	if checker != nil {
		state.SetUpdateChecker(checker)
	}
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "https://sysmon.tailnet.example:9443/api/update/check", strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const updateCheckTestOrigin = "https://sysmon.tailnet.example:9443"

func TestUpdateCheckRouteAnswersWithTheUpdateStatus(t *testing.T) {
	checker, _, _ := requestCheckTestChecker(t, "v1.0.0", releaseAnswer("v1.2.0"))
	rec := serveUpdateCheck(t, checker, http.MethodPost, updateCheckTestOrigin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var answer struct {
		Asked  bool           `json:"asked"`
		Update map[string]any `json:"update"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Asked || answer.Update["available"] != true || answer.Update["latest_version"] != "v1.2.0" || answer.Error != "" {
		t.Fatalf("body = %s, want asked and v1.2.0 available", rec.Body.String())
	}
	// /api/status serves the same answer afterwards.
	if status := checker.Status(); status.LatestVersion != "v1.2.0" {
		t.Fatalf("cache after the route = %+v", status)
	}
}

func TestUpdateCheckRouteGuards(t *testing.T) {
	never := func() (ReleaseInfo, error) {
		t.Error("a refused request asked GitHub")
		return ReleaseInfo{}, errors.New("unexpected")
	}
	for _, tc := range []struct {
		name   string
		method string
		origin string
		body   string
		want   int
	}{
		{"cross origin", http.MethodPost, "https://example.invalid", "", http.StatusForbidden},
		{"no origin", http.MethodPost, "", "", http.StatusForbidden},
		{"a body", http.MethodPost, updateCheckTestOrigin, `{"force":true}`, http.StatusUnsupportedMediaType},
		// GET falls through to the static files, as it does for /api/update.
		{"GET", http.MethodGet, updateCheckTestOrigin, "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker, _, _ := requestCheckTestChecker(t, "v1.0.0", never)
			if rec := serveUpdateCheck(t, checker, tc.method, tc.origin, tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
	if rec := serveUpdateCheck(t, nil, http.MethodPost, updateCheckTestOrigin, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no checker: status = %d, want 503", rec.Code)
	}
}

func TestUpdateCheckRouteRefusalsAndFailures(t *testing.T) {
	off, _, _ := requestCheckTestChecker(t, "v1.0.0", releaseAnswer("v9.9.9"))
	off.SetEnabled(false)
	if rec := serveUpdateCheck(t, off, http.MethodPost, updateCheckTestOrigin, ""); rec.Code != http.StatusConflict {
		t.Fatalf("turned off: status = %d, want 409", rec.Code)
	}
	dev, _, _ := requestCheckTestChecker(t, "dev", releaseAnswer("v9.9.9"))
	if rec := serveUpdateCheck(t, dev, http.MethodPost, updateCheckTestOrigin, ""); rec.Code != http.StatusConflict {
		t.Fatalf("dev build: status = %d, want 409", rec.Code)
	}
	failing, _, _ := requestCheckTestChecker(t, "v1.0.0", func() (ReleaseInfo, error) {
		return ReleaseInfo{}, errors.New("dial tcp: no route to host")
	})
	rec := serveUpdateCheck(t, failing, http.MethodPost, updateCheckTestOrigin, "")
	var answer UpdateCheckAnswer
	if rec.Code != http.StatusBadGateway || json.Unmarshal(rec.Body.Bytes(), &answer) != nil || !answer.Asked || !strings.Contains(answer.Error, "no route to host") {
		t.Fatalf("failing GitHub: %d %s, want 502 naming the failure", rec.Code, rec.Body.String())
	}
}
