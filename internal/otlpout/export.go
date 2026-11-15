package otlpout

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

const maxBodyEcho = 256

// Export POSTs the report as OTLP/HTTP protobuf. Errors never include header
// values or URL userinfo.
func Export(ctx context.Context, rep *evaluate.Report, c Config) error {
	if !c.Enabled() {
		return nil
	}
	shown := redactURL(c.MetricsURL)
	body, err := pmetricotlp.NewExportRequestFromMetrics(Build(rep, c)).MarshalProto()
	if err != nil {
		return fmt.Errorf("otlp export: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.MetricsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("otlp export: invalid endpoint %s", shown)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{
			Timeout: 10 * time.Second,
			// Never forward auth headers to a redirect target.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := cl.Do(req)
	if err != nil {
		// url.Error embeds the URL (with userinfo); report only the cause.
		cause := error(err)
		if ue, ok := err.(*url.Error); ok {
			cause = ue.Err
		}
		return fmt.Errorf("otlp export: POST %s: %w", shown, cause)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	msg := fmt.Sprintf("otlp export: POST %s: HTTP %d", shown, resp.StatusCode)
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); strings.HasPrefix(mt, "text/") || strings.Contains(mt, "json") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyEcho))
		if s := strings.TrimSpace(string(b)); s != "" {
			msg += ": " + s
		}
	}
	return fmt.Errorf("%s", msg)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	u.User, u.RawQuery = nil, ""
	return u.String()
}
