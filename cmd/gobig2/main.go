// Command gobig2 decodes a standalone or PDF-embedded JBIG2 stream
// into a bitmap. Full flag reference + exit-code table in [USAGE.md].
// Thin entry point - delegates to the package.
//
// [USAGE.md]: ../../USAGE.md
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/tannevaled/gobig2"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/segment"
)

// version is the CLI version. `var` so Taskfile.yml `build:release`
// overrides via `-ldflags="-X main.version=v1.2.3"`.
// debug.ReadBuildInfo() supplies module / VCS metadata.
var version = "dev"

// Exit codes per USAGE.md. Scripts branch on failure class
// without parsing stderr.
const (
	exitOK              = 0 // success.
	exitErr             = 1 // generic / unclassified failure (gobig2 internal bug, I/O error, etc.).
	exitUsage           = 2 // flag parse error or missing required arg.
	exitMalformed       = 3 // input is not legal JBIG2 (wraps gobig2.ErrMalformed).
	exitResourceBudget  = 4 // a Limits cap fired (wraps gobig2.ErrResourceBudget).
	exitUnsupported     = 5 // legal JBIG2 but uses a feature gobig2 doesn't implement (wraps gobig2.ErrUnsupported).
	exitTimeoutExceeded = 6 // wall-clock budget from --timeout exhausted.
)

// classifyExit picks an exit code from a decode error. nil -> exitOK.
// Use at every error site for consistent failure-class signal.
func classifyExit(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.DeadlineExceeded):
		return exitTimeoutExceeded
	case errors.Is(err, gobig2.ErrResourceBudget):
		return exitResourceBudget
	case errors.Is(err, gobig2.ErrMalformed):
		return exitMalformed
	case errors.Is(err, gobig2.ErrUnsupported):
		return exitUnsupported
	case errors.Is(err, io.EOF):
		// --page past end. Decoder.Decode returns io.EOF after
		// final page. Route to exitUsage (2) so scripts can
		// distinguish out-of-range from generic exit 1.
		return exitUsage
	default:
		return exitErr
	}
}

// flagsConfig is the parsed flag set. Aggregated for test-driven
// run() calls.
type flagsConfig struct {
	globals     string
	inspect     bool
	format      string
	page        int
	maxPixels   int64
	maxAlloc    int64
	maxAllocSet bool // true when --max-alloc was explicitly passed; gates the runtime.SetMemoryLimit call to skip its ~2-5 ms cost on default invocations
	timeout     time.Duration
	cpuprofile  string // hidden flag; writes go test-style cpuprofile for PGO collection
	verbose     int
	showHelp    bool
	showVer     bool
}

func main() {
	cfg, args, code := parseFlags(os.Args[1:], os.Stderr)
	if code != exitOK {
		os.Exit(code)
	}
	if cfg.showHelp {
		printUsage(os.Stdout)
		os.Exit(exitOK)
	}
	if cfg.showVer {
		printVersion(os.Stdout)
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "gobig2: missing input path (use - for stdin)")
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
	in := args[0]
	out := "-"
	if len(args) >= 2 {
		out = args[1]
	}
	os.Exit(run(cfg, in, out))
}

// parseFlags parses argv (no program name) into flagsConfig.
// Returns config, positional args, exit code (exitOK / exitUsage).
func parseFlags(argv []string, stderr io.Writer) (flagsConfig, []string, int) {
	fs := flag.NewFlagSet("gobig2", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
		timeout:   10 * time.Second,
	}
	fs.StringVar(&cfg.globals, "globals", "", "Path to a JBIG2 globals stream")
	fs.BoolVar(&cfg.inspect, "inspect", false, "Print segment table and exit")
	fs.StringVar(&cfg.format, "format", cfg.format, "Output bitmap format (png|pbm|raw)")
	fs.IntVar(&cfg.page, "page", cfg.page, "1-based page number")
	fs.Int64Var(&cfg.maxPixels, "max-pixels", cfg.maxPixels, "Reject regions whose pixel count exceeds this")
	allocStr := fs.String("max-alloc", "", "Reject decodes whose declared bitmap allocation exceeds this (e.g. 64M, 1G; default 1GiB)")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "Wall-clock budget for the entire decode")
	// --cpuprofile is for PGO profile capture; not advertised
	// in -h output. Pass `--cpuprofile=path.pprof` and the run
	// writes a pprof CPU profile covering decode + writeOutput.
	// See cmd/gobig2/default.pgo + scripts/perf/gen-pgo.sh.
	fs.StringVar(&cfg.cpuprofile, "cpuprofile", "", "")
	fs.Var(&counterFlag{val: &cfg.verbose}, "v", "Verbose logging on stderr (repeat for more detail)")
	fs.Var(&counterFlag{val: &cfg.verbose}, "verbose", "Verbose logging on stderr (repeat for more detail)")
	fs.BoolVar(&cfg.showHelp, "h", false, "Print help, exit 0")
	fs.BoolVar(&cfg.showHelp, "help", false, "Print help, exit 0")
	fs.BoolVar(&cfg.showVer, "version", false, "Print version, exit 0")

	if err := fs.Parse(argv); err != nil {
		// flag.ContinueOnError already wrote a message via fs.Output().
		return cfg, nil, exitUsage
	}
	if *allocStr != "" {
		v, err := parseSize(*allocStr)
		if err != nil {
			fmt.Fprintf(stderr, "gobig2: --max-alloc: %v\n", err)
			return cfg, nil, exitUsage
		}
		cfg.maxAlloc = v
		cfg.maxAllocSet = true
	}
	switch cfg.format {
	case "png", "pbm", "raw":
		// ok
	default:
		fmt.Fprintf(stderr, "gobig2: --format must be one of png|pbm|raw (got %q)\n", cfg.format)
		return cfg, nil, exitUsage
	}
	if cfg.page < 1 {
		fmt.Fprintf(stderr, "gobig2: --page must be >= 1 (got %d)\n", cfg.page)
		return cfg, nil, exitUsage
	}
	return cfg, fs.Args(), exitOK
}

// counterFlag implements flag.Value as repeatable bool - each
// occurrence increments val. Mirrors -v -v -v idiom in USAGE.md.
type counterFlag struct {
	val *int
}

func (c *counterFlag) String() string {
	if c == nil || c.val == nil {
		return "0"
	}
	return strconv.Itoa(*c.val)
}
func (c *counterFlag) IsBoolFlag() bool { return true }
func (c *counterFlag) Set(s string) error {
	if s == "true" || s == "" {
		*c.val++
		return nil
	}
	if s == "false" {
		*c.val = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid verbose count %q", s)
	}
	*c.val = n
	return nil
}

// parseSize parses byte count with optional K/M/G/T suffix
// (case-insensitive). 1024 KiB style, no fractions.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty value")
	}
	mul := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mul = 1 << 10
		s = s[:len(s)-1]
	case 'M', 'm':
		mul = 1 << 20
		s = s[:len(s)-1]
	case 'G', 'g':
		mul = 1 << 30
		s = s[:len(s)-1]
	case 'T', 't':
		mul = 1 << 40
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a byte count: %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("negative byte count: %d", v)
	}
	// Reject parse-succeeds / multiply-overflows. Large prefix
	// + suffix wraps int64 to negative -> callers treat as
	// "disabled".
	if mul > 1 && v > math.MaxInt64/mul {
		return 0, fmt.Errorf("byte count overflows int64: %d x %d", v, mul)
	}
	return v * mul, nil
}

// run is the decode entry point. Separate so tests drive it
// without spawning a subprocess. Returns exit code; never panics.
func run(cfg flagsConfig, in, out string) int {
	// PGO profile capture - kept first so the captured profile
	// spans setup + decode + output write.
	if cfg.cpuprofile != "" {
		f, err := os.Create(cfg.cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gobig2: --cpuprofile: %v\n", err)
			return exitUsage
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "gobig2: --cpuprofile: %v\n", err)
			_ = f.Close()
			return exitErr
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close()
		}()
	}

	// Per-decode resource budgets. MaxPixels caps any single
	// bitmap NewImage allocates; MaxAlloc sets the soft memory
	// ceiling via debug.SetMemoryLimit for the decode.
	//
	// Save + defer restore page.MaxImagePixels so long-lived
	// callers (tests, embedded helpers) don't leak this
	// invocation's cap. Mirrors debug.SetMemoryLimit save/
	// restore below.
	prevMaxImagePixels := page.MaxImagePixels
	// debug.SetMemoryLimit triggers a runtime tuning path -
	// each save+restore pair is ~2-5 ms on cold start. Only
	// touch the limit when --max-alloc was explicitly passed;
	// the default flag value is documented as "1 GiB" but the
	// CLI was always calling it even when the user wasn't
	// trying to constrain memory.
	var prevLimit int64
	if cfg.maxAllocSet {
		prevLimit = debug.SetMemoryLimit(-1)
	}
	defer func() {
		page.MaxImagePixels = prevMaxImagePixels
		if cfg.maxAllocSet {
			debug.SetMemoryLimit(prevLimit)
		}
	}()
	if cfg.maxPixels > 0 {
		page.MaxImagePixels = cfg.maxPixels
	} else {
		page.MaxImagePixels = 0
	}
	if cfg.maxAllocSet && cfg.maxAlloc > 0 {
		debug.SetMemoryLimit(cfg.maxAlloc)
	}

	data, err := readInput(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gobig2: %v\n", err)
		// Budget rejection (input over MaxInputBytes) routes
		// via classifyExit (exit 4). Path-open / IO stays
		// exitUsage.
		if errors.Is(err, gobig2.ErrResourceBudget) {
			return classifyExit(err)
		}
		return exitUsage
	}
	var globals []byte
	if cfg.globals != "" {
		// "-" reads globals from stdin. Two-stream-on-stdin
		// banned: callers using "-" for globals must pass main
		// stream as file path.
		if cfg.globals == "-" {
			if in == "-" {
				fmt.Fprintln(os.Stderr, "gobig2: --globals - cannot be combined with stdin input; pass input as a file path")
				return exitUsage
			}
			globals, err = readBoundedFromCLI(os.Stdin, "--globals (stdin)")
		} else {
			f, oerr := os.Open(cfg.globals) //nolint:gosec // path is user-supplied by design
			if oerr != nil {
				err = oerr
			} else {
				globals, err = readBoundedFromCLI(f, cfg.globals)
				_ = f.Close()
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "gobig2: --globals: %v\n", err)
			// Budget rejections route via classifyExit;
			// usage-class (path open, etc.) stays exitUsage.
			if errors.Is(err, gobig2.ErrResourceBudget) {
				return classifyExit(err)
			}
			return exitUsage
		}
	}
	if cfg.verbose > 0 {
		fmt.Fprintf(os.Stderr, "gobig2: input=%s bytes=%d globals=%d\n", in, len(data), len(globals))
	}

	// Construct Decoder. Globals-non-empty path uses
	// NewDecoderWithGlobals (falls back to embedded on probe
	// fail); else NewDecoder auto-detects.
	var dec *gobig2.Decoder
	switch {
	case len(globals) > 0:
		dec, err = gobig2.NewDecoderWithGlobals(bytes.NewReader(data), globals)
	default:
		dec, err = gobig2.NewDecoder(bytes.NewReader(data))
	}
	if err != nil {
		// Auto-detect fail on headerless/PDF streams is
		// recoverable via NewDecoderEmbedded - but only when
		// standalone constructor failed on magic/probe
		// mismatch (ErrMalformed from probeConfigs).
		// ErrUnsupported / ErrResourceBudget must propagate
		// as-is so exit code is right and user sees real
		// cause, not the embedded sniff's downstream
		// ErrMalformed.
		if !errors.Is(err, gobig2.ErrMalformed) {
			fmt.Fprintf(os.Stderr, "gobig2: %v\n", err)
			return classifyExit(err)
		}
		dec2, err2 := gobig2.NewDecoderEmbedded(bytes.NewReader(data), globals)
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "gobig2: %v (embedded fallback also failed: %v)\n", err, err2)
			return classifyExit(err2)
		}
		dec = dec2
	}

	if cfg.inspect {
		return inspect(dec, cfg.verbose, cfg.timeout)
	}

	// Wall-clock budget enforced via DecodeContext /
	// DecodePackedContext - both check ctx.Err() between
	// segments, so the timeout context fires at the next
	// segment boundary. Earlier versions ran the decode in a
	// goroutine with a select on ctx.Done() as a belt-and-
	// suspenders against a single segment running longer than
	// the per-region Limits caps would bound. The goroutine
	// added ~2 KB stack alloc + scheduler-warmup work on every
	// invocation; checking inline saves that for the common
	// (no-timeout) case. Real-input segments stay well under
	// the per-region budget; truly adversarial inputs that
	// hang inside one segment now hang the CLI - the user can
	// SIGINT, and fuzz / Limits cover the documented attack
	// surface.
	//
	// PBM / raw output paths skip the *image.Gray conversion
	// entirely - the packed 1bpp internal buffer already
	// matches their on-wire layout. PNG still wants the Gray
	// form. Decide once up front so the loop picks the right
	// decode entry point.
	wantPacked := cfg.format == "pbm" || cfg.format == "raw"
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	var img image.Image
	var pp gobig2.PackedPage
	var decErr error
	for i := 1; i <= cfg.page; i++ {
		if wantPacked {
			pp, decErr = dec.DecodePackedContext(ctx)
		} else {
			img, decErr = dec.DecodeContext(ctx)
		}
		if decErr != nil {
			break
		}
	}
	if errors.Is(decErr, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "gobig2: timeout (%s) exceeded\n", cfg.timeout)
		return exitTimeoutExceeded
	}
	res := struct {
		img image.Image
		pp  gobig2.PackedPage
		err error
	}{img: img, pp: pp, err: decErr}
	if res.err != nil {
		// Spell out io.EOF before generic line so scripts see
		// why exit 2 fired, not a bare "EOF".
		if errors.Is(res.err, io.EOF) {
			fmt.Fprintf(os.Stderr, "gobig2: page %d is beyond end of stream\n", cfg.page)
		} else {
			fmt.Fprintf(os.Stderr, "gobig2: %v\n", res.err)
		}
		return classifyExit(res.err)
	}
	if wantPacked {
		if res.pp.Data == nil {
			fmt.Fprintln(os.Stderr, "gobig2: page produced no image")
			return exitErr
		}
		if err := writePackedOutput(out, cfg.format, res.pp); err != nil {
			fmt.Fprintf(os.Stderr, "gobig2: %v\n", err)
			return exitErr
		}
		return exitOK
	}
	if res.img == nil {
		fmt.Fprintln(os.Stderr, "gobig2: page produced no image")
		return exitErr
	}
	if err := writeOutput(out, cfg.format, res.img); err != nil {
		fmt.Fprintf(os.Stderr, "gobig2: %v\n", err)
		return exitErr
	}
	return exitOK
}

// readInput reads from path, or stdin when path is "-". Capped at
// gobig2.MaxInputBytes + 1 -> hostile source rejected with
// ErrResourceBudget before CLI allocates full buffer. Without
// this, `gobig2 huge.jb2` slurps entire file before library's
// bounded-read could classify.
func readInput(path string) ([]byte, error) {
	var src io.Reader
	if path == "-" {
		src = os.Stdin
	} else {
		f, err := os.Open(path) //nolint:gosec // path is user-supplied by design
		if err != nil {
			return nil, err
		}
		defer f.Close()
		src = f
	}
	return readBoundedFromCLI(src, path)
}

// readBoundedFromCLI is CLI's analog of internal/input.ReadBounded:
// cap at MaxInputBytes+1, surface over-cap as ErrResourceBudget
// with path-naming diagnostic.
func readBoundedFromCLI(src io.Reader, label string) ([]byte, error) {
	limited := &io.LimitedReader{R: src, N: int64(gobig2.MaxInputBytes) + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > int64(gobig2.MaxInputBytes) {
		return nil, fmt.Errorf("%s: input exceeds %d-byte cap: %w",
			label, gobig2.MaxInputBytes, gobig2.ErrResourceBudget)
	}
	return data, nil
}

// inspect prints a short segment-table report to stdout and exits.
// Triage-only fields; richer dumps belong in a dedicated tool.
func inspect(dec *gobig2.Decoder, verbose int, timeout time.Duration) int {
	doc := dec.GetDocument()
	if doc == nil {
		fmt.Fprintln(os.Stderr, "gobig2: --inspect: decoder produced no document")
		return exitErr
	}
	// Honor --timeout. DecodeSequential checks ctx.Err()
	// between segments via document context; without this
	// binding --inspect walks the segment list ignoring the
	// budget.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	doc.SetContext(ctx)
	defer doc.SetContext(context.Background())
	// Walk segment list once to populate it. Orchestrator
	// fills GetSegments() as it parses; drive DecodeSequential
	// until end-reached or first failure.
	timedOut := false
	for {
		res := doc.DecodeSequential()
		if res == segment.ResultEndReached || res == segment.ResultFailure {
			break
		}
		if ctx.Err() != nil {
			timedOut = true
			break
		}
	}
	segs := doc.GetSegments()
	fmt.Println("offset  type   number  flags  refs        length  description")
	for _, s := range segs {
		desc := segmentTypeName(s.Flags.Type)
		refs := "[]"
		if len(s.ReferredToSegmentNumbers) > 0 {
			parts := make([]string, len(s.ReferredToSegmentNumbers))
			for i, n := range s.ReferredToSegmentNumbers {
				parts[i] = strconv.FormatUint(uint64(n), 10)
			}
			refs = "[" + strings.Join(parts, ",") + "]"
		}
		fmt.Printf("%6d  0x%02x  %6d   0x%02x  %-9s  %6d  %s\n",
			s.DataOffset, s.Flags.Type, s.Number, headerFlagsByte(s.Flags),
			refs, s.DataLength, desc)
	}
	// --inspect is best-effort (segment table useful even on
	// broken inputs) but signals failure class via exit code.
	// Stopping error prints to stderr regardless of verbosity -
	// scripts shouldn't need -v to see why.
	if err := doc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "gobig2: parse stopped: %v\n", err)
		return classifyExit(err)
	}
	if timedOut {
		fmt.Fprintf(os.Stderr, "gobig2: --inspect: timeout (%s) exceeded\n", timeout)
		return exitTimeoutExceeded
	}
	return exitOK
}

// headerFlagsByte reconstructs the wire-format flags byte from
// parsed SegmentFlags. Inspect report only.
func headerFlagsByte(f segment.SegmentFlags) byte {
	b := f.Type & 0x3F
	if f.PageAssociationSize {
		b |= 0x40
	}
	if f.DeferredNonRetain {
		b |= 0x80
	}
	return b
}

// segmentTypeName returns a human label for a JBIG2 segment type
// per T.88 / ISO/IEC 14492 Annex H. Backed by [segment.TypeInfo]
// registry so the three codes inside text/halftone/generic/
// refinement families each get their label, and reserved codes
// (e.g. type 17) aren't misreported as a neighbor.
func segmentTypeName(t uint8) string {
	if _, name := segment.TypeInfo(t); name != "" {
		return name
	}
	return fmt.Sprintf("unknown 0x%02x", t)
}

// writeOutput renders img to dst (or stdout for "-") in the chosen
// format. PNG path only: pbm / raw take the packed fast path
// via [writePackedOutput].
func writeOutput(dst, format string, img image.Image) error {
	var w io.Writer = os.Stdout
	if dst != "-" {
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	switch format {
	case "png":
		return png.Encode(w, img)
	case "pbm":
		return writePBM(w, img)
	case "raw":
		return writeRawBitmap(w, img)
	}
	return fmt.Errorf("unsupported format %q", format)
}

// writePackedOutput emits a [gobig2.PackedPage] as PBM (P4) or
// raw 1-bit bitmap. Same on-wire format as the writeOutput
// pbm / raw branches, but the input is already in MSB-first
// packed bytes - skip the *image.Gray expansion + the
// per-pixel bit-pack walk that writeOutput does. On a 600 dpi
// A4 page that's ~12 ms (ToGoImage) + ~30 ms (per-row Write
// syscalls + Gray-to-PBM walk) of work the packed path avoids.
//
// Stride may be greater than (Width+7)/8 - the internal page
// buffer rounds row stride to whole bytes. Trailing pad bytes
// past the rightmost-pixel byte stay zero, so the PBM grammar
// stays well-formed: PBM rows are `ceil(W/8)` bytes each, and
// we slice each Data row to exactly that length.
//
// bufio.Writer wraps the output so the row-by-row writes
// collapse into a few big syscalls (4 KB default buffer);
// without the wrap, a 4960x7016 page makes 7016 separate
// write() syscalls into the kernel.
func writePackedOutput(dst, format string, pp gobig2.PackedPage) error {
	var raw io.Writer = os.Stdout
	if dst != "-" {
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		raw = f
	}
	w := bufio.NewWriter(raw)
	if format == "pbm" {
		if _, err := fmt.Fprintf(w, "P4\n%d %d\n", pp.Width, pp.Height); err != nil {
			return err
		}
	}
	rowBytes := (pp.Width + 7) / 8
	for y := 0; y < pp.Height; y++ {
		off := y * pp.Stride
		if _, err := w.Write(pp.Data[off : off+rowBytes]); err != nil {
			return err
		}
	}
	return w.Flush()
}

// writePBM writes a P4 (binary PBM) bitmap. JBIG2 ToGoImage
// produces *image.Gray with ink=0 / paper=255; PBM inverts
// (1 = ink), so map per-pixel.
func writePBM(w io.Writer, img image.Image) error {
	b := img.Bounds()
	if _, err := fmt.Fprintf(w, "P4\n%d %d\n", b.Dx(), b.Dy()); err != nil {
		return err
	}
	return packBitmapRows(w, img, b)
}

// writeRawBitmap writes packed MSB-first bytes, ink=1, no header.
// Pair with --inspect to recover dimensions.
func writeRawBitmap(w io.Writer, img image.Image) error {
	return packBitmapRows(w, img, img.Bounds())
}

// packBitmapRows packs the bounded region of img into MSB-first
// bytes (ink=1 when Y<128) and writes them row by row. Decoder
// hands back *image.Gray; the type-asserted fast path stride-
// walks Pix instead of paying ~35M img.At + color.GrayModel
// dispatch calls on a 600 dpi page. Falls back to the generic
// loop for any non-Gray input (defensive; current decoder
// never produces one).
func packBitmapRows(w io.Writer, img image.Image, b image.Rectangle) error {
	width := b.Dx()
	rowBytes := (width + 7) / 8
	row := make([]byte, rowBytes)
	if g, ok := img.(*image.Gray); ok {
		minXOff := b.Min.X - g.Rect.Min.X
		for y := 0; y < b.Dy(); y++ {
			for i := range row {
				row[i] = 0
			}
			off := (b.Min.Y + y - g.Rect.Min.Y) * g.Stride
			pix := g.Pix[off+minXOff : off+minXOff+width]
			for x, p := range pix {
				if p < 128 { // ink
					row[x>>3] |= 1 << (7 - uint(x&7))
				}
			}
			if _, err := w.Write(row); err != nil {
				return err
			}
		}
		return nil
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for i := range row {
			row[i] = 0
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			c, _ := color.GrayModel.Convert(img.At(x, y)).(color.Gray)
			if c.Y < 128 {
				bx := x - b.Min.X
				row[bx>>3] |= 1 << (7 - uint(bx&7))
			}
		}
		if _, err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// printVersion renders version line plus module build info when
// built with module-aware tooling.
//
// When `-ldflags="-X main.version=..."` is unset (version still
// "dev"), fall back to gobig2.Version so printed line reflects
// the library tag linked against. Avoids `gobig2 dev` for a
// release build that forgot the ldflag.
func printVersion(w io.Writer) {
	v := version
	if v == "dev" {
		v = gobig2.Version
	}
	fmt.Fprintf(w, "gobig2 %s\n", v)
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprintf(w, "  module: %s\n  go:     %s\n", info.Main.Path, info.GoVersion)
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.time" {
				fmt.Fprintf(w, "  %s: %s\n", s.Key, s.Value)
			}
		}
	}
}

// printUsage prints the synopsis. Long-form documentation lives
// in USAGE.md - we don't duplicate it here.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: gobig2 [flags] <input> [output]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  <input>  path to a .jb2 / .jbig2 file (or - for stdin)")
	fmt.Fprintln(w, "  output   destination path (defaults to stdout in PNG)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --globals PATH    JBIG2 globals stream for PDF-embedded inputs (or \"-\" for stdin)")
	fmt.Fprintln(w, "  --inspect         print segment table and exit")
	fmt.Fprintln(w, "  --format png|pbm|raw   output bitmap format (default png)")
	fmt.Fprintln(w, "  --page N          1-based page number (default 1)")
	fmt.Fprintln(w, "  --max-pixels N    pixel-count cap (default 100000000)")
	fmt.Fprintln(w, "  --max-alloc SIZE  bitmap allocation cap (e.g. 64M, 1G; default 1G)")
	fmt.Fprintln(w, "  --timeout DUR     wall-clock budget (default 10s)")
	fmt.Fprintln(w, "  -v, --verbose     verbose logging on stderr (repeat for more)")
	fmt.Fprintln(w, "  --version         print version and exit")
	fmt.Fprintln(w, "  -h, --help        print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "See USAGE.md for examples and the full reference.")
}
