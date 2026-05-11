package semconv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// FetchClient fetches semconv model YAMLs from GitHub at a pinned ref.
// We list model/ recursively via the Git Trees API and then download each
// YAML in parallel via raw.githubusercontent.com.
type FetchClient struct {
	Repo string
	Ref  string
	HTTP *http.Client
	// Concurrency limits parallel raw downloads. Defaults to 8.
	Concurrency int
}

func DefaultFetchClient(repo, ref string) *FetchClient {
	return &FetchClient{
		Repo:        repo,
		Ref:         ref,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		Concurrency: 8,
	}
}

// FetchAll downloads every YAML under model/ and returns a map of
// {upstream-path: file-body}. The map is suitable input to Build.
func (c *FetchClient) FetchAll(ctx context.Context) (map[string][]byte, error) {
	paths, err := c.listModelYAMLs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list model/: %w", err)
	}

	out := make(map[string][]byte, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.Concurrency)
	errCh := make(chan error, 1)

	for _, p := range paths {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			body, err := c.fetchRaw(ctx, p)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("fetch %s: %w", p, err):
				default:
				}
				return
			}
			mu.Lock()
			out[p] = body
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	select {
	case e := <-errCh:
		return nil, e
	default:
	}
	return out, nil
}

func (c *FetchClient) listModelYAMLs(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/git/trees/%s?recursive=1", c.Repo, c.Ref)
	body, err := c.doGet(ctx, url, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, fmt.Errorf("decode tree: %w", err)
	}
	if tree.Truncated {
		return nil, fmt.Errorf("upstream git tree truncated — pinned ref has too many files for recursive listing")
	}
	out := []string{}
	for _, e := range tree.Tree {
		if e.Type != "blob" {
			continue
		}
		if !strings.HasPrefix(e.Path, "model/") {
			continue
		}
		if !(strings.HasSuffix(e.Path, ".yaml") || strings.HasSuffix(e.Path, ".yml")) {
			continue
		}
		out = append(out, e.Path)
	}
	return out, nil
}

func (c *FetchClient) fetchRaw(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", c.Repo, c.Ref, path)
	return c.doGet(ctx, url, "")
}

func (c *FetchClient) doGet(ctx context.Context, url, accept string) ([]byte, error) {
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
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}
