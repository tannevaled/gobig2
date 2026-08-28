package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tannevaled/gobig2"
	"github.com/tannevaled/gobig2/internal/page"
)

// fixtureSamplePath points at the committed PDF-extracted JBIG2
// fixture used end-to-end by the parent package. CLI auto-detects
// (NewDecoder fails -> NewDecoderEmbedded), exercising fallback.
const fixtureSamplePath = "../../testdata/pdf-embedded/sample.jb2"

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantCode   int
		wantArgs   []string
		check      func(t *testing.T, cfg flagsConfig)
		wantStderr string
	}{
		{
			name:     "defaults",
			argv:     []string{"in.jb2"},
			wantCode: exitOK,
			wantArgs: []string{"in.jb2"},
			check: func(t *testing.T, cfg flagsConfig) {
				if cfg.format != "png" {
					t.Errorf("format = %q, want png", cfg.format)
				}
				if cfg.page != 1 {
					t.Errorf("page = %d, want 1", cfg.page)
				}
				if cfg.maxAlloc != 1<<30 {
					t.Errorf("maxAlloc = %d, want %d", cfg.maxAlloc, 1<<30)
				}
			},
		},
		{
			name:     "format pbm",
			argv:     []string{"--format=pbm", "in.jb2", "out.pbm"},
			wantCode: exitOK,
			wantArgs: []string{"in.jb2", "out.pbm"},
			check: func(t *testing.T, cfg flagsConfig) {
				if cfg.format != "pbm" {
					t.Errorf("format = %q, want pbm", cfg.format)
				}
			},
		},
		{
			name:     "verbose repeat",
			argv:     []string{"-v", "-v", "-v", "in.jb2"},
			wantCode: exitOK,
			wantArgs: []string{"in.jb2"},
			check: func(t *testing.T, cfg flagsConfig) {
				if cfg.verbose != 3 {
					t.Errorf("verbose = %d, want 3", cfg.verbose)
				}
			},
		},
		{
			name:     "max-alloc with suffix",
			argv:     []string{"--max-alloc=64M", "in.jb2"},
			wantCode: exitOK,
			wantArgs: []string{"in.jb2"},
			check: func(t *testing.T, cfg flagsConfig) {
				if cfg.maxAlloc != 64<<20 {
					t.Errorf("maxAlloc = %d, want %d", cfg.maxAlloc, 64<<20)
				}
			},
		},
		{
			name:       "bad format",
			argv:       []string{"--format=xyz", "in.jb2"},
			wantCode:   exitUsage,
			wantStderr: "--format must be one of",
		},
		{
			name:       "bad page",
			argv:       []string{"--page=0", "in.jb2"},
			wantCode:   exitUsage,
			wantStderr: "--page must be >= 1",
		},
		{
			name:       "bad max-alloc",
			argv:       []string{"--max-alloc=abcM", "in.jb2"},
			wantCode:   exitUsage,
			wantStderr: "--max-alloc:",
		},
		{
			name:       "unknown flag",
			argv:       []string{"--bogus"},
			wantCode:   exitUsage,
			wantStderr: "flag provided but not defined",
		},
		{
			name:     "help",
			argv:     []string{"--help"},
			wantCode: exitOK,
			check: func(t *testing.T, cfg flagsConfig) {
				if !cfg.showHelp {
					t.Error("showHelp not set")
				}
			},
		},
		{
			name:     "version",
			argv:     []string{"--version"},
			wantCode: exitOK,
			check: func(t *testing.T, cfg flagsConfig) {
				if !cfg.showVer {
					t.Error("showVer not set")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			cfg, args, code := parseFlags(tc.argv, &stderr)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d (stderr=%q)", code, tc.wantCode, stderr.String())
			}
			if tc.wantArgs != nil {
				if len(args) != len(tc.wantArgs) {
					t.Errorf("args = %v, want %v", args, tc.wantArgs)
				} else {
					for i, a := range tc.wantArgs {
						if args[i] != a {
							t.Errorf("args[%d] = %q, want %q", i, args[i], a)
						}
					}
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.wantStderr)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1024", 1024, true},
		{"1K", 1 << 10, true},
		{"4k", 4 << 10, true},
		{"64M", 64 << 20, true},
		{"2G", 2 << 30, true},
		{"1T", 1 << 40, true},
		{"", 0, false},
		{"abc", 0, false},
		{"12X", 0, false}, // unknown suffix -> ParseInt failure
		// Large x multiplier must not wrap int64 negative
		// (callers treat negative as "disabled").
		// 1 << 24 x 1 << 40 (T) > MaxInt64.
		{"16777216T", 0, false},
		// 1 << 53 x 1 << 10 (K) > MaxInt64.
		{"9007199254740992K", 0, false},
		// Negative prefix rejected - else parseSize returns
		// negative count callers treat as "disabled".
		{"-1G", 0, false},
	}
	for _, tc := range tests {
		got, err := parseSize(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("parseSize(%q) ok = %v, want %v (err=%v)", tc.in, err == nil, tc.ok, err)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCounterFlag(t *testing.T) {
	var n int
	c := &counterFlag{val: &n}
	if c.String() != "0" {
		t.Errorf("zero String = %q", c.String())
	}
	// Repeated Set("true") should increment.
	for i := 0; i < 3; i++ {
		if err := c.Set("true"); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if n != 3 {
		t.Errorf("after 3 sets n = %d, want 3", n)
	}
	if err := c.Set("false"); err != nil {
		t.Fatalf("Set false: %v", err)
	}
	if n != 0 {
		t.Errorf("after Set false n = %d, want 0", n)
	}
	// Numeric form.
	if err := c.Set("5"); err != nil {
		t.Fatalf("Set numeric: %v", err)
	}
	if n != 5 {
		t.Errorf("after Set 5 n = %d, want 5", n)
	}
	// Invalid form.
	if err := c.Set("garbage"); err == nil {
		t.Error("Set garbage accepted")
	}
	// Nil-safe String.
	var nilC *counterFlag
	if nilC.String() != "0" {
		t.Errorf("nil String = %q", nilC.String())
	}
	// IsBoolFlag.
	if !c.IsBoolFlag() {
		t.Error("IsBoolFlag = false")
	}
}

// pdfEmbeddedSampleSHA is packed-bits SHA-256 of the committed
// sample fixture (verified by root package's
// TestPDFEmbeddedSampleFixture; duplicated as const so CLI tests
// pin the invariant without a cross-package dep).
const (
	pdfEmbeddedSampleSHA    = "e44aad16c4c8385d0b0f7f624ba7660a5db7055ff4ae10a07e9e71e04f422695"
	pdfEmbeddedSampleWidth  = 3562
	pdfEmbeddedSampleHeight = 851
)

func TestRunDecodeToPNG(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.png")
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
		timeout:   30_000_000_000, // 30 s
	}
	if code := run(cfg, fixtureSamplePath, out); code != exitOK {
		t.Fatalf("run code = %d, want %d", code, exitOK)
	}
	// Decode PNG; confirm dims + packed-bits hash match
	// committed invariant. Without hash check, one-bit
	// inversion, x/y loop bug, or PNG color-model wreck pass
	// bare "output > 0 bytes".
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	if got, want := img.Bounds().Dx(), pdfEmbeddedSampleWidth; got != want {
		t.Errorf("PNG width = %d, want %d", got, want)
	}
	if got, want := img.Bounds().Dy(), pdfEmbeddedSampleHeight; got != want {
		t.Errorf("PNG height = %d, want %d", got, want)
	}
	gotSHA := sha256OfImageBits(img)
	if gotSHA != pdfEmbeddedSampleSHA {
		t.Errorf("PNG packed-bits hash = %s, want %s", gotSHA, pdfEmbeddedSampleSHA)
	}
}

func TestRunDecodeToPBM(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.pbm")
	cfg := flagsConfig{
		format:    "pbm",
		page:      1,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
		timeout:   30_000_000_000,
	}
	if code := run(cfg, fixtureSamplePath, out); code != exitOK {
		t.Fatalf("run code = %d, want %d", code, exitOK)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.HasPrefix(got, []byte("P4\n")) {
		t.Errorf("PBM output missing P4 magic: prefix = %q", got[:min(8, len(got))])
	}
	// Parse PBM header (P4 / W H / data); assert raw packed
	// bits match committed sha256. PBM stores MSB-first per
	// byte, 0=white, 1=ink - same polarity sha256OfImageBits
	// uses.
	header := bytes.SplitN(got, []byte{'\n'}, 3)
	if len(header) != 3 {
		t.Fatalf("PBM has fewer than 3 newline-separated header parts")
	}
	dims := strings.Fields(string(header[1]))
	if len(dims) != 2 {
		t.Fatalf("PBM dims line = %q, want 'W H'", header[1])
	}
	wantStride := (pdfEmbeddedSampleWidth + 7) / 8
	wantBody := wantStride * pdfEmbeddedSampleHeight
	body := header[2]
	if len(body) != wantBody {
		t.Fatalf("PBM body size = %d, want %d (stride %d x height %d)",
			len(body), wantBody, wantStride, pdfEmbeddedSampleHeight)
	}
	h := sha256.New()
	h.Write(body)
	gotSHA := hex.EncodeToString(h.Sum(nil))
	if gotSHA != pdfEmbeddedSampleSHA {
		t.Errorf("PBM body hash = %s, want %s", gotSHA, pdfEmbeddedSampleSHA)
	}
}

func TestRunDecodeToRaw(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.bits")
	cfg := flagsConfig{
		format:    "raw",
		page:      1,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
		timeout:   30_000_000_000,
	}
	if code := run(cfg, fixtureSamplePath, out); code != exitOK {
		t.Fatalf("run code = %d, want %d", code, exitOK)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	// Raw bitmap: stride = ceil(3562/8) = 446; height = 851.
	wantSize := int64(446 * 851)
	if info.Size() != wantSize {
		t.Errorf("raw size = %d, want %d", info.Size(), wantSize)
	}
	// Verify polarity + packing match committed hash. Raw is
	// same MSB-first packed bytes as PBM body, no header.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read raw output: %v", err)
	}
	h := sha256.New()
	h.Write(raw)
	if gotSHA := hex.EncodeToString(h.Sum(nil)); gotSHA != pdfEmbeddedSampleSHA {
		t.Errorf("raw hash = %s, want %s", gotSHA, pdfEmbeddedSampleSHA)
	}
}

// sha256OfImageBits packs img MSB-first into stride-rounded
// 1-bit buffer and hashes. Mirrors root package's
// sha256OfGrayBits without the *image.Gray assertion - PNG
// decoder may return NRGBA or Paletted; we only care about
// bi-level rendering.
func sha256OfImageBits(img image.Image) string {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	stride := (w + 7) / 8
	row := make([]byte, stride)
	hasher := sha256.New()
	for y := 0; y < h; y++ {
		for i := range row {
			row[i] = 0
		}
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// 1-bit threshold: non-white = ink. RGBA returns
			// 16-bit per channel; under half-bright = ink.
			lum := (r + g + b) / 3
			if lum < 0x8000 {
				row[x>>3] |= 1 << (7 - uint(x&7))
			}
		}
		hasher.Write(row)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func TestRunMissingInput(t *testing.T) {
	cfg := flagsConfig{
		format: "png", page: 1, maxPixels: 100_000_000,
		maxAlloc: 1 << 30, timeout: 30_000_000_000,
	}
	if code := run(cfg, "/nonexistent.jb2", "/dev/null"); code != exitUsage {
		t.Errorf("run code = %d, want %d (exitUsage)", code, exitUsage)
	}
}

func TestRunMissingGlobals(t *testing.T) {
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		globals:   "/nonexistent.bin",
		timeout:   30_000_000_000,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
	}
	if code := run(cfg, fixtureSamplePath, "/dev/null"); code != exitUsage {
		t.Errorf("run code = %d, want %d (exitUsage)", code, exitUsage)
	}
}

// TestRunWithGlobalsSuccess exercises --globals decode against
// bitmap-p32-eof PDF-embedded fixture carrying a real
// /JBIG2Globals reference. Complements missing-globals error
// path with real "image + globals sibling" workflow.
func TestRunWithGlobalsSuccess(t *testing.T) {
	const (
		imgPath     = "../../testdata/pdf-embedded/serenityos/bitmap-p32-eof-obj6.jb2"
		globalsPath = "../../testdata/pdf-embedded/serenityos/bitmap-p32-eof-obj6.globals.jb2"
	)
	if _, err := os.Stat(imgPath); err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	if _, err := os.Stat(globalsPath); err != nil {
		t.Skipf("globals fixture not present: %v", err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.png")
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		globals:   globalsPath,
		timeout:   30_000_000_000,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
	}
	if code := run(cfg, imgPath, out); code != exitOK {
		t.Fatalf("run code = %d, want %d (exitOK)", code, exitOK)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output file is empty")
	}
}

func TestSegmentTypeName(t *testing.T) {
	cases := map[uint8]string{
		0:  "symbol dictionary",
		16: "pattern dictionary",
		20: "halftone region",
		38: "generic region",
		48: "page information",
		49: "end of page",
		51: "end of file",
		99: "unknown 0x63",
	}
	for typ, want := range cases {
		got := segmentTypeName(typ)
		if !strings.Contains(got, want) {
			t.Errorf("segmentTypeName(%d) = %q, want substring %q", typ, got, want)
		}
	}
}

func TestPrintVersionAndUsage(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf)
	if !strings.Contains(buf.String(), "gobig2") {
		t.Errorf("version output missing 'gobig2': %q", buf.String())
	}
	buf.Reset()
	printUsage(&buf)
	if !strings.Contains(buf.String(), "Usage:") {
		t.Errorf("usage output missing 'Usage:': %q", buf.String())
	}
}

// TestReadBoundedFromCLI pins the CLI's bounded-read contract:
// oversized inputs rejected with ErrResourceBudget-wrapped error
// before full buffer alloc. Without CLI-side cap, naive
// os.ReadFile / io.ReadAll slurps entire file before library's
// bounded read can classify.
func TestReadBoundedFromCLI(t *testing.T) {
	// MaxInputBytes + 1 zero bytes from a counting reader.
	r := &countingZeroSrc{remaining: int64(gobig2.MaxInputBytes) + 1}
	_, err := readBoundedFromCLI(r, "synthetic")
	if err == nil {
		t.Fatal("readBoundedFromCLI accepted over-cap input")
	}
	if !errors.Is(err, gobig2.ErrResourceBudget) {
		t.Errorf("expected ErrResourceBudget wrap, got: %v", err)
	}

	// At-cap input passes through.
	r2 := &countingZeroSrc{remaining: 1024}
	data, err := readBoundedFromCLI(r2, "small")
	if err != nil {
		t.Fatalf("readBoundedFromCLI rejected small input: %v", err)
	}
	if len(data) != 1024 {
		t.Errorf("len(data) = %d, want 1024", len(data))
	}
}

// countingZeroSrc emits `remaining` zero bytes then EOF.
type countingZeroSrc struct {
	remaining int64
}

func (s *countingZeroSrc) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > s.remaining {
		n = s.remaining
	}
	for i := range p[:n] {
		p[i] = 0
	}
	s.remaining -= n
	return int(n), nil
}

// TestRunMultiPageDifferentBytes pins multi-page CLI contract:
// --page=1, --page=2, --page=3 against annex-h fixture all
// produce valid PNG; page 3 differs from page 1.
//
// Annex-h pages 1 and 2 are byte-identical (T.88 Annex H tutorial
// shares dims + content for the demo). Page 3 is a 37x8 region
// with a different bitstream - that pair tests "page-index
// actually advanced".
func TestRunMultiPageDifferentBytes(t *testing.T) {
	const multiPagePath = "../../testdata/serenityos/annex-h.jbig2"
	if _, err := os.Stat(multiPagePath); err != nil {
		t.Skipf("multi-page fixture unavailable: %v", err)
	}
	tmp := t.TempDir()

	runPage := func(p int) []byte {
		t.Helper()
		out := filepath.Join(tmp, fmt.Sprintf("page%d.png", p))
		cfg := flagsConfig{
			format:    "png",
			page:      p,
			timeout:   30 * time.Second,
			maxPixels: 100_000_000,
			maxAlloc:  1 << 30,
		}
		if code := run(cfg, multiPagePath, out); code != exitOK {
			t.Fatalf("run --page=%d code = %d, want %d", p, code, exitOK)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read --page=%d output: %v", p, err)
		}
		return data
	}

	p1 := runPage(1)
	p2 := runPage(2)
	p3 := runPage(3)
	if len(p1) == 0 || len(p2) == 0 || len(p3) == 0 {
		t.Fatal("page output empty")
	}
	// Page 3 differs from page 1 - broken page index (always
	// returns page 1) fails here.
	if bytes.Equal(p1, p3) {
		t.Error("--page=1 and --page=3 produced identical bytes; multi-page page-index accounting may be broken")
	}
	// Belt + suspenders: page 2 should decode. Don't compare
	// to p1; annex-h pages 1+2 intentionally identical.
	if len(p2) < 64 {
		t.Errorf("--page=2 output suspiciously small (%d bytes)", len(p2))
	}
}

// TestRunPageBeyondEOF pins page-past-EOF exit contract: --page
// past end surfaces as exit 2 (usage) with 'beyond end of stream'
// diagnostic, not generic exit 1.
func TestRunPageBeyondEOF(t *testing.T) {
	cfg := flagsConfig{
		format:    "png",
		page:      99, // sample fixture is single-page
		timeout:   30 * time.Second,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
	}

	// run() writes bitmap to stdout when out == "-"; discard
	// via os.Pipe to keep test output clean.
	saved := os.Stdout
	_, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = saved
	}()

	code := run(cfg, fixtureSamplePath, "-")
	if code != exitUsage {
		t.Errorf("run code = %d, want %d (usage / page beyond EOF)", code, exitUsage)
	}
}

// TestRunRestoresMaxImagePixels pins the page.MaxImagePixels
// save/restore contract: run() saves + restores package cap so
// repeat callers don't leak --max-pixels across invocations.
func TestRunRestoresMaxImagePixels(t *testing.T) {
	pre := page.MaxImagePixels
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		timeout:   30 * time.Second,
		maxPixels: 4_242_424, // arbitrary distinct value
		maxAlloc:  1 << 30,
	}
	saved := os.Stdout
	_, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = saved
	}()
	_ = run(cfg, fixtureSamplePath, "-")
	if page.MaxImagePixels != pre {
		t.Errorf("page.MaxImagePixels leaked: pre=%d post=%d", pre, page.MaxImagePixels)
	}
}

// TestRunTimeoutDrainsGoroutine pins timeout-drain contract:
// when timeout fires, run() propagates cancel, waits for decode
// goroutine to observe ctx.Err() at next segment boundary, then
// returns - so deferred restore of page.MaxImagePixels runs
// after goroutine stops mutating decoder state.
//
// timeout = 0 makes [context.WithTimeout] cancel its returned
// context synchronously inside WithDeadline (Go runtime: dur<=0
// path), so ctx.Done() is already closed before the decode
// goroutine starts. That avoids racing the 1 ns wall-clock
// timer against the 94-byte sample's decode time, which on
// Windows the timer loses (timer resolution > sample decode
// time), wins on Linux only by accident of scheduling.
func TestRunTimeoutDrainsGoroutine(t *testing.T) {
	pre := page.MaxImagePixels
	cfg := flagsConfig{
		format:    "png",
		page:      1,
		maxPixels: 4_242_424,
		maxAlloc:  1 << 30,
		timeout:   0, // cancels synchronously, no timer race
	}
	// Pipe stdout into a draining goroutine so writeOutput's
	// PNG bytes never block / EPIPE if the timeout branch
	// doesn't fire for some reason. A bare `_, w, _ := Pipe()`
	// orphans the reader and on Windows the next write hits
	// "the pipe is being closed".
	saved := os.Stdout
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stdout = w
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(drained)
	}()
	defer func() {
		w.Close()
		<-drained
		os.Stdout = saved
	}()
	code := run(cfg, fixtureSamplePath, "-")
	if code != exitTimeoutExceeded {
		t.Errorf("run with 0 timeout = exit %d, want %d (timeout)", code, exitTimeoutExceeded)
	}
	if page.MaxImagePixels != pre {
		t.Errorf("page.MaxImagePixels leaked across timeout path: pre=%d post=%d", pre, page.MaxImagePixels)
	}
}

// TestInspectHonorsTimeout pins --inspect timeout contract:
// --inspect respects --timeout. Without ctx binding, inspect
// drives DecodeSequential in a bare for loop and returns exitOK
// regardless of budget.
func TestInspectHonorsTimeout(t *testing.T) {
	// Build decoder against standalone sample, call inspect()
	// with expired timeout. Should bail with
	// exitTimeoutExceeded before completing segment walk.
	data, err := os.ReadFile(fixtureSamplePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("NewDecoderEmbedded: %v", err)
	}

	// Capture stdout (inspect prints segment table) to keep
	// test output clean.
	saved := os.Stdout
	_, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = saved
	}()

	// Zero timeout: ctx fires on first ctx.Err() check.
	code := inspect(dec, 0, 0)
	if code != exitTimeoutExceeded {
		t.Errorf("inspect with 0 timeout = exit %d, want %d (timeout)", code, exitTimeoutExceeded)
	}
}

func TestRunInspect(t *testing.T) {
	// --inspect writes to stdout. Pipe stdout via os.Stdout
	// reassignment for the call duration.
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.Bytes()
	}()

	cfg := flagsConfig{
		format:    "png",
		page:      1,
		inspect:   true,
		timeout:   30_000_000_000,
		maxPixels: 100_000_000,
		maxAlloc:  1 << 30,
	}
	code := run(cfg, fixtureSamplePath, "-")
	w.Close()
	output := <-done
	os.Stdout = saved

	if code != exitOK {
		t.Fatalf("run code = %d, want %d", code, exitOK)
	}
	s := string(output)
	if !strings.Contains(s, "offset") || !strings.Contains(s, "page information") {
		t.Errorf("inspect output missing expected columns / labels:\n%s", s)
	}
}
