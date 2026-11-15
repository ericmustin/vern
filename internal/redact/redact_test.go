package redact

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := map[string]string{
		"http://vern:pw@es:9200/?token=q#frag": "http://es:9200/",
		"https://vern@es.example/base?x=1":     "https://es.example/base",
		"http://es:9200":                       "http://es:9200",
		"http://vern:pw@[::1":                  "(invalid URL)",
	}
	for in, want := range cases {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTransportError(t *testing.T) {
	// A real client error: net/http masks only the password.
	req, _ := http.NewRequest(http.MethodGet, "http://vern:pw@127.0.0.1:1/_data_stream?token=q-secret", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, raw := http.DefaultClient.Do(req.WithContext(ctx))
	if raw == nil {
		t.Fatal("want a transport error")
	}
	err := TransportError(raw)
	for _, s := range []string{"vern", "pw", "q-secret"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error still contains %q: %v", s, err)
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cause lost: %v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) || ue.URL != "http://127.0.0.1:1/_data_stream" {
		t.Errorf("not a *url.Error with the redacted URL: %#v", err)
	}
	plain := errors.New("plain")
	if TransportError(plain) != plain {
		t.Error("non-url errors must pass through unchanged")
	}
}
