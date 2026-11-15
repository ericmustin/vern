package elastic

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/engine"
)

func TestFields_BreakerIsBusy(t *testing.T) {
	for name, status := range map[string]int{"429": 429, "breaker": 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"type":"circuit_breaking_exception"}}`))
		}))
		e := New(Config{URL: srv.URL, HTTP: srv.Client()})
		_, err := e.Fields(t.Context(), newRunID(t, time.Now()))
		srv.Close()
		if !errors.Is(err, engine.ErrBusy) {
			t.Errorf("%s: err = %v, want ErrBusy", name, err)
		}
	}
}
