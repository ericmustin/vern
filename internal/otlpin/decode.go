// Package otlpin turns user-supplied OTLP JSON captures (collector file
// exporter JSONL, single OTLP JSON documents, optionally gzip or zip
// compressed) into pdata structures. Inputs are untrusted, so every layer of
// reading is bounded by Limits.
package otlpin

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Sample is the merged content of all decoded inputs.
type Sample struct {
	Traces  ptrace.Traces
	Logs    plog.Logs
	Metrics pmetric.Metrics
	Notes   []string // non-fatal decode notes, shown in the report
	// DecompressedBytes is how much was read after decompression, summed
	// over all inputs.
	DecompressedBytes int64
}

// Input is one named byte stream (a file or an uploaded part).
type Input struct {
	Name string
	R    io.Reader
}

// Limits bounds the resources a single Decode call may use.
type Limits struct {
	MaxCompressedBytes   int64 // per call, summed over inputs
	MaxDecompressedBytes int64 // per call, summed over everything decompressed
	MaxItems             int   // spans + span events + log records + data points
	MaxFiles             int   // inputs plus zip members
}

// DefaultLimits are the limits used by the CLI and server.
var DefaultLimits = Limits{MaxCompressedBytes: 30 << 20, MaxDecompressedBytes: 200 << 20, MaxItems: 500_000, MaxFiles: 64}

var (
	ErrTooLarge        = errors.New("input exceeds the size limit")
	ErrTooManyItems    = errors.New("input exceeds the item limit")
	ErrTooManyFiles    = errors.New("input exceeds the file limit")
	ErrConsoleExporter = errors.New("looks like SDK console exporter output, which is not OTLP; capture telemetry with the OpenTelemetry Collector file exporter instead")
	ErrUnrecognized    = errors.New("not recognized as OTLP JSON (expected resourceSpans, resourceLogs or resourceMetrics)")
	ErrEmpty           = errors.New("no telemetry found in input")
)

// maxNesting caps gzip/zip-inside-gzip/zip recursion.
const maxNesting = 4

// Decode reads every input and merges the OTLP content into one Sample.
func Decode(inputs []Input, lim Limits) (*Sample, error) {
	d := &decoder{
		lim: lim,
		s: &Sample{
			Traces:  ptrace.NewTraces(),
			Logs:    plog.NewLogs(),
			Metrics: pmetric.NewMetrics(),
		},
	}
	compressed := &counter{max: lim.MaxCompressedBytes}
	for _, in := range inputs {
		if err := d.file(); err != nil {
			return nil, err
		}
		if err := d.stream(in.Name, &limitReader{r: in.R, c: compressed}, 0); err != nil {
			return nil, err
		}
	}
	if d.objects == 0 {
		return nil, ErrEmpty
	}
	d.s.DecompressedBytes = d.decomp.n
	return d.s, nil
}

type counter struct {
	n, max int64
}

// limitReader fails with ErrTooLarge once the shared counter passes max.
type limitReader struct {
	r io.Reader
	c *counter
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.c.n += int64(n)
	if l.c.n > l.c.max {
		return n, ErrTooLarge
	}
	return n, err
}

type decoder struct {
	lim     Limits
	s       *Sample
	decomp  counter
	files   int
	items   int
	objects int
	init    bool
}

func (d *decoder) file() error {
	d.files++
	if d.files > d.lim.MaxFiles {
		return fmt.Errorf("more than %d files: %w", d.lim.MaxFiles, ErrTooManyFiles)
	}
	return nil
}

func (d *decoder) limited(r io.Reader) io.Reader {
	if !d.init {
		d.decomp.max = d.lim.MaxDecompressedBytes
		d.init = true
	}
	return &limitReader{r: r, c: &d.decomp}
}

// stream sniffs the leading bytes of r and dispatches to gzip, zip or JSON.
func (d *decoder) stream(name string, r io.Reader, depth int) error {
	br := bufio.NewReader(r)
	head, err := br.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", name, err)
	}
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		if depth >= maxNesting {
			return fmt.Errorf("%s: nested too deeply: %w", name, ErrUnrecognized)
		}
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("%s: gzip: %w", name, err)
		}
		return d.stream(name, d.limited(zr), depth+1)
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		if depth >= maxNesting {
			return fmt.Errorf("%s: nested too deeply: %w", name, ErrUnrecognized)
		}
		return d.zip(name, br, depth)
	}
	return d.jsonStream(name, br)
}

func (d *decoder) zip(name string, r io.Reader, depth int) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// archive/zip allocates an entry per central-directory record before we
	// could check the count, so bound the records first.
	if err := d.checkZipEntries(name, data); err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("%s: zip: %w", name, err)
	}
	if len(zr.File) > d.lim.MaxFiles {
		return fmt.Errorf("%s: %d members: %w", name, len(zr.File), ErrTooManyFiles)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if err := d.file(); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: %w", path.Join(name, f.Name), err)
		}
		err = d.stream(path.Join(name, f.Name), d.limited(rc), depth+1)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// checkZipEntries walks the central directory headers (without allocating
// per-entry state) and fails if there are more than MaxFiles of them. The
// EOCD entry count is not trusted, since archive/zip ignores it while reading.
func (d *decoder) checkZipEntries(name string, data []byte) error {
	const eocdLen, cdHeaderLen = 22, 46
	eocd := -1
	for i := len(data) - eocdLen; i >= 0 && i >= len(data)-eocdLen-0xFFFF; i-- {
		if binary.LittleEndian.Uint32(data[i:]) == 0x06054b50 {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return fmt.Errorf("%s: zip: no end of central directory: %w", name, ErrUnrecognized)
	}
	count := binary.LittleEndian.Uint16(data[eocd+10:])
	cdSize := binary.LittleEndian.Uint32(data[eocd+12:])
	cdOff := binary.LittleEndian.Uint32(data[eocd+16:])
	// archive/zip treats a directory size of 0xFFFF like the other zip64 markers.
	if count == 0xFFFF || cdSize == 0xFFFF || cdSize == 0xFFFFFFFF || cdOff == 0xFFFFFFFF {
		return fmt.Errorf("%s: zip64 archives are not supported: %w", name, ErrUnrecognized)
	}
	// Require a self-consistent EOCD (directory ends exactly at the EOCD, no
	// prepended data). Then archive/zip's base offset is 0 and it starts
	// reading at cdOff, exactly where we walk. Anything else is ambiguous.
	if uint64(cdOff)+uint64(cdSize) != uint64(eocd) {
		return fmt.Errorf("%s: zip: inconsistent central directory (prepended data or forged header): %w", name, ErrUnrecognized)
	}
	pos := int(cdOff)
	for n := 0; pos+cdHeaderLen <= len(data) && binary.LittleEndian.Uint32(data[pos:]) == 0x02014b50; n++ {
		if n >= d.lim.MaxFiles {
			return fmt.Errorf("%s: more than %d members: %w", name, d.lim.MaxFiles, ErrTooManyFiles)
		}
		pos += cdHeaderLen +
			int(binary.LittleEndian.Uint16(data[pos+28:])) +
			int(binary.LittleEndian.Uint16(data[pos+30:])) +
			int(binary.LittleEndian.Uint16(data[pos+32:]))
	}
	return nil
}
