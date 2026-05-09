package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

type Client struct {
	KibanaURL  string
	APIKey     string
	HTTPClient *http.Client
}

func NewClient(kibanaURL, apiKey string) *Client {
	return &Client{
		KibanaURL:  strings.TrimRight(kibanaURL, "/"),
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type UploadResult struct {
	StatusCode int
	Body       string
	WorkflowID string
	Name       string
	Valid      bool
	Replaced   []string // IDs deleted before upload (when Replace=true)
}

type listResp struct {
	Results []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"results"`
}

// FindByName returns workflow IDs whose name matches `name` exactly.
func (c *Client) FindByName(name string) ([]string, error) {
	if c.KibanaURL == "" || c.APIKey == "" {
		return nil, nil
	}
	req, err := http.NewRequest(http.MethodGet, c.KibanaURL+"/api/workflows", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("x-elastic-internal-origin", "Kibana")
	req.Header.Set("Authorization", "ApiKey "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list workflows: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed listResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	var ids []string
	for _, r := range parsed.Results {
		if r.Name == name {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// DeleteByIDs removes the given workflow IDs.
func (c *Client) DeleteByIDs(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string][]string{"ids": ids})
	req, err := http.NewRequest(http.MethodDelete, c.KibanaURL+"/api/workflows", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("x-elastic-internal-origin", "Kibana")
	req.Header.Set("Authorization", "ApiKey "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("delete workflows: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return nil
}

func (c *Client) Upload(yamlContent []byte, dryRun bool) (*UploadResult, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"workflows": []map[string]string{
			{"yaml": string(yamlContent)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	if dryRun {
		fmt.Println("[dry-run] POST", c.KibanaURL+"/api/workflows")
		fmt.Println("[dry-run] payload size:", len(payload), "bytes")
		fmt.Println("[dry-run] yaml size:", len(yamlContent), "bytes")
		return &UploadResult{StatusCode: 0, Body: "(dry-run)"}, nil
	}

	if c.KibanaURL == "" {
		return nil, fmt.Errorf("kibana URL is required")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	req, err := http.NewRequest(http.MethodPost, c.KibanaURL+"/api/workflows", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("x-elastic-internal-origin", "Kibana")
	req.Header.Set("Authorization", "ApiKey "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := &UploadResult{StatusCode: resp.StatusCode, Body: string(body)}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return result, fmt.Errorf("unauthorized (401): check that VERN_API_KEY / --api-key is a valid Kibana API key")
	case resp.StatusCode == http.StatusForbidden:
		return result, fmt.Errorf("forbidden (403): API key lacks permission to create workflows")
	case resp.StatusCode == http.StatusNotFound:
		return result, fmt.Errorf("not found (404): %s — confirm this is the Kibana URL (not Elasticsearch) and Workflows is enabled", c.KibanaURL+"/api/workflows")
	case resp.StatusCode >= 400:
		return result, fmt.Errorf("upload failed: HTTP %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	var parsed struct {
		Created []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Valid bool   `json:"valid"`
		} `json:"created"`
		Failed []struct {
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Created) > 0 {
		result.WorkflowID = parsed.Created[0].ID
		result.Name = parsed.Created[0].Name
		result.Valid = parsed.Created[0].Valid
	}
	if len(parsed.Failed) > 0 {
		msgs := make([]string, 0, len(parsed.Failed))
		for _, f := range parsed.Failed {
			msgs = append(msgs, f.Error)
		}
		return result, fmt.Errorf("upload failed: %s", strings.Join(msgs, "; "))
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// UpsertSkill creates or replaces an Elastic Agent Builder skill via
// PUT /api/agent_builder/skills/{id} (with POST fallback for first-time
// creation). PUT bodies must NOT include `id` (path-only); POST bodies must.
func (c *Client) UpsertSkill(id string, bodyNoID, bodyWithID []byte) (string, error) {
	return c.upsertAgentBuilderResource("skills", id, bodyNoID, bodyWithID)
}

// UpsertAgent creates or replaces an Elastic Agent Builder agent.
//
//   - On PUT /api/agent_builder/agents/{id}, the API rejects `id` in the body
//     (it must be path-only). We pass `bodyNoID` for that.
//   - On POST /api/agent_builder/agents (used as fallback when the agent
//     doesn't exist), the API requires `id` in the body. We pass `bodyWithID`.
//
// Returns the created/updated agent's id.
func (c *Client) UpsertAgent(id string, bodyNoID, bodyWithID []byte) (string, error) {
	return c.upsertAgentBuilderResource("agents", id, bodyNoID, bodyWithID)
}

// upsertAgentBuilderResource is the shared PUT-then-POST-fallback path used
// for both /skills/{id} and /agents/{id}. Both endpoints follow the same
// schema rules: PUT body has no `id` (path-only), POST body has `id`.
func (c *Client) upsertAgentBuilderResource(resource, id string, bodyNoID, bodyWithID []byte) (string, error) {
	if c.KibanaURL == "" {
		return "", fmt.Errorf("kibana URL is required")
	}
	if c.APIKey == "" {
		return "", fmt.Errorf("API key is required")
	}

	url := fmt.Sprintf("%s/api/agent_builder/%s/%s", c.KibanaURL, resource, id)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(bodyNoID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("x-elastic-internal-origin", "Kibana")
	req.Header.Set("Authorization", "ApiKey "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("PUT %s: %w", resource, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		req2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/agent_builder/%s", c.KibanaURL, resource), bytes.NewReader(bodyWithID))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("kbn-xsrf", "true")
		req2.Header.Set("x-elastic-internal-origin", "Kibana")
		req2.Header.Set("Authorization", "ApiKey "+c.APIKey)
		resp2, err := c.HTTPClient.Do(req2)
		if err != nil {
			return "", fmt.Errorf("POST %s: %w", resource, err)
		}
		defer resp2.Body.Close()
		respBody, _ = io.ReadAll(resp2.Body)
		if resp2.StatusCode >= 400 {
			return "", fmt.Errorf("create %s: HTTP %d: %s", resource, resp2.StatusCode, truncate(string(respBody), 500))
		}
		return parseAgentID(respBody, id), nil
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upsert %s: HTTP %d: %s", resource, resp.StatusCode, truncate(string(respBody), 500))
	}
	return parseAgentID(respBody, id), nil
}

func parseAgentID(body []byte, fallback string) string {
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.ID != "" {
		return parsed.ID
	}
	return fallback
}

// SavedObjectsImportResult captures the outcome of POST /api/saved_objects/_import.
type SavedObjectsImportResult struct {
	Success      bool
	SuccessCount int
	Errors       []string
	Imported     []string // "type:id (title)" entries
	StatusCode   int
}

// ImportSavedObjects uploads an NDJSON of Kibana saved objects via
// POST /api/saved_objects/_import?overwrite=true. The endpoint requires a
// multipart/form-data body with a "file" field.
func (c *Client) ImportSavedObjects(ndjson []byte) (*SavedObjectsImportResult, error) {
	if c.KibanaURL == "" {
		return nil, fmt.Errorf("kibana URL is required")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="dashboards.ndjson"`)
	header.Set("Content-Type", "application/ndjson")
	part, err := mw.CreatePart(header)
	if err != nil {
		return nil, fmt.Errorf("multipart create: %w", err)
	}
	if _, err := part.Write(ndjson); err != nil {
		return nil, fmt.Errorf("multipart write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("multipart close: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.KibanaURL+"/api/saved_objects/_import?overwrite=true", body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("kbn-xsrf", "true")
	req.Header.Set("x-elastic-internal-origin", "Kibana")
	req.Header.Set("Authorization", "ApiKey "+c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	result := &SavedObjectsImportResult{StatusCode: resp.StatusCode}
	if resp.StatusCode >= 400 {
		return result, fmt.Errorf("import saved objects: HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var parsed struct {
		Success        bool `json:"success"`
		SuccessCount   int  `json:"successCount"`
		SuccessResults []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Meta struct {
				Title string `json:"title"`
			} `json:"meta"`
		} `json:"successResults"`
		Errors []struct {
			Type  string                 `json:"type"`
			ID    string                 `json:"id"`
			Title string                 `json:"title"`
			Error map[string]interface{} `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return result, fmt.Errorf("decode import response: %w", err)
	}

	result.Success = parsed.Success
	result.SuccessCount = parsed.SuccessCount
	for _, r := range parsed.SuccessResults {
		result.Imported = append(result.Imported, fmt.Sprintf("%s:%s (%s)", r.Type, r.ID, r.Meta.Title))
	}
	for _, e := range parsed.Errors {
		msg := fmt.Sprintf("%s:%s", e.Type, e.ID)
		if e.Title != "" {
			msg += " (" + e.Title + ")"
		}
		if e.Error != nil {
			if t, ok := e.Error["type"].(string); ok {
				msg += " — " + t
			}
		}
		result.Errors = append(result.Errors, msg)
	}
	return result, nil
}
