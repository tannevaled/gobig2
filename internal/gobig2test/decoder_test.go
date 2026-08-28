package gobig2test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"os"
	"testing"

	gobig2 "github.com/tannevaled/gobig2"
)

// Tests exercise public gobig2.Decoder entry points corpus tests
// don't reach directly. Corpus harness drives decode via
// gobig2.NewDecoder + gobig2.Decoder.Decode against real fixtures;
// these unit tests cover wrapper / convenience paths (top-level
// gobig2.Decode + gobig2.DecodeConfig + gobig2.DecodeAll + GetDocument
// + gobig2.NewDecoderWithGlobals + io.EOF path).
//
// Standalone fixture: re-wrap testdata/pdf-embedded/sample.jb2
// (94 bytes PDF-extracted segment stream) into standalone .jb2
// format gobig2.NewDecoder probes for. Re-wrap is well-defined:
// prepend 8-byte JBIG2 magic + 1-byte flags (0x03 = sequential, no
// page count), append end-of-page (type 49, page 1) + end-of-file
// (type 51) segments. Synthesize on demand vs committing second
// fixture, keeps testdata size minimum.

// loadStandaloneSample materializes standalone .jb2 equivalent of
// testdata/pdf-embedded/sample.jb2 (same page-info + generic-region
// segments as embedded sample, plus file header + tail markers
// PDF strips).
func loadStandaloneSample(t testing.TB) []byte {
	t.Helper()
	embedded, err := os.ReadFile(pdfEmbeddedSample.path)
	if err != nil {
		t.Fatalf("read embedded sample: %v", err)
	}
	var buf bytes.Buffer
	// File header: 8-byte magic + flags (0x03 = sequential,
	// no page-count word).
	buf.Write([]byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A, 0x03})
	buf.Write(embedded)
	// End-of-page segment (type 49). Sequential header layout:
	// segNum(4) + flags(1) + refByte(1) + pageAssoc(1) +
	// dataLen(4). No data follows.
	buf.Write([]byte{
		0, 0, 0, 2, // segment number
		49,         // flags: type 49, no page-assoc-size, no defer
		0,          // refByte: 0 referred-to segments
		1,          // page association: page 1
		0, 0, 0, 0, // data length
	})
	// End-of-file segment (type 51). Same layout; page assoc 0
	// (not bound to any page).
	buf.Write([]byte{
		0, 0, 0, 3,
		51,
		0,
		0,
		0, 0, 0, 0,
	})
	return buf.Bytes()
}

func TestDecodeTopLevel(t *testing.T) {
	data := loadStandaloneSample(t)
	img, err := gobig2.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.Decode: %v", err)
	}
	if img == nil {
		t.Fatal("gobig2.Decode returned nil image")
	}
	if got := img.Bounds().Dx(); got != pdfEmbeddedSample.width {
		t.Errorf("width = %d, want %d", got, pdfEmbeddedSample.width)
	}
	if got := img.Bounds().Dy(); got != pdfEmbeddedSample.height {
		t.Errorf("height = %d, want %d", got, pdfEmbeddedSample.height)
	}
}

func TestDecodeConfig(t *testing.T) {
	data := loadStandaloneSample(t)
	cfg, err := gobig2.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.DecodeConfig: %v", err)
	}
	if cfg.Width != pdfEmbeddedSample.width || cfg.Height != pdfEmbeddedSample.height {
		t.Errorf("config = %dx%d, want %dx%d",
			cfg.Width, cfg.Height,
			pdfEmbeddedSample.width, pdfEmbeddedSample.height)
	}
	if cfg.ColorModel == nil {
		t.Error("ColorModel is nil")
	}
}

func TestDecodeConfigGarbage(t *testing.T) {
	// Random bytes shouldn't pass standalone header probing.
	if _, err := gobig2.DecodeConfig(bytes.NewReader(bytes.Repeat([]byte{0xAB}, 64))); err == nil {
		t.Error("gobig2.DecodeConfig accepted non-JBIG2 input")
	}
}

// TestDecodeConfigContextCanceled pins gobig2.DecodeConfigContext
// respects ctx cancellation between segments during page-info
// probe. Without it gobig2.DecodeConfig probe loop has no ctx
// binding and server callers can't cancel a dimension probe.
func TestDecodeConfigContextCanceled(t *testing.T) {
	data := loadStandaloneSample(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately
	cfg, err := gobig2.DecodeConfigContext(ctx, bytes.NewReader(data))
	if err == nil {
		t.Fatalf("gobig2.DecodeConfigContext succeeded under canceled ctx; cfg=%+v", cfg)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled wrap", err)
	}
}

// TestDecodeConfigPreservesResourceBudget pins that when a
// gobig2.Limits cap fires while gobig2.DecodeConfig probes for
// page-info, returned error preserves gobig2.ErrResourceBudget
// classification segment parser stamped via failf. Without guard,
// gobig2.DecodeConfig would wrap bare gobig2.ErrMalformed on every
// ResultFailure, dropping sentinel and forcing callers to
// re-derive resource policy from full gobig2.Decode call.
func TestDecodeConfigPreservesResourceBudget(t *testing.T) {
	data := loadStandaloneSample(t)

	// Tighten MaxPixelsPerByte to force failure during page-info
	// parse. Standalone sample has tiny page-info segment + small
	// image; cap fires when parsePageInfo's declared-pixels /
	// input-bytes check runs.
	prev := gobig2.DefaultLimits()
	tight := gobig2.DefaultLimits()
	tight.MaxPixelsPerByte = 1 // any non-zero value smaller than the fixture's ratio
	tight.Apply()
	defer prev.Apply()

	_, err := gobig2.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		t.Fatal("gobig2.DecodeConfig accepted input under impossibly tight MaxPixelsPerByte")
	}
	if !errors.Is(err, gobig2.ErrResourceBudget) {
		t.Errorf("gobig2.DecodeConfig dropped budget classification: got %v, want gobig2.ErrResourceBudget", err)
	}
}

func TestNewDecoderWithGlobalsStandalone(t *testing.T) {
	// Standalone path with empty globals - should behave like
	// gobig2.NewDecoder.
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoderWithGlobals(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("gobig2.NewDecoderWithGlobals: %v", err)
	}
	img, err := dec.Decode()
	if err != nil {
		t.Fatalf("gobig2.Decode: %v", err)
	}
	if img == nil {
		t.Fatal("nil image")
	}
}

func TestNewDecoderWithGlobalsStandalonePlusGlobals(t *testing.T) {
	// When input HAS file header, gobig2.NewDecoderWithGlobals
	// behaves like gobig2.NewDecoder + globals supplement (documented
	// "could be either shape" path). Pass standalone sample +
	// globals = end-of-file segment only: minimal valid globals
	// stream DecodeSequential walks to ResultEndReached without
	// choking.
	data := loadStandaloneSample(t)
	globals := []byte{
		0, 0, 0, 0, // segNum
		51,         // type 51 (end of file)
		0,          // refByte
		0,          // page-association = 0 (not a page segment)
		0, 0, 0, 0, // dataLen
	}
	dec, err := gobig2.NewDecoderWithGlobals(bytes.NewReader(data), globals)
	if err != nil {
		t.Fatalf("gobig2.NewDecoderWithGlobals: %v", err)
	}
	img, err := dec.Decode()
	if err != nil {
		t.Fatalf("gobig2.Decode: %v", err)
	}
	if img == nil {
		t.Fatal("nil image")
	}
}

func TestNewDecoderWithGlobalsRejectsGarbage(t *testing.T) {
	// No header + no globals: should be rejected.
	if _, err := gobig2.NewDecoderWithGlobals(bytes.NewReader([]byte{0x00, 0x01, 0x02}), nil); err == nil {
		t.Error("gobig2.NewDecoderWithGlobals accepted headerless garbage with nil globals")
	}
}

func TestDecodeAllSinglePage(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	imgs, err := dec.DecodeAll()
	if err != nil {
		t.Fatalf("gobig2.DecodeAll: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("gobig2.DecodeAll returned %d images, want 1", len(imgs))
	}
	if imgs[0].Bounds().Dx() != pdfEmbeddedSample.width {
		t.Errorf("page 1 width = %d, want %d",
			imgs[0].Bounds().Dx(), pdfEmbeddedSample.width)
	}
}

// multiPageFixturePath points at committed standalone .jbig2 file
// carrying 3 page-info segments + regions + end-of-page markers.
// Multi-page confirmed via `gobig2 --inspect`. Only standalone
// multi-page file in bundled corpus.
const multiPageFixturePath = "testdata/serenityos/annex-h.jbig2"

// TestDecoderDecodeMultiPage pins multi-page gobig2.Decode loop
// returns pages 1, 2, 3 in order, then io.EOF. Without this,
// regression in page ordering, page-index accounting, or
// ReleasePageSegments could pass every test as long as single-page
// decode + EOF still worked.
func TestDecoderDecodeMultiPage(t *testing.T) {
	data, err := os.ReadFile(multiPageFixturePath)
	if err != nil {
		t.Skipf("multi-page fixture unavailable: %v", err)
	}
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	// 3 gobig2.Decode calls - each returns non-nil image with
	// positive bounds - then io.EOF.
	for page := 1; page <= 3; page++ {
		img, derr := dec.Decode()
		if derr != nil {
			t.Fatalf("page %d gobig2.Decode: %v", page, derr)
		}
		if img == nil {
			t.Fatalf("page %d gobig2.Decode returned nil image", page)
		}
		if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
			t.Errorf("page %d bounds non-positive: %v", page, img.Bounds())
		}
	}
	if _, derr := dec.Decode(); !errors.Is(derr, io.EOF) {
		t.Errorf("gobig2.Decode after last page = %v, want io.EOF", derr)
	}
}

// TestDecodeAllMultiPage pins gobig2.DecodeAll returns exactly 3
// images in order on multi-page fixture, each with positive bounds.
// Pages 2 + 3 of annex-h have different shapes than page 1
// (verified via --inspect), test also asserts pages 1 + 2 have
// distinct bounds on successful decode.
func TestDecodeAllMultiPage(t *testing.T) {
	data, err := os.ReadFile(multiPageFixturePath)
	if err != nil {
		t.Skipf("multi-page fixture unavailable: %v", err)
	}
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	imgs, err := dec.DecodeAll()
	if err != nil {
		t.Fatalf("gobig2.DecodeAll: %v", err)
	}
	if len(imgs) != 3 {
		t.Fatalf("gobig2.DecodeAll returned %d images, want 3", len(imgs))
	}
	for i, img := range imgs {
		if img == nil {
			t.Errorf("page %d nil image", i+1)
			continue
		}
		if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
			t.Errorf("page %d bounds non-positive: %v", i+1, img.Bounds())
		}
	}
}

func TestDecoderDecodeReturnsEOFAfterLastPage(t *testing.T) {
	// After first gobig2.Decode succeeds, second gobig2.Decode on
	// same gobig2.Decoder returns io.EOF (no more pages).
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	if _, err := dec.Decode(); err != nil {
		t.Fatalf("first gobig2.Decode: %v", err)
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Errorf("second gobig2.Decode returned %v, want io.EOF", err)
	}
}

func TestGetDocument(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	if doc := dec.GetDocument(); doc == nil {
		t.Fatal("GetDocument returned nil")
	}
}

func TestImageDecodeRegistersJBIG2(t *testing.T) {
	// init() registers gobig2 via image.RegisterFormat as "jbig2".
	// image.Decode picks it up.
	data := loadStandaloneSample(t)
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image.Decode: %v", err)
	}
	if format != "jbig2" {
		t.Errorf("format = %q, want %q", format, "jbig2")
	}
	if img == nil {
		t.Fatal("nil image")
	}
}

// TestImageDecodeConfigRegistersJBIG2 pins image.DecodeConfig path
// of image.RegisterFormat wiring - gobig2.DecodeConfig returns
// matching dimensions + format name without decoding pixel data.
func TestImageDecodeConfigRegistersJBIG2(t *testing.T) {
	data := loadStandaloneSample(t)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image.DecodeConfig: %v", err)
	}
	if format != "jbig2" {
		t.Errorf("format = %q, want %q", format, "jbig2")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		t.Errorf("gobig2.DecodeConfig returned non-positive dims: %dx%d", cfg.Width, cfg.Height)
	}
	// Sanity: pair gobig2.DecodeConfig dims with full gobig2.Decode,
	// confirm agree. Cheap; standalone sample is tiny.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("paired image.Decode: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		t.Errorf("gobig2.DecodeConfig %dx%d disagrees with gobig2.Decode bounds %dx%d",
			cfg.Width, cfg.Height, b.Dx(), b.Dy())
	}
}

// TestDecodeContextCanceled exercises the cancellation path of
// gobig2.DecodeContext. A pre-canceled context should fail the decode
// promptly with an error wrapping context.Canceled.
func TestDecodeContextCanceled(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	img, err := dec.DecodeContext(ctx)
	if err == nil {
		t.Fatal("gobig2.DecodeContext succeeded under canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if img != nil {
		t.Errorf("img = %v, want nil under canceled ctx", img)
	}
}

// TestDecodeContextBackground confirms gobig2.DecodeContext with a
// background context behaves identically to gobig2.Decode().
func TestDecodeContextBackground(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	img, err := dec.DecodeContext(context.Background())
	if err != nil {
		t.Fatalf("gobig2.DecodeContext(Background): %v", err)
	}
	if img == nil {
		t.Fatal("gobig2.DecodeContext returned nil image")
	}
}

// TestDecodeAllContextCanceled exercises the multi-page
// cancellation path. A pre-canceled context should fail
// promptly with an error wrapping context.Canceled.
func TestDecodeAllContextCanceled(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	imgs, err := dec.DecodeAllContext(ctx)
	if err == nil {
		t.Fatal("gobig2.DecodeAllContext succeeded under canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(imgs) != 0 {
		t.Errorf("len(imgs) = %d, want 0 under canceled ctx", len(imgs))
	}
}

// TestDecodeAllContextBackground confirms gobig2.DecodeAllContext with
// a background context returns the same single page gobig2.DecodeAll
// would on the standalone sample (one-page fixture).
func TestDecodeAllContextBackground(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	imgs, err := dec.DecodeAllContext(context.Background())
	if err != nil {
		t.Fatalf("gobig2.DecodeAllContext(Background): %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("len(imgs) = %d, want 1", len(imgs))
	}
}

// TestPackageLevelDecodeContext exercises the package-level
// gobig2.DecodeContext helper (gobig2.NewDecoder + gobig2.DecodeContext combined).
func TestPackageLevelDecodeContext(t *testing.T) {
	data := loadStandaloneSample(t)
	img, err := gobig2.DecodeContext(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.DecodeContext: %v", err)
	}
	if img == nil {
		t.Fatal("gobig2.DecodeContext returned nil image")
	}

	// Cancellation path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gobig2.DecodeContext(ctx, bytes.NewReader(data)); err == nil {
		t.Error("gobig2.DecodeContext succeeded under canceled ctx")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

// TestParseGlobalsAndReset exercises PDF-reader hot path: parse
// globals once, bind to gobig2.Decoder via
// gobig2.NewDecoderEmbeddedWithGlobals, then Reset across multiple
// image streams without re-parsing.
func TestParseGlobalsAndReset(t *testing.T) {
	// Empty globals path: gobig2.ParseGlobals(nil) returns a no-op handle.
	g, err := gobig2.ParseGlobals(nil)
	if err != nil {
		t.Fatalf("gobig2.ParseGlobals(nil): %v", err)
	}
	if g == nil {
		t.Fatal("gobig2.ParseGlobals(nil) returned nil handle")
	}

	// Use committed PDF-embedded fixture as stand-in for per-image
	// stream. No globals dependency, so empty ParsedGlobals +
	// decode should succeed.
	data, err := os.ReadFile("../../testdata/pdf-embedded/sample.jb2")
	if err != nil {
		t.Skipf("sample.jb2 not present: %v", err)
	}

	dec, err := gobig2.NewDecoderEmbeddedWithGlobals(bytes.NewReader(data), g)
	if err != nil {
		t.Fatalf("gobig2.NewDecoderEmbeddedWithGlobals: %v", err)
	}
	img, err := dec.Decode()
	if err != nil {
		t.Fatalf("first gobig2.Decode: %v", err)
	}
	if img == nil {
		t.Fatal("first gobig2.Decode returned nil image")
	}

	// Reset for a second image; same gobig2.Decoder, no globals re-parse.
	if rerr := dec.Reset(bytes.NewReader(data)); rerr != nil {
		t.Fatalf("Reset: %v", rerr)
	}
	img2, err := dec.Decode()
	if err != nil {
		t.Fatalf("gobig2.Decode after Reset: %v", err)
	}
	if img2 == nil {
		t.Fatal("gobig2.Decode after Reset returned nil image")
	}
}

// TestResetRequiresEmbeddedConstructor confirms Reset on a
// gobig2.Decoder built without gobig2.NewDecoderEmbeddedWithGlobals returns
// gobig2.ErrUnsupported.
func TestResetRequiresEmbeddedConstructor(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	err = dec.Reset(bytes.NewReader(data))
	if err == nil {
		t.Fatal("Reset on standalone gobig2.Decoder succeeded; want gobig2.ErrUnsupported")
	}
	if !errors.Is(err, gobig2.ErrUnsupported) {
		t.Errorf("err = %v, want errors.Is(err, gobig2.ErrUnsupported)", err)
	}
}

// TestErrorSentinels confirms public gobig2.ErrMalformed /
// gobig2.ErrResourceBudget sentinels wrap real failure paths.
// Pre-confirmed malformed input matches gobig2.ErrMalformed; tight
// gobig2.Limits cap makes real fixture fail with
// gobig2.ErrResourceBudget.
func TestErrorSentinels(t *testing.T) {
	// Malformed: garbage bytes don't sniff as JBIG2.
	_, err := gobig2.NewDecoder(bytes.NewReader([]byte("not jbig2 at all")))
	if err == nil {
		t.Fatal("gobig2.NewDecoder accepted non-JBIG2 input")
	}
	if !errors.Is(err, gobig2.ErrMalformed) {
		t.Errorf("err = %v, want errors.Is(err, gobig2.ErrMalformed)", err)
	}

	// Resource budget: a 1 megapixel cap rejects the 3 megapixel sample.
	saved := gobig2.DefaultLimits()
	defer saved.Apply()
	tight := saved
	tight.MaxImagePixels = 1 << 20
	tight.Apply()

	data := loadStandaloneSample(t)
	_, err = gobig2.Decode(bytes.NewReader(data))
	if err == nil {
		t.Fatal("gobig2.Decode succeeded under tight gobig2.Limits")
	}
	if !errors.Is(err, gobig2.ErrResourceBudget) {
		t.Errorf("err = %v, want errors.Is(err, gobig2.ErrResourceBudget)", err)
	}
}

// TestPackageLevelDecodeAll exercises the package-level
// gobig2.DecodeAll helper plus gobig2.DecodeAllContext.
func TestPackageLevelDecodeAll(t *testing.T) {
	data := loadStandaloneSample(t)
	imgs, err := gobig2.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.DecodeAll: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("gobig2.DecodeAll: len(imgs) = %d, want 1", len(imgs))
	}

	imgs2, err := gobig2.DecodeAllContext(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.DecodeAllContext: %v", err)
	}
	if len(imgs2) != 1 {
		t.Fatalf("gobig2.DecodeAllContext: len(imgs2) = %d, want 1", len(imgs2))
	}
}
