package gobig2test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	gobig2 "github.com/tannevaled/gobig2"
)

// PDF-embedded JBIG2 streams have no file header - PDF §7.4.7
// requires producer to strip 8-byte magic, file-flags byte, optional
// page-count word, end-of-page/end-of-file tail markers. JBIG2Decode
// filter reads sequence of segment headers + payloads.
//
// gobig2.NewDecoderEmbedded (and gobig2.NewDecoderEmbeddedWithGlobals
// + ParsedGlobals) are entry points for this shape. Tests exercise
// both.
//
//   - TestPDFEmbeddedSampleFixture: committed-fixture form. 94-byte
//     sample.jb2 in testdata/pdf-embedded/. Decodes, asserts
//     dimensions + SHA-256 over raw bitmap so embedded-path
//     regression can't slip past.
//
//   - TestPDFEmbeddedCorpus walks testdata/pdf-embedded/serenityos/
//     by default - 98 PDF-extracted .jb2 + 1 paired .globals.jb2
//     (bitmap-p32-eof-obj6). Each .jb2 pairs with optional
//     .globals.jb2 sibling; test parses globals once, builds one
//     gobig2.Decoder via gobig2.NewDecoderEmbeddedWithGlobals, decodes
//     within standard 10 s / 1 GiB budget. JBIG2_PDF_EXTRACTED_DIR
//     overrides path for fresher local copy.

const (
	envPDFExtracted        = "JBIG2_PDF_EXTRACTED_DIR"
	defaultPDFExtractedDir = "../../testdata/pdf-embedded/serenityos"
)

// pdfEmbeddedSample names committed sample fixture + invariants.
// Dimensions + raw-bitmap hash in one place for cheap regression
// cross-check.
//
// Fixture: 94 bytes PDF-extracted JBIG2 - one page-info segment
// (DEFPIXEL=0, white default) + one 53-byte generic region whose
// payload XORs onto page. Decoded bitmap fully black across all
// 3,031,262 pixels; SHA-256 below over packed MSB-first bytes a
// fresh decode produces. One-pixel flip breaks test.
var pdfEmbeddedSample = struct {
	path   string
	width  int
	height int
	sha256 string
}{
	path:   filepath.Join("..", "..", "testdata", "pdf-embedded", "sample.jb2"),
	width:  3562,
	height: 851,
	sha256: "e44aad16c4c8385d0b0f7f624ba7660a5db7055ff4ae10a07e9e71e04f422695",
}

// TestPDFEmbeddedSampleFixture is the always-on regression for
// embedded decode path. Fixture + invariants committed under
// testdata/pdf-embedded/.
func TestPDFEmbeddedSampleFixture(t *testing.T) {
	data, err := os.ReadFile(pdfEmbeddedSample.path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("gobig2.NewDecoderEmbedded: %v", err)
	}
	img, err := dec.Decode()
	if err != nil {
		t.Fatalf("gobig2.Decode: %v", err)
	}
	if img == nil {
		t.Fatal("gobig2.Decode returned nil image")
	}
	gotW := img.Bounds().Dx()
	gotH := img.Bounds().Dy()
	if gotW != pdfEmbeddedSample.width || gotH != pdfEmbeddedSample.height {
		t.Fatalf("dimensions = %dx%d, want %dx%d",
			gotW, gotH, pdfEmbeddedSample.width, pdfEmbeddedSample.height)
	}

	// Hash underlying bi-level bitmap, not image.Gray rendering -
	// gray rendering inflates each bit to a byte, 8x noise in hash.
	// Decoder returns image.Image; *Image alias has Data() byte
	// view reachable via type assertion.
	bits, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("expected *image.Gray, got %T", img)
	}
	got := sha256OfGrayBits(bits)
	if got != pdfEmbeddedSample.sha256 {
		t.Errorf("bitmap hash = %s, want %s", got, pdfEmbeddedSample.sha256)
	}
}

// sha256OfGrayBits hashes 1-bit-equivalent grayscale image by
// rebuilding packed MSB-first byte form and digesting. Can't hash
// *image.Gray buffer directly: each bit stored as one byte (0 or
// 255), bitmap content buried in 7-of-8 padding bytes per pixel.
func sha256OfGrayBits(g *image.Gray) string {
	w := g.Bounds().Dx()
	h := g.Bounds().Dy()
	stride := (w + 7) / 8
	row := make([]byte, stride)
	hasher := sha256.New()
	for y := 0; y < h; y++ {
		for i := range row {
			row[i] = 0
		}
		for x := 0; x < w; x++ {
			c := g.GrayAt(x, y).Y
			if c < 128 { // ink
				row[x>>3] |= 1 << (7 - uint(x&7))
			}
		}
		hasher.Write(row)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// TestPDFEmbeddedCorpus walks PDF-extracted JBIG2 corpus, decodes
// every .jb2 via gobig2.NewDecoderEmbeddedWithGlobals (with paired
// .globals.jb2 sibling when present) inside standard time + memory
// budget.
//
// Defaults to testdata/pdf-embedded/serenityos/ - corpus vendored
// from SerenityOS-generated PDFs. Override path via
// JBIG2_PDF_EXTRACTED_DIR for local copy of different corpus (e.g.
// real-world PDF extractions during integration testing).
//
// Matches *.jb2. .globals.jb2 paired with image sibling (filename
// prefix) and parsed via gobig2.ParseGlobals; not decoded standalone
// (globals stream has no page-info segment, would EOF). .txt
// provenance sidecars skipped.
func TestPDFEmbeddedCorpus(t *testing.T) {
	dir := os.Getenv(envPDFExtracted)
	if dir == "" {
		dir = defaultPDFExtractedDir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	// Collect image fixtures and a basename -> globals-bytes map.
	type fixture struct {
		path        string
		globalsPath string // empty when no paired globals
	}
	globalsByPrefix := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".globals.jb2") {
			prefix := strings.TrimSuffix(name, ".globals.jb2")
			globalsByPrefix[prefix] = filepath.Join(dir, name)
		}
	}
	var fixtures []fixture
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jb2") || strings.HasSuffix(name, ".globals.jb2") {
			continue
		}
		prefix := strings.TrimSuffix(name, ".jb2")
		fixtures = append(fixtures, fixture{
			path:        filepath.Join(dir, name),
			globalsPath: globalsByPrefix[prefix], // "" if no sibling
		})
	}
	if len(fixtures) == 0 {
		t.Skipf("no PDF-extracted fixtures found in %s", dir)
	}
	for _, f := range fixtures {
		f := f
		t.Run(filepath.Base(f.path), func(t *testing.T) {
			data, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var globals []byte
			if f.globalsPath != "" {
				gb, gerr := os.ReadFile(f.globalsPath)
				if gerr != nil {
					t.Fatalf("read globals %s: %v", f.globalsPath, gerr)
				}
				globals = gb
			}
			img, err := decodeEmbeddedWithBudget(data, globals)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if img == nil {
				t.Fatal("decode produced nil image")
			}
		})
	}
}

// decodeEmbeddedWithBudget runs PDF-embedded decode path against
// data + optional globals inside standard 10 s / 1 GiB ceiling used
// by conformance harness. Routes via
// gobig2.NewDecoderEmbeddedWithGlobals when globals non-nil (real
// PDF-reader hot path) or gobig2.NewDecoderEmbedded otherwise.
//
// Shared decodeWithBudget in corpus_test.go targets auto-detect path
// (gobig2.NewDecoder); PDF-embedded fixtures need embedded entry
// points specifically so test fails loud if standalone header
// probing silently locks onto bytes resembling a header.
func decodeEmbeddedWithBudget(data, globals []byte) (image.Image, error) {
	type result struct {
		img image.Image
		err error
	}
	prevLimit := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(1 << 30)
	defer debug.SetMemoryLimit(prevLimit)
	done := make(chan result, 1)
	// Plumb cancellation so decode goroutine unwinds at next segment
	// boundary when wall-clock budget fires. Without this goroutine
	// keeps gobig2.Decoder / Document / page graph alive after test
	// reports failure; under -race or parallel tests this stacks
	// concurrent decoder graphs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		var dec *gobig2.Decoder
		var err error
		if len(globals) > 0 {
			parsed, perr := gobig2.ParseGlobals(globals)
			if perr != nil {
				done <- result{nil, perr}
				return
			}
			dec, err = gobig2.NewDecoderEmbeddedWithGlobals(bytes.NewReader(data), parsed)
		} else {
			dec, err = gobig2.NewDecoderEmbedded(bytes.NewReader(data), nil)
		}
		if err != nil {
			done <- result{nil, err}
			return
		}
		img, err := dec.DecodeContext(ctx)
		done <- result{img, err}
	}()
	// time.NewTimer + defer Stop avoids leak `case <-time.After(...)`
	// leaves on success path.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.img, r.err
	case <-timer.C:
		cancel()
		return nil, errBudgetTimeout
	}
}

var errBudgetTimeout = errors.New("decode timed out (10 s wall-clock)")
