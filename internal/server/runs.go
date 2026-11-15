package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/report"
)

// multipartOverhead is allowed on top of Limits.MaxCompressedBytes for
// multipart boundaries and part headers.
const multipartOverhead = 1 << 20

var errNoFiles = errors.New("no files in upload")

// handleRuns scores one uploaded sample synchronously and returns the report.
func (s *server) handleRuns(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "too many runs in progress; try again shortly")
		return
	}
	defer func() { <-s.sem }()

	ri := infoFrom(r.Context())
	maxBody := s.limits.MaxCompressedBytes
	if isMultipart(r) {
		maxBody += multipartOverhead
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	inputs, names, err := readInputs(r, s.limits.MaxFiles)
	if err != nil {
		s.failUpload(w, r, ri, err)
		return
	}
	sample, err := otlpin.Decode(inputs, s.limits)
	if err != nil {
		s.failUpload(w, r, ri, err)
		return
	}
	// WriteTimeout started when the headers were read; the upload, decode
	// and Ready below may have eaten into it, so give the response a fresh
	// window covering the whole run.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(s.runTimeout + readyTimeout + 15*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.logf("run: extending write deadline: %v", err)
	}
	s.scoreSample(w, r, ri, sample, names)
}

func isMultipart(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "multipart/form-data"
}

// scoreSample checks the engine, scores the sample under RunTimeout and
// writes the report.
func (s *server) scoreSample(w http.ResponseWriter, r *http.Request, ri *reqInfo, sample *otlpin.Sample, names []string) {
	rctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	err := s.cfg.Engine.Ready(rctx)
	cancel()
	if err != nil {
		if r.Context().Err() != nil {
			s.clientClosed(ri)
			return
		}
		s.logf("run: engine not ready: %v", err)
		w.Header().Set("Retry-After", "15")
		writeError(w, http.StatusServiceUnavailable, "the scoring engine is not ready; try again shortly")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.runTimeout)
	defer cancel()
	opts := s.cfg.Options
	opts.Files = names
	opts.Sweep = 0 // RunJanitor owns sweeping
	// The server log is shared: a failed sensitive-rule query's error can
	// echo field and metric names from the upload, so it is never logged.
	opts.QueryErrorDetail = false
	rep, err := s.score(ctx, s.cfg.Engine, sample, opts)
	if err == nil && rep == nil {
		err = errors.New("score returned no report")
	}
	if err != nil {
		s.failRun(w, r, ri, err)
		return
	}
	// Apply the trust check whatever ScoreFunc produced (idempotent).
	rep.Problems = evaluate.Problems(rep)
	if rep.Sample != nil {
		ri.runID = rep.Sample.RunID
	}

	var body any = rep
	if r.URL.Query().Get("markdown") == "1" {
		body = reportWithMarkdown{Report: rep, Markdown: report.RenderMarkdown(rep)}
	}
	if err := writeJSON(w, http.StatusOK, body); err != nil {
		s.logf("run: writing response: %v", err)
	}
}

// reportWithMarkdown is the report object plus a top-level "markdown" field.
type reportWithMarkdown struct {
	*evaluate.Report
	Markdown string `json:"markdown"`
}

// readInputs turns the request body into decoder inputs. A multipart body
// yields one input per part with a filename (read into memory, named by its
// base name); any other body is a single input named "upload".
func readInputs(r *http.Request, maxFiles int) ([]otlpin.Input, []string, error) {
	if !isMultipart(r) {
		return []otlpin.Input{{Name: "upload", R: r.Body}}, []string{"upload"}, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("malformed multipart upload: %w", err)
	}
	var inputs []otlpin.Input
	var names []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, multipartErr(err)
		}
		if p.FileName() == "" {
			continue // NextPart discards the rest of this part
		}
		if len(inputs) >= maxFiles {
			return nil, nil, fmt.Errorf("more than %d files: %w", maxFiles, otlpin.ErrTooManyFiles)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, nil, multipartErr(err)
		}
		name := path.Base(p.FileName())
		inputs = append(inputs, otlpin.Input{Name: name, R: bytes.NewReader(data)})
		names = append(names, name)
	}
	if len(inputs) == 0 {
		return nil, nil, errNoFiles
	}
	return inputs, names, nil
}

func multipartErr(err error) error {
	return fmt.Errorf("malformed multipart upload: %w", err)
}

// failUpload maps a body-reading or decode error. Size limits are 413; any
// other decode error is the client's input, so a 400 with its message
// (JSON-encoded on the way out).
func (s *server) failUpload(w http.ResponseWriter, r *http.Request, ri *reqInfo, err error) {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds the size limit (%d MiB compressed)", s.limits.MaxCompressedBytes>>20))
	case errors.Is(err, otlpin.ErrTooLarge), errors.Is(err, otlpin.ErrTooManyItems), errors.Is(err, otlpin.ErrTooManyFiles):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case r.Context().Err() != nil:
		s.clientClosed(ri)
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// failRun maps an engine or scoring error. 5xx bodies never carry the
// internal error (it may name the cluster); it goes to the log instead.
func (s *server) failRun(w http.ResponseWriter, r *http.Request, ri *reqInfo, err error) {
	switch {
	case r.Context().Err() != nil:
		s.clientClosed(ri)
	case errors.Is(err, context.DeadlineExceeded):
		s.logf("run: timed out: %s", describeRunError(err))
		writeError(w, http.StatusGatewayTimeout, "scoring took too long and was stopped")
	case errors.Is(err, otlpin.ErrEmpty):
		writeError(w, http.StatusBadRequest, otlpin.ErrEmpty.Error())
	default:
		s.logf("run: internal error: %s", describeRunError(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// runStages are the fixed prefixes samplescore (and this handler) put on
// their errors; they name the stage that failed without quoting any data.
var runStages = []string{
	"engine not ready",
	"load sample",
	"evaluate",
	"load embedded mappings",
	"load embedded spec rules",
	"score returned no report",
}

var httpStatusRe = regexp.MustCompile(`\b(?:status|HTTP) (\d{3})\b`)

// describeRunError is what the log gets for a failed run instead of the
// error text. Engine errors quote backend response bodies, and a rejected
// batch's body can echo uploaded values, which must never reach the log. So
// only a bounded classification is logged: the failing stage (from a fixed
// list), the first HTTP status code mentioned (digits only), and whether a
// purge also failed. The full error stays available where it is the user's
// own data: `vern score` prints it to the user's terminal.
func describeRunError(err error) string {
	text := err.Error()
	stage := "unknown"
	for _, p := range runStages {
		if strings.HasPrefix(text, p) {
			stage = p
			break
		}
	}
	out := fmt.Sprintf("stage=%q", stage)
	if m := httpStatusRe.FindStringSubmatch(text); m != nil {
		out += " http_status=" + m[1]
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		out += " cause=deadline"
	case errors.Is(err, context.Canceled):
		out += " cause=canceled"
	}
	if strings.Contains(text, "\npurge run ") {
		out += " purge_failed=true"
	}
	return out + " (detail withheld: engine errors can quote uploaded data)"
}

// clientClosed records a request the client abandoned: no response, one log
// line. The error is not logged: mid-upload it can quote the body.
func (s *server) clientClosed(ri *reqInfo) {
	ri.clientClosed = true
	s.logf("run: client closed the request")
}
