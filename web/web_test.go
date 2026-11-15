package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// readAll returns every embedded file with one of the given extensions.
func readAll(t *testing.T, exts ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, e := range exts {
			if path.Ext(p) == e {
				b, err := fs.ReadFile(FS(), p)
				if err != nil {
					return err
				}
				out[p] = string(b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded FS: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("no embedded %v files", exts)
	}
	return out
}

func TestFS_HasPagesAndExample(t *testing.T) {
	for _, name := range []string{
		"index.html", "methodology.html", "privacy.html",
		"app.js", "methodology.js", "app.css",
		"examples/demo.jsonl.gz",
	} {
		b, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	// The example is the real gzip sample, not a placeholder.
	if b, err := fs.ReadFile(FS(), "examples/demo.jsonl.gz"); err == nil && (len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b) {
		t.Errorf("examples/demo.jsonl.gz is not gzip")
	}
}

// TestFS_Conforms checks the overlay of examples/ on static/ behaves as a
// well-formed fs.FS (directory listings, Stat names, ReadDir paging).
func TestFS_Conforms(t *testing.T) {
	if err := fstest.TestFS(FS(), "index.html", "methodology.html", "privacy.html",
		"app.js", "methodology.js", "app.css", "examples/demo.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
}

// TestNoUnsafeDOMSinks: every value from an upload reaches the page through
// textContent or attributes, never through an HTML-parsing sink.
func TestNoUnsafeDOMSinks(t *testing.T) {
	sinks := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"}
	for name, src := range readAll(t, ".js") {
		for _, s := range sinks {
			if strings.Contains(src, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
	}
}

// stringTimerRe matches setTimeout/setInterval given a string (code) instead
// of a function.
var stringTimerRe = regexp.MustCompile(`set(?:Timeout|Interval)\(\s*["'` + "`" + `]`)

// TestNoStringTimers: a string timer is eval by another name.
func TestNoStringTimers(t *testing.T) {
	for name, src := range readAll(t, ".js") {
		if m := stringTimerRe.FindString(src); m != "" {
			t.Errorf("%s uses a string timer: %q", name, m)
		}
	}
}

// TestAppReadsLimitsFromAPI: the Limits line comes from the server, with the
// static text only as the fallback.
func TestAppReadsLimitsFromAPI(t *testing.T) {
	src := readAll(t, ".js")["app.js"]
	for _, want := range []string{"/api/v1/limits", "max_decompressed_bytes", "run_timeout_seconds", "$('limits').textContent"} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	html, err := fs.ReadFile(FS(), "index.html")
	if err != nil || !strings.Contains(string(html), `id="limits"`) {
		t.Errorf(`index.html lacks the id="limits" element: %v`, err)
	}
}

var (
	scriptTagRe   = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	scriptSrcRe   = regexp.MustCompile(`(?i)\ssrc\s*=`)
	styleAttrRe   = regexp.MustCompile(`(?i)<[a-z][^>]*\sstyle\s*=`)
	styleTagRe    = regexp.MustCompile(`(?i)<style\b`)
	handlerAttrRe = regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)
)

// TestNoInlineScriptsOrStyles: the CSP allows only same-origin script and
// style files, so inline code would be blocked (and must not be relied on).
func TestNoInlineScriptsOrStyles(t *testing.T) {
	for name, src := range readAll(t, ".html") {
		for _, tag := range scriptTagRe.FindAllString(src, -1) {
			if !scriptSrcRe.MatchString(tag) {
				t.Errorf("%s: inline script %q", name, tag)
			}
		}
		if m := styleAttrRe.FindString(src); m != "" {
			t.Errorf("%s: style attribute in %q", name, m)
		}
		if styleTagRe.MatchString(src) {
			t.Errorf("%s: <style> element", name)
		}
		if m := handlerAttrRe.FindString(src); m != "" {
			t.Errorf("%s: inline event handler in %q", name, m)
		}
	}
}

var (
	urlRe       = regexp.MustCompile(`https?://[^\s"'<>)]*`)
	anchorURLRe = regexp.MustCompile(`(?i)<a\b[^>]*\shref\s*=\s*"(https?://[^"]*)"`)
	srcURLRe    = regexp.MustCompile(`(?i)\ssrc\s*=\s*["']?https?://`)
	linkURLRe   = regexp.MustCompile(`(?i)<link\b[^>]*\shref\s*=\s*["']?https?://`)
	fetchURLRe  = regexp.MustCompile("fetch\\(\\s*[\"'`]https?://")
)

// allowedLink reports whether u is one of the links the UI may point at.
func allowedLink(u string) bool {
	for _, base := range []string{"https://github.com/instrumentation-score/spec", "https://github.com/ericmustin/vern"} {
		if u == base || strings.HasPrefix(u, base+"/") {
			return true
		}
	}
	return false
}

// TestNoExternalResources: no external scripts, styles, fonts or API calls.
// The only absolute URLs are the spec and repo links (and the SVG namespace,
// which is an identifier, not a resource).
func TestNoExternalResources(t *testing.T) {
	for name, src := range readAll(t, ".html") {
		if srcURLRe.MatchString(src) || linkURLRe.MatchString(src) {
			t.Errorf("%s loads an external resource", name)
		}
		anchors := map[string]bool{}
		for _, m := range anchorURLRe.FindAllStringSubmatch(src, -1) {
			anchors[m[1]] = true
			if !allowedLink(m[1]) {
				t.Errorf("%s: link to %q", name, m[1])
			}
		}
		for _, u := range urlRe.FindAllString(src, -1) {
			if !anchors[u] {
				t.Errorf("%s: absolute URL %q outside an <a href>", name, u)
			}
		}
	}
	for name, src := range readAll(t, ".js") {
		if fetchURLRe.MatchString(src) {
			t.Errorf("%s fetches an absolute URL", name)
		}
		for _, u := range urlRe.FindAllString(src, -1) {
			if u != "http://www.w3.org/2000/svg" && !allowedLink(u) {
				t.Errorf("%s: absolute URL %q", name, u)
			}
		}
	}
	for name, src := range readAll(t, ".css") {
		if urlRe.MatchString(src) || strings.Contains(src, "@import") {
			t.Errorf("%s references an external resource", name)
		}
	}
}

// TestTargetBlankLinksAreIsolated: links opening a new tab carry
// rel="noopener noreferrer" in the HTML pages.
func TestTargetBlankLinksAreIsolated(t *testing.T) {
	anchorRe := regexp.MustCompile(`(?i)<a\b[^>]*>`)
	for name, src := range readAll(t, ".html") {
		for _, a := range anchorRe.FindAllString(src, -1) {
			if strings.Contains(a, `target="_blank"`) && !strings.Contains(a, `rel="noopener noreferrer"`) {
				t.Errorf("%s: %q lacks rel=\"noopener noreferrer\"", name, a)
			}
		}
	}
}

// TestVendorNamesOnlyOnMethodologyAndPrivacy: the main page is vendor-neutral.
func TestVendorNamesOnlyOnMethodologyAndPrivacy(t *testing.T) {
	vendorRe := regexp.MustCompile(`(?i)elastic|kibana|opensearch|clickhouse`)
	for _, name := range []string{"index.html", "app.js", "app.css"} {
		b, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m := vendorRe.FindString(string(b)); m != "" {
			t.Errorf("%s mentions %q", name, m)
		}
	}
}

// TestNoToFixed: scores are shown with the API's display strings
// (score.Format); toFixed rounds 6.25 to 6.3 and would disagree.
func TestNoToFixed(t *testing.T) {
	for name, src := range readAll(t, ".js") {
		if strings.Contains(src, "toFixed") {
			t.Errorf("%s calls toFixed", name)
		}
	}
}

// TestAppRendersBlindSpotsAsText: app.js shows the sample's blind_spots in
// the sample panel and the disclosure, and only through list(), whose items
// are set with textContent (el's text prop). TestNoUnsafeDOMSinks covers the
// rest of the file.
func TestAppRendersBlindSpotsAsText(t *testing.T) {
	src := readAll(t, ".js")["app.js"]
	for _, want := range []string{
		"report.sample.blind_spots", // read from the report
		"s.blind_spots",             // the sample panel
		"list(spots)",               // rendered through list()
		"el('li', { text: it })",    // list() items are textContent
		"node.textContent = String(props.text)",
		"r.sensitive", // the disclosure keys on sensitive rules
		// passBlindSpots reads the outcome's API field (score.Outcome's
		// json tag), not a made-up name.
		"outs.some(function (o) { return o.rule_passed; })",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// Outcomes carry rule_passed; a bare o.passed is always undefined.
	if regexp.MustCompile(`\bo\.passed\b`).MatchString(src) {
		t.Error("app.js reads o.passed; the outcome field is rule_passed")
	}
	// Every use of blind spots goes through list(): no other rendering.
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "spots") && (strings.Contains(line, "textContent") || strings.Contains(line, "createTextNode") || strings.Contains(line, "setAttribute")) {
			t.Errorf("blind spots rendered outside list(): %s", strings.TrimSpace(line))
		}
	}
}

// TestAppPollsReadyzForWarmup: while the hosted engine starts, the page polls
// /readyz and shows the warming-up text through textContent (setStatus), and
// a 503 that says "warming up" is not shown as "busy".
func TestAppPollsReadyzForWarmup(t *testing.T) {
	src := readAll(t, ".js")["app.js"]
	for _, want := range []string{
		"'/readyz'",
		"Starting the scorer. After an idle period this takes about a minute.",
		"The scorer did not start. Reload the page to try again.",
		"WARMING_POLL_MS = 3000",
		"3 * 60 * 1000",
		"setStatus(WARMING_TEXT, true)",
		"warming up",
		"checkReady();",
		"s.textContent = text || ''",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// TestApp413DoesNotRequireJSON: Cloud Run's front end answers an oversized
// upload with 413 and an HTML body, so the 413 branch must work without a
// JSON error body.
func TestApp413DoesNotRequireJSON(t *testing.T) {
	src := readAll(t, ".js")["app.js"]
	i := strings.Index(src, "async function serverMessage(res)")
	if i < 0 {
		t.Fatal("app.js has no serverMessage")
	}
	fn := src[i : i+strings.Index(src[i:], "\n  }\n")]
	if !strings.Contains(fn, "catch (e)") || !strings.Contains(fn, "return ''") {
		t.Errorf("serverMessage must tolerate a non-JSON body:\n%s", fn)
	}
	j := strings.Index(src, "case 413:")
	if j < 0 {
		t.Fatal("app.js has no 413 branch")
	}
	branch := src[j : j+strings.Index(src[j:], "break;")]
	if !strings.Contains(branch, "msg ?") || !strings.Contains(branch, "LIMITS_TEXT") {
		t.Errorf("413 branch must fall back to the limits text when msg is empty:\n%s", branch)
	}
}
