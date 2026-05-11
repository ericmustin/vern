// Package specsync fetches the upstream Instrumentation Score spec at a
// pinned git ref and reports drift against the locally vendored copy in
// ./spec/. It does not run as part of the build — operators invoke it
// explicitly via `vern spec sync` when they want to refresh.
package specsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Client fetches spec files via the GitHub REST API. A pinned ref (tag or
// commit SHA) is required; passing "main" or any branch will work but is
// discouraged because it makes builds non-reproducible.
type Client struct {
	Repo string // e.g. "instrumentation-score/spec"
	Ref  string // tag, branch, or commit SHA
	HTTP *http.Client
}

const (
	// rulesSubdir is the path inside the upstream repo that holds rule
	// markdown files. Vendored locally under ./spec/rules.
	rulesSubdir = "rules"

	// specFile is the path inside the upstream repo for the spec doc itself.
	specFile = "specification.md"
)

// FileStatus describes the local/upstream relationship for a single file.
type FileStatus struct {
	Path     string // path relative to the local spec dir
	Local    bool   // file exists locally
	Upstream bool   // file exists upstream
	Drift    bool   // contents differ (only meaningful when both Local and Upstream)
	// SHA hints; we report upstream's SHA from GitHub when available.
	UpstreamSHA string
}

// Status enumerates the categorical drift state for human-readable output.
func (f FileStatus) Status() string {
	switch {
	case !f.Local && f.Upstream:
		return "added-upstream"
	case f.Local && !f.Upstream:
		return "removed-upstream"
	case f.Drift:
		return "modified"
	default:
		return "in-sync"
	}
}

// Report aggregates per-file status across the spec tree.
type Report struct {
	Repo       string
	Ref        string
	GeneratedAt time.Time
	Files      []FileStatus
}

// NumOutOfSync returns the count of files whose Status is not "in-sync".
func (r Report) NumOutOfSync() int {
	n := 0
	for _, f := range r.Files {
		if f.Status() != "in-sync" {
			n++
		}
	}
	return n
}

// DefaultClient builds a Client with a 30s HTTP timeout.
func DefaultClient(repo, ref string) *Client {
	return &Client{
		Repo: repo,
		Ref:  ref,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// Compare fetches the upstream spec at the pinned ref and compares it against
// the local spec directory (which must contain specification.md and a rules/
// subdir to match upstream layout).
func (c *Client) Compare(ctx context.Context, localDir string) (*Report, error) {
	rep := &Report{
		Repo:        c.Repo,
		Ref:         c.Ref,
		GeneratedAt: time.Now().UTC(),
	}

	// Compare top-level specification.md.
	if status, err := c.compareFile(ctx, localDir, specFile, specFile); err != nil {
		return nil, err
	} else {
		rep.Files = append(rep.Files, status)
	}

	// Compare every rule markdown file. We union the local rules/*.md set
	// with the upstream listing.
	upstreamRules, err := c.listRulesDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("list upstream rules: %w", err)
	}

	localRules, err := listLocalRules(filepath.Join(localDir, "rules"))
	if err != nil {
		return nil, err
	}

	names := map[string]struct{}{}
	for n := range upstreamRules {
		names[n] = struct{}{}
	}
	for n := range localRules {
		names[n] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	for _, name := range ordered {
		rel := filepath.Join("rules", name)
		upPath := rulesSubdir + "/" + name
		status, err := c.compareFile(ctx, localDir, rel, upPath)
		if err != nil {
			return nil, err
		}
		if sha, ok := upstreamRules[name]; ok {
			status.UpstreamSHA = sha
		}
		rep.Files = append(rep.Files, status)
	}

	return rep, nil
}

// Apply downloads every drifted/missing file from upstream and writes it to
// localDir. Files that exist locally but not upstream are left alone (the
// operator must remove them manually).
func (c *Client) Apply(ctx context.Context, localDir string, rep *Report) error {
	for _, f := range rep.Files {
		switch f.Status() {
		case "in-sync", "removed-upstream":
			continue
		}
		upPath := f.Path
		if strings.HasPrefix(upPath, "rules"+string(filepath.Separator)) {
			upPath = rulesSubdir + "/" + filepath.Base(upPath)
		}
		body, err := c.fetchRaw(ctx, upPath)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", upPath, err)
		}
		target := filepath.Join(localDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// listRulesDir returns a map of {filename: sha} for upstream rules/*.md.
func (c *Client) listRulesDir(ctx context.Context) (map[string]string, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s",
		c.Repo, rulesSubdir, c.Ref)
	body, err := c.doGet(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decode contents listing: %w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(e.Name, ".md") {
			continue
		}
		if strings.HasPrefix(e.Name, "_") {
			continue
		}
		out[e.Name] = e.SHA
	}
	return out, nil
}

func listLocalRules(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	out := map[string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, "_") {
			continue
		}
		out[name] = struct{}{}
	}
	return out, nil
}

func (c *Client) compareFile(ctx context.Context, localDir, relPath, upstreamPath string) (FileStatus, error) {
	status := FileStatus{Path: relPath}

	localBytes, localErr := os.ReadFile(filepath.Join(localDir, relPath))
	if localErr == nil {
		status.Local = true
	} else if !os.IsNotExist(localErr) {
		return status, localErr
	}

	upBytes, upErr := c.fetchRaw(ctx, upstreamPath)
	switch {
	case upErr == nil:
		status.Upstream = true
	case isNotFound(upErr):
		// file gone upstream; leave Upstream=false
	default:
		return status, upErr
	}

	if status.Local && status.Upstream {
		status.Drift = string(localBytes) != string(upBytes)
	}
	return status, nil
}

// notFoundErr signals a 404 from the upstream raw endpoint.
type notFoundErr struct{ path string }

func (e *notFoundErr) Error() string { return "not found: " + e.path }

func isNotFound(err error) bool {
	_, ok := err.(*notFoundErr)
	return ok
}

func (c *Client) fetchRaw(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", c.Repo, c.Ref, path)
	body, err := c.doGet(ctx, url, "")
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (c *Client) doGet(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &notFoundErr{path: url}
	}
	if resp.StatusCode >= 400 {
		bodySnip, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(bodySnip)))
	}
	return io.ReadAll(resp.Body)
}
