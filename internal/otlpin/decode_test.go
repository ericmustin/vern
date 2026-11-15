package otlpin

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const (
	tracesDoc = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]},"scopeSpans":[{"scope":{"name":"s"},"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"op","kind":1,"startTimeUnixNano":"1544712660000000000","endTimeUnixNano":"1544712661000000000","events":[{"timeUnixNano":"1544712660500000000","name":"ev"}]}]}]}]}`
	logsDoc   = `{"resourceLogs":[{"resource":{},"scopeLogs":[{"scope":{},"logRecords":[{"timeUnixNano":"1544712660300000000","body":{"stringValue":"hi"}}]}]}]}`
	metricDoc = `{"resourceMetrics":[{"resource":{},"scopeMetrics":[{"scope":{},"metrics":[{"name":"m","gauge":{"dataPoints":[{"timeUnixNano":"1544712660300000000","asInt":"5"},{"timeUnixNano":"1544712660400000000","asInt":"6"}]}}]}]}]}`
)

func in(name, s string) Input { return Input{Name: name, R: strings.NewReader(s)} }

func counts(t *testing.T, s *Sample) (spans, events, logs, dps int) {
	t.Helper()
	rs := s.Traces.ResourceSpans()
	for i := 0; i < rs.Len(); i++ {
		for j := 0; j < rs.At(i).ScopeSpans().Len(); j++ {
			sp := rs.At(i).ScopeSpans().At(j).Spans()
			spans += sp.Len()
			for k := 0; k < sp.Len(); k++ {
				events += sp.At(k).Events().Len()
			}
		}
	}
	logs = s.Logs.LogRecordCount()
	dps = s.Metrics.DataPointCount()
	return
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecode_JSONDocumentEachSignal(t *testing.T) {
	s, err := Decode([]Input{in("t.json", tracesDoc), in("l.json", logsDoc), in("m.json", metricDoc)}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	sp, ev, lg, dp := counts(t, s)
	if sp != 1 || ev != 1 || lg != 1 || dp != 2 {
		t.Fatalf("got spans=%d events=%d logs=%d dps=%d", sp, ev, lg, dp)
	}
	if got := s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID().String(); got != "5b8efff798038103d269b633813fc60c" {
		t.Fatalf("trace id %s", got)
	}
}

func TestDecode_JSONLMixedSignals(t *testing.T) {
	data := tracesDoc + "\n\n" + logsDoc + "\n" + metricDoc + "\n"
	s, err := Decode([]Input{in("c.jsonl", data)}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	sp, _, lg, dp := counts(t, s)
	if sp != 1 || lg != 1 || dp != 2 {
		t.Fatalf("got spans=%d logs=%d dps=%d", sp, lg, dp)
	}
}

func TestDecode_GzipAndZip(t *testing.T) {
	data := []byte(tracesDoc + "\n" + logsDoc + "\n" + metricDoc + "\n")
	plain, err := Decode([]Input{{Name: "c.jsonl", R: bytes.NewReader(data)}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Decode([]Input{{Name: "c.jsonl.gz", R: bytes.NewReader(gz(t, data))}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, n := range []string{"a.jsonl", "b.jsonl.gz"} {
		w, _ := zw.Create(n)
		if strings.HasSuffix(n, ".gz") {
			w.Write(gz(t, data))
		} else {
			w.Write(data)
		}
	}
	zw.Close()
	z, err := Decode([]Input{{Name: "c.zip", R: &zb}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	a1, b1, c1, d1 := counts(t, plain)
	a2, b2, c2, d2 := counts(t, g)
	a3, b3, c3, d3 := counts(t, z)
	if a1 != a2 || b1 != b2 || c1 != c2 || d1 != d2 {
		t.Fatalf("gzip mismatch")
	}
	if a3 != 2*a1 || b3 != 2*b1 || c3 != 2*c1 || d3 != 2*d1 {
		t.Fatalf("zip counts %d %d %d %d vs single %d %d %d %d", a3, b3, c3, d3, a1, b1, c1, d1)
	}
}

func TestDecode_BareResourceSpans(t *testing.T) {
	doc := `{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"j"}}]},"scopeSpans":[{"scope":{},"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"op"}]}]}`
	s, err := Decode([]Input{in("j.json", doc)}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if s.Traces.ResourceSpans().Len() != 1 {
		t.Fatalf("resources=%d", s.Traces.ResourceSpans().Len())
	}
}

func TestDecode_ConsoleExporterRefused(t *testing.T) {
	doc := `{"name":"op","context":{"trace_id":"0x5b8efff798038103d269b633813fc60c","span_id":"0xeee19b7ec3c1b174"}}`
	_, err := Decode([]Input{in("py.json", doc)}, DefaultLimits)
	if !errors.Is(err, ErrConsoleExporter) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "file exporter") {
		t.Fatalf("message should mention collector file exporter: %v", err)
	}
}

func TestDecode_TruncatedLastLineSkipped(t *testing.T) {
	data := tracesDoc + "\n" + logsDoc + "\n" + `{"resourceMetrics":[{"resou`
	s, err := Decode([]Input{in("capture.jsonl", data)}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	sp, _, lg, _ := counts(t, s)
	if sp != 1 || lg != 1 {
		t.Fatalf("spans=%d logs=%d", sp, lg)
	}
	if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "skipped truncated last line") || !strings.Contains(s.Notes[0], "capture.jsonl") {
		t.Fatalf("notes=%v", s.Notes)
	}
}

func TestDecode_MalformedMiddleLineFails(t *testing.T) {
	data := tracesDoc + "\n" + `{"resourceLogs":[{"resou` + "\n" + logsDoc + "\n"
	_, err := Decode([]Input{in("capture.jsonl", data)}, DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "capture.jsonl:2:") {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_Limits(t *testing.T) {
	t.Run("gzip bomb", func(t *testing.T) {
		bomb := gz(t, bytes.Repeat([]byte("\n"), 10<<20))
		lim := DefaultLimits
		lim.MaxDecompressedBytes = 1 << 20
		_, err := Decode([]Input{{Name: "b.gz", R: bytes.NewReader(bomb)}}, lim)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("compressed", func(t *testing.T) {
		lim := DefaultLimits
		lim.MaxCompressedBytes = 10
		_, err := Decode([]Input{in("t.json", tracesDoc)}, lim)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("items", func(t *testing.T) {
		var sb strings.Builder
		for i := 0; i < 3; i++ {
			sb.WriteString(strings.ReplaceAll(tracesDoc, `,"events":[{"timeUnixNano":"1544712660500000000","name":"ev"}]`, ""))
			sb.WriteString("\n")
		}
		lim := DefaultLimits
		lim.MaxItems = 2
		_, err := Decode([]Input{in("t.jsonl", sb.String())}, lim)
		if !errors.Is(err, ErrTooManyItems) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("files", func(t *testing.T) {
		var zb bytes.Buffer
		zw := zip.NewWriter(&zb)
		lim := DefaultLimits
		for i := 0; i <= lim.MaxFiles; i++ {
			w, _ := zw.Create(fmt.Sprintf("f%d.json", i))
			io.WriteString(w, tracesDoc)
		}
		zw.Close()
		_, err := Decode([]Input{{Name: "many.zip", R: &zb}}, lim)
		if !errors.Is(err, ErrTooManyFiles) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestDecode_EmptyInput(t *testing.T) {
	for _, inputs := range [][]Input{nil, {in("e.jsonl", "")}, {in("e.jsonl", "\n\n")}} {
		if _, err := Decode(inputs, DefaultLimits); !errors.Is(err, ErrEmpty) {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestDecode_DemoSample(t *testing.T) {
	f, err := os.Open("../../web/examples/demo.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := Decode([]Input{{Name: "demo.jsonl.gz", R: f}}, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	sp, _, lg, dp := counts(t, s)
	if sp == 0 || lg == 0 || dp == 0 {
		t.Fatalf("spans=%d logs=%d dps=%d", sp, lg, dp)
	}
	if len(s.Notes) != 0 {
		t.Fatalf("notes=%v", s.Notes)
	}
	if s.Traces.ResourceSpans().Len() != 18 || s.Logs.ResourceLogs().Len() != 90 || s.Metrics.ResourceMetrics().Len() != 20 {
		t.Fatalf("resource counts %d/%d/%d", s.Traces.ResourceSpans().Len(), s.Logs.ResourceLogs().Len(), s.Metrics.ResourceMetrics().Len())
	}
}

func indent(t *testing.T, doc string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(doc), "", "  "); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func zipOf(t *testing.T, members map[string][]byte, order ...string) []byte {
	t.Helper()
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, n := range order {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(members[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return zb.Bytes()
}

func TestDecode_PrettyPrintedDocument(t *testing.T) {
	for name, data := range map[string]string{
		"plain":      indent(t, tracesDoc),
		"blank":      "\n\n" + indent(t, tracesDoc),
		"bom":        "\xef\xbb\xbf" + indent(t, tracesDoc) + "\n",
		"two-pretty": indent(t, tracesDoc) + "\n" + indent(t, tracesDoc),
	} {
		s, err := Decode([]Input{in("p.json", data)}, DefaultLimits)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := 1
		if name == "two-pretty" {
			want = 2
		}
		if sp, _, _, _ := counts(t, s); sp != want {
			t.Fatalf("%s: spans=%d", name, sp)
		}
	}
}

func TestDecode_ConcatenatedPrettyConsoleObjects(t *testing.T) {
	obj := "{\n    \"name\": \"op\",\n    \"context\": {\n        \"trace_id\": \"0x5b8efff798038103d269b633813fc60c\",\n        \"span_id\": \"0xeee19b7ec3c1b174\"\n    }\n}\n"
	_, err := Decode([]Input{in("py.txt", obj+obj+obj)}, DefaultLimits)
	if !errors.Is(err, ErrConsoleExporter) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_SingleLineTruncated(t *testing.T) {
	_, err := Decode([]Input{in("t.jsonl", `{"resourceSpans":[{"res`)}, DefaultLimits)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err=%v", err)
	}
	s, err := Decode([]Input{in("t.jsonl", `{"resourceSpans":[{"res`), in("ok.json", logsDoc)}, DefaultLimits)
	if err != nil || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "t.jsonl") {
		t.Fatalf("err=%v notes=%v", err, s)
	}
}

func TestDecode_LineErrorWrapsSentinel(t *testing.T) {
	data := tracesDoc + "\n" + `{"foo":1}` + "\n" + logsDoc + "\n"
	_, err := Decode([]Input{in("capture.jsonl", data)}, DefaultLimits)
	if !errors.Is(err, ErrUnrecognized) || !strings.Contains(err.Error(), "capture.jsonl:2:") {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_NestedBombs(t *testing.T) {
	newlines := bytes.Repeat([]byte("\n"), 10<<20)
	lim := DefaultLimits
	lim.MaxDecompressedBytes = 1 << 20
	cases := map[string][]byte{
		"zip-in-gzip":    gz(t, zipOf(t, map[string][]byte{"a.jsonl": newlines}, "a.jsonl")),
		"gzip-in-zip":    zipOf(t, map[string][]byte{"a.gz": gz(t, newlines)}, "a.gz"),
		"zip-member-raw": zipOf(t, map[string][]byte{"a.jsonl": newlines}, "a.jsonl"),
	}
	for name, data := range cases {
		_, err := Decode([]Input{{Name: name, R: bytes.NewReader(data)}}, lim)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestDecode_ZipCentralDirectoryBound(t *testing.T) {
	lim := DefaultLimits
	names := make([]string, 60_000)
	members := map[string][]byte{}
	for i := range names {
		names[i] = fmt.Sprintf("%d", i)
		members[names[i]] = nil
	}
	data := zipOf(t, members, names...)
	// Lie in the EOCD: claim a single entry.
	eocd := bytes.LastIndex(data, []byte("PK\x05\x06"))
	data[eocd+8], data[eocd+9], data[eocd+10], data[eocd+11] = 1, 0, 1, 0
	// The pre-walk must reject without building the reader (no per-entry
	// allocation). The bound is far below one allocation per entry (60k) but
	// leaves room for the extra allocations -race instrumentation adds.
	d := &decoder{lim: lim}
	if allocs := testing.AllocsPerRun(3, func() { _ = d.checkZipEntries("big.zip", data) }); allocs > 50 {
		t.Fatalf("checkZipEntries allocated %v times", allocs)
	}
	if err := d.checkZipEntries("big.zip", data); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("checkZipEntries err=%v", err)
	}
	_, err := Decode([]Input{{Name: "big.zip", R: bytes.NewReader(data)}}, lim)
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_ZipEOCDMissingAndZip64(t *testing.T) {
	data := zipOf(t, map[string][]byte{"a": []byte(tracesDoc)}, "a")
	_, err := Decode([]Input{{Name: "x.zip", R: bytes.NewReader(data[:len(data)-10])}}, DefaultLimits)
	if err == nil {
		t.Fatal("truncated zip accepted")
	}
	eocd := bytes.LastIndex(data, []byte("PK\x05\x06"))
	data[eocd+10], data[eocd+11] = 0xFF, 0xFF
	_, err = Decode([]Input{{Name: "x.zip", R: bytes.NewReader(data)}}, DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "zip64") {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_MaxNesting(t *testing.T) {
	data := []byte(tracesDoc)
	for i := 0; i < maxNesting; i++ {
		data = gz(t, data)
	}
	if _, err := Decode([]Input{{Name: "n.gz", R: bytes.NewReader(data)}}, DefaultLimits); err != nil {
		t.Fatalf("depth %d: %v", maxNesting, err)
	}
	data = gz(t, data)
	_, err := Decode([]Input{{Name: "n.gz", R: bytes.NewReader(data)}}, DefaultLimits)
	if !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_CompressedLimitSummedAcrossInputs(t *testing.T) {
	lim := DefaultLimits
	lim.MaxCompressedBytes = int64(len(tracesDoc)) + 10
	if _, err := Decode([]Input{in("a.json", tracesDoc)}, lim); err != nil {
		t.Fatal(err)
	}
	_, err := Decode([]Input{in("a.json", tracesDoc), in("b.json", logsDoc)}, lim)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecode_ZipForgedDirectoryRejected(t *testing.T) {
	members := map[string][]byte{}
	names := make([]string, 1000)
	for i := range names {
		names[i] = fmt.Sprintf("m%d", i)
		members[names[i]] = []byte(tracesDoc)
	}
	good := zipOf(t, members, names...)
	eocdOf := func(b []byte) int { return bytes.LastIndex(b, []byte("PK\x05\x06")) }
	d := &decoder{lim: DefaultLimits}
	cases := map[string]func([]byte) []byte{
		"cdSize=0": func(b []byte) []byte {
			e := eocdOf(b)
			copy(b[e+12:e+16], []byte{0, 0, 0, 0})
			return b
		},
		"cdOff forged": func(b []byte) []byte {
			e := eocdOf(b)
			b[e+16]++
			return b
		},
		"prepended junk": func(b []byte) []byte {
			return append([]byte("PK\x03\x04junkjunkjunk"), b...)
		},
	}
	for name, mutate := range cases {
		data := mutate(append([]byte(nil), good...))
		if err := d.checkZipEntries("z.zip", data); !errors.Is(err, ErrUnrecognized) {
			t.Fatalf("%s: checkZipEntries err=%v", name, err)
		}
		_, err := Decode([]Input{{Name: "z.zip", R: bytes.NewReader(data)}}, DefaultLimits)
		if !errors.Is(err, ErrUnrecognized) {
			t.Fatalf("%s: Decode err=%v", name, err)
		}
	}
	// Unmodified archive with 1000 members is still bounded by MaxFiles.
	lim := DefaultLimits
	if _, err := Decode([]Input{{Name: "z.zip", R: bytes.NewReader(good)}}, lim); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("good err=%v", err)
	}
}
