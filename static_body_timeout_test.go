package main

import (
	"strings"
	"testing"
)

// The fetch deadline must cover the response body, not just the headers: a
// body that stalls after the headers would otherwise leave response.json()
// pending forever and with it a caller's in-flight flag. verify-dashboard.mjs
// drives the behaviour; this pins the wiring.
func TestDashboardFetchDeadlineCoversTheBody(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`const deadlineMS = Date.now() + timeoutMS;`,
		`timeBoundResponseBody(response, controller, deadlineMS, timeoutMS);`,
		`function timeBoundResponseBody(response, controller, deadlineMS, timeoutMS) {`,
		`for (const method of ["json", "text"]) {`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Errorf("app.js is missing %q", needle)
		}
	}
}
