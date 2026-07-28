package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestWebClientParsesTheActualCSRFCookieName guards against the cookie name the API
// sets (CSRFCookieName) and the cookie name web/src/api.ts parses out of
// document.cookie drifting apart again (see issue: CSRF cookie name mismatch — every
// web UI mutation returns 403).
func TestWebClientParsesTheActualCSRFCookieName(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine source file location")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	apiTS := filepath.Join(repoRoot, "web", "src", "api.ts")
	data, err := os.ReadFile(apiTS)
	if err != nil {
		t.Fatalf("read %s: %v", apiTS, err)
	}
	source := string(data)

	declared := regexp.MustCompile(`CSRF_COOKIE_NAME\s*=\s*"([^"]+)"`).FindStringSubmatch(source)
	if declared == nil {
		t.Fatalf("web/src/api.ts does not declare a CSRF_COOKIE_NAME constant")
	}
	if declared[1] != CSRFCookieName {
		t.Fatalf("web/src/api.ts CSRF_COOKIE_NAME is %q, want %q (auth.CSRFCookieName)", declared[1], CSRFCookieName)
	}

	getCSRF := regexp.MustCompile(`getCSRF[\s\S]*?document\.cookie\.match[\s\S]*?};`).FindString(source)
	if getCSRF == "" {
		t.Fatalf("web/src/api.ts does not define getCSRF parsing document.cookie")
	}
	if !regexp.MustCompile(`CSRF_COOKIE_NAME`).MatchString(getCSRF) {
		t.Fatalf("web/src/api.ts getCSRF does not build its cookie regex from CSRF_COOKIE_NAME")
	}
}
