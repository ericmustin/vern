package specsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpstream simulates the subset of the GitHub API we touch:
// - GET https://raw.githubusercontent.com/<repo>/<ref>/<path>
// - GET https://api.github.com/repos/<repo>/contents/<path>?ref=<ref>
func fakeUpstream(t *testing.T, files map[string]string) (string, string, func()) {
	t.Helper()

	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path comes in like /<repo>/<ref>/<file-path>
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		filePath := parts[3]
		body, ok := files[filePath]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/<owner>/<name>/contents/<subdir>?ref=<ref>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[3] != "contents" {
			http.NotFound(w, r)
			return
		}
		subdir := strings.Join(parts[4:], "/")
		entries := []map[string]string{}
		prefix := subdir + "/"
		for path := range files {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			name := strings.TrimPrefix(path, prefix)
			if strings.Contains(name, "/") {
				continue
			}
			entries = append(entries, map[string]string{
				"name": name,
				"type": "file",
				"sha":  "fake-sha-" + name,
			})
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))

	return rawSrv.URL, apiSrv.URL, func() {
		rawSrv.Close()
		apiSrv.Close()
	}
}

// fetchOverride wires Client to test servers by overriding endpoints.
// We do this with a tiny custom transport that rewrites the upstream hosts.
type rewriteTransport struct {
	rawHost string
	apiHost string
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "raw.githubusercontent.com" {
		newURL := r.rawHost + req.URL.Path
		req2, _ := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		req2.Header = req.Header
		return http.DefaultTransport.RoundTrip(req2)
	}
	if req.URL.Host == "api.github.com" {
		newURL := r.apiHost + req.URL.Path
		if req.URL.RawQuery != "" {
			newURL += "?" + req.URL.RawQuery
		}
		req2, _ := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		req2.Header = req.Header
		return http.DefaultTransport.RoundTrip(req2)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestCompare_DetectsDrift(t *testing.T) {
	upstream := map[string]string{
		"specification.md":   "upstream spec body\n",
		"rules/RES-005.md":   "**Rule ID:** RES-005\n",
		"rules/MET-007.md":   "**Rule ID:** MET-007 (new upstream)\n",
	}
	rawHost, apiHost, cleanup := fakeUpstream(t, upstream)
	defer cleanup()

	// Local dir: same RES-005 contents, missing MET-007, modified specification.md.
	local := t.TempDir()
	mustWrite(t, filepath.Join(local, "specification.md"), "older local spec body\n")
	mustWrite(t, filepath.Join(local, "rules", "RES-005.md"), "**Rule ID:** RES-005\n")
	mustWrite(t, filepath.Join(local, "rules", "DEAD-001.md"), "local-only rule\n")

	c := &Client{
		Repo: "fake/repo",
		Ref:  "v1.0.0",
		HTTP: &http.Client{
			Timeout:   3 * time.Second,
			Transport: &rewriteTransport{rawHost: rawHost, apiHost: apiHost},
		},
	}

	rep, err := c.Compare(context.Background(), local)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	status := map[string]string{}
	for _, f := range rep.Files {
		status[f.Path] = f.Status()
	}

	if got := status["specification.md"]; got != "modified" {
		t.Errorf("specification.md: want modified, got %s", got)
	}
	if got := status[filepath.Join("rules", "RES-005.md")]; got != "in-sync" {
		t.Errorf("RES-005.md: want in-sync, got %s", got)
	}
	if got := status[filepath.Join("rules", "MET-007.md")]; got != "added-upstream" {
		t.Errorf("MET-007.md: want added-upstream, got %s", got)
	}
	if got := status[filepath.Join("rules", "DEAD-001.md")]; got != "removed-upstream" {
		t.Errorf("DEAD-001.md: want removed-upstream, got %s", got)
	}
	if rep.NumOutOfSync() != 3 {
		t.Errorf("NumOutOfSync = %d, want 3", rep.NumOutOfSync())
	}
}

func TestApply_WritesUpstreamFiles(t *testing.T) {
	upstream := map[string]string{
		"specification.md":   "new spec\n",
		"rules/RES-005.md":   "new RES-005\n",
	}
	rawHost, apiHost, cleanup := fakeUpstream(t, upstream)
	defer cleanup()

	local := t.TempDir()
	mustWrite(t, filepath.Join(local, "specification.md"), "old spec\n")
	// rules/ doesn't exist locally — Apply must create it.

	c := &Client{
		Repo: "fake/repo",
		Ref:  "v1.0.0",
		HTTP: &http.Client{
			Timeout:   3 * time.Second,
			Transport: &rewriteTransport{rawHost: rawHost, apiHost: apiHost},
		},
	}

	rep, err := c.Compare(context.Background(), local)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if err := c.Apply(context.Background(), local, rep); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(local, "specification.md"))
	if string(got) != "new spec\n" {
		t.Errorf("spec body: %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(local, "rules", "RES-005.md"))
	if string(got) != "new RES-005\n" {
		t.Errorf("RES-005 body: %q", got)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
