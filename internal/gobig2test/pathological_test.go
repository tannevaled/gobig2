package gobig2test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	gobig2 "github.com/tannevaled/gobig2"

	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/segment"
)

// TestAggregateInputSymbolsBudget asserts aggregate SBSYMS /
// SDINSYMS pool a text region or symbol dict assembles across all
// referenced symbol-dict segments is bounded by MaxSymbolsPerDict.
// Without aggregate cap, adversary could chain N dicts each below
// per-dict cap, growing pool unbounded. Real corpora top at 308
// aggregate input symbols; bitmap-symbol-manyrefs fixture has 5
// across 5 referenced dicts, so cap=3 should reject with
// gobig2.ErrResourceBudget.
func TestAggregateInputSymbolsBudget(t *testing.T) {
	data, err := os.ReadFile("../../testdata/serenityos/bitmap-symbol-manyrefs.jbig2")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}

	// Lower package-var cap. Saved+restored so change doesn't
	// leak into other tests in same run.
	prev := segment.MaxSymbolsPerDict
	segment.MaxSymbolsPerDict = 3
	defer func() { segment.MaxSymbolsPerDict = prev }()

	res := decodeWithBudget(data, nil, 5*time.Second, decodeMemory)
	if res.timedOut {
		t.Fatalf("decode hung past 5s - aggregate cap not bounding")
	}
	if res.err == nil {
		t.Fatalf("decode unexpectedly succeeded; expected aggregate-cap rejection")
	}
	if !errors.Is(res.err, gobig2.ErrResourceBudget) {
		t.Fatalf("expected gobig2.ErrResourceBudget, got: %v", res.err)
	}
	if !strings.Contains(res.err.Error(), "MaxSymbolsPerDict") {
		t.Fatalf("expected MaxSymbolsPerDict mention, got: %v", res.err)
	}
	t.Logf("rejected with: %v", res.err)
}

// TestPathologicalSymbolDictBudget asserts JBIG2 stream declaring
// absurd symbol count rejected before allocation proportional to
// count. Without MaxSymbolsPerDict boundary check, symbol-dict
// parser would call make([]*Image, SDNUMNEWSYMS) and attempt 30+
// GB allocation.
//
// Fixture built in code vs committed bytes - hex-table form more
// readable than binary blob in testdata/, documents which
// segment-header field drives bug.
func TestPathologicalSymbolDictBudget(t *testing.T) {
	// Minimum-viable JBIG2 file with one symbol-dict segment
	// claiming 4 G symbols. Layout:
	//   File header:    97 4A 42 32 0D 0A 1A 0A   (8-byte magic)
	//                   01                         (flags: sequential, no random access)
	//                   00 00 00 01                (number of pages = 1)
	//   Segment 0:      00 00 00 00                (segment number)
	//                   30                         (flags: type 48 = page info)
	//                   00                         (refer-to count = 0)
	//                   01                         (page association)
	//                   00 00 00 13                (data length = 19)
	//                   00 00 00 40                (page width = 64)
	//                   00 00 00 38                (page height = 56)
	//                   00 00 00 00                (X resolution = 0)
	//                   00 00 00 00                (Y resolution = 0)
	//                   00                         (page flags = 0)
	//                   00 00                      (striping info = 0)
	//   Segment 1:      00 00 00 01                (segment number = 1)
	//                   00                         (flags: type 0 = symbol dict)
	//                   00                         (refer-to count = 0)
	//                   01                         (page association)
	//                   00 00 00 0a                (data length = 10)
	//                   00 00                      (SD flags = 0; SDHUFF=0, SDTEMPLATE=0)
	//                   FF FF FF FF                (SDNUMEXSYMS = uint32 max)
	//                   FF FF FF FF                (SDNUMNEWSYMS = uint32 max)
	pathological := []byte{
		0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A,
		0x01,
		0x00, 0x00, 0x00, 0x01,
		// segment 0 (page info)
		0x00, 0x00, 0x00, 0x00,
		0x30,
		0x00,
		0x01,
		0x00, 0x00, 0x00, 0x13,
		0x00, 0x00, 0x00, 0x40,
		0x00, 0x00, 0x00, 0x38,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00,
		0x00, 0x00,
		// segment 1 (symbol dict header + body, SDTEMPLATE=0 so 8 SDAT bytes are
		// expected, but we never get that far because SDNUMNEWSYMS validation
		// fires first).
		0x00, 0x00, 0x00, 0x01,
		0x00,
		0x00,
		0x01,
		0x00, 0x00, 0x00, 0x12, // data length = 18 (flags 2 + SDAT 8 + EXSYMS 4 + NEWSYMS 4)
		0x00, 0x00, // SD flags (SDHUFF=0, SDTEMPLATE=0, SDREFAGG=0)
		0x03, 0xff, 0xfd, 0xff, 0x02, 0xfe, 0xfe, 0xfe, // SDAT default for template 0
		0xFF, 0xFF, 0xFF, 0xFF, // SDNUMEXSYMS (would be a uint32 max)
		0xFF, 0xFF, 0xFF, 0xFF, // SDNUMNEWSYMS (would be a uint32 max)
	}

	res := decodeWithBudget(pathological, nil, 2*time.Second, decodeMemory)
	if res.timedOut {
		t.Fatalf("decode hung past 2s - pathological fixture not bounded")
	}
	if res.err == nil {
		t.Fatalf("decode unexpectedly succeeded; expected MaxSymbolsPerDict rejection")
	}
	if !strings.Contains(res.err.Error(), "MaxSymbolsPerDict") {
		t.Fatalf("expected MaxSymbolsPerDict rejection, got: %v", res.err)
	}
	if !errors.Is(res.err, gobig2.ErrResourceBudget) {
		t.Fatalf("expected wrap with gobig2.ErrResourceBudget for CLI exit classification, got: %v", res.err)
	}
	if res.peakMB > 64 {
		t.Fatalf("decode allocated %d MiB before rejecting; expected <= 64 MiB", res.peakMB)
	}
}

// TestPathologicalRegionDimensionsBudget asserts JBIG2 stream
// declaring absurd region size rejected by MaxImagePixels check in
// NewImage vs allocating full rectangle.
func TestPathologicalRegionDimensionsBudget(t *testing.T) {
	// Minimum-viable file with page-info segment declaring
	// width x height = uint32 max x uint32 max = 16 EiB. NewImage
	// returns nil; page-info parser surfaces failure vs asking for
	// that many bits of memory.
	pathological := []byte{
		0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A,
		0x01,
		0x00, 0x00, 0x00, 0x01,
		// segment 0 (page info) with 4G x 4G dimensions
		0x00, 0x00, 0x00, 0x00,
		0x30,
		0x00,
		0x01,
		0x00, 0x00, 0x00, 0x13,
		0xFF, 0xFF, 0xFF, 0xFF, // page width = uint32 max
		0xFF, 0xFF, 0xFF, 0xFF, // page height = uint32 max
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00,
		0x00, 0x00,
	}

	res := decodeWithBudget(pathological, nil, 2*time.Second, decodeMemory)
	if res.timedOut {
		t.Fatalf("decode hung past 2s on absurd page dimensions")
	}
	if res.err == nil {
		t.Fatalf("decode unexpectedly succeeded; expected NewImage rejection")
	}
	// NewImage MaxImagePixels guard keeps page bitmap allocation
	// in check; decoder reports failure at page-info segment level
	// via wrapped error.
	if !errors.Is(res.err, gobig2.ErrResourceBudget) {
		t.Fatalf("expected wrap with gobig2.ErrResourceBudget for CLI exit classification, got: %v", res.err)
	}
	if res.peakMB > 64 {
		t.Fatalf("decode allocated %d MiB before rejecting; expected <= 64 MiB", res.peakMB)
	}
	t.Logf("rejected with: %v", res.err)
}

// TestMalformedMMRClassifiesAsErrMalformed pins truncated MMR
// fixture classifies as gobig2.ErrMalformed (CLI exit 3) because
// parseGenericRegion routes DecodeMMR errors through
// classifyLeafErr. Distinct from oversize / NewImage-nil which
// DecodeMMR still wraps as gobig2.ErrResourceBudget.
func TestMalformedMMRClassifiesAsErrMalformed(t *testing.T) {
	full, err := os.ReadFile("../../testdata/serenityos/bitmap-mmr.jbig2")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	// Cut at 100 bytes: inside generic-region body so mmr.DecodeG4
	// hits "out of bounds" on truncated codewords. Page-info and
	// segment headers parse cleanly; only MMR payload is short.
	truncated := full[:100]
	res := decodeWithBudget(truncated, nil, 2*time.Second, decodeMemory)
	if res.timedOut {
		t.Fatalf("decode hung on truncated MMR fixture")
	}
	if res.err == nil {
		t.Fatalf("decode unexpectedly succeeded on truncated MMR fixture")
	}
	if errors.Is(res.err, gobig2.ErrResourceBudget) {
		t.Fatalf("malformed MMR body classified as gobig2.ErrResourceBudget; "+
			"expected gobig2.ErrMalformed. got: %v", res.err)
	}
	if !errors.Is(res.err, gobig2.ErrMalformed) {
		t.Fatalf("expected gobig2.ErrMalformed for malformed MMR body, got: %v", res.err)
	}
	t.Logf("rejected with: %v", res.err)
}

// TestTruncatedSegmentHeaderClassifiesAsErrMalformed pins
// ParseSegmentData fallback (segment-header parsing fails without
// inner failf call) stamps gobig2.ErrMalformed too. Without guard,
// path produces bare "parse failed" error; CLI exit-code
// classification falls through to generic exit 1.
func TestTruncatedSegmentHeaderClassifiesAsErrMalformed(t *testing.T) {
	full, err := os.ReadFile("../../testdata/serenityos/bitmap-mmr.jbig2")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	// Cut at 70 bytes: page-info parses, generic-region segment
	// header incomplete. Triggers ParseSegmentData fallback vs
	// inner failf.
	truncated := full[:70]
	res := decodeWithBudget(truncated, nil, 2*time.Second, decodeMemory)
	if res.timedOut {
		t.Fatalf("decode hung on truncated-header fixture")
	}
	if res.err == nil {
		t.Fatalf("decode unexpectedly succeeded on truncated-header fixture")
	}
	if !errors.Is(res.err, gobig2.ErrMalformed) {
		t.Fatalf("expected gobig2.ErrMalformed for truncated segment header, got: %v", res.err)
	}
	t.Logf("rejected with: %v", res.err)
}

// TestBoundedInputReadCap pins public constructors reject inputs
// larger than bio.MaxInputBytes up front with
// gobig2.ErrResourceBudget vs allocating full read and surfacing
// downstream malformed error.
//
// Uses io.Reader synthesizing bio.MaxInputBytes + 1 bytes without
// materializing buffer in test memory; reader stops at cap+1
// sentinel byte so bounded-read helper detects over-cap condition
// with only ~256 MiB held inside constructor for failure path.
// (Larger reader would work too; 256 MiB enough to hit guard.)
func TestBoundedInputReadCap(t *testing.T) {
	// Build a reader that emits gobig2.MaxInputBytes+1 zero bytes.
	overCap := int64(bio.MaxInputBytes) + 1
	r := &countingZeroReader{remaining: overCap}

	_, err := gobig2.NewDecoder(r)
	if err == nil {
		t.Fatal("gobig2.NewDecoder accepted over-cap input")
	}
	if !errors.Is(err, gobig2.ErrResourceBudget) {
		t.Errorf("expected gobig2.ErrResourceBudget, got: %v", err)
	}
}

// countingZeroReader produces `remaining` zero bytes then EOF.
// Cheap source for synthetic huge input without holding whole
// buffer in test heap.
type countingZeroReader struct {
	remaining int64
}

func (r *countingZeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = 0
	}
	r.remaining -= n
	return int(n), nil
}

// TestResetAfterNilGlobals pins gobig2.Decoder built with
// gobig2.NewDecoderEmbeddedWithGlobals(r, nil) - documented
// self-contained-stream path - is resettable. Without guard,
// constructor would store nil on resetGlobals, which
// gobig2.Decoder.Reset uses as sentinel for 'not built by this
// constructor' and would reject with gobig2.ErrUnsupported.
func TestResetAfterNilGlobals(t *testing.T) {
	data, err := os.ReadFile("../../testdata/pdf-embedded/sample.jb2")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	dec, err := gobig2.NewDecoderEmbeddedWithGlobals(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("gobig2.NewDecoderEmbeddedWithGlobals(r, nil): %v", err)
	}
	// Reset for new image of same shape. Resettable property is
	// documented contract; should not surface as
	// gobig2.ErrUnsupported.
	resetErr := dec.Reset(bytes.NewReader(data))
	if resetErr != nil {
		t.Fatalf("Reset after nil-globals construction failed: %v", resetErr)
	}
	if errors.Is(resetErr, gobig2.ErrUnsupported) {
		t.Fatalf("Reset surfaced gobig2.ErrUnsupported despite the documented "+
			"nil-globals contract: %v", resetErr)
	}
}

// TestSmallGlobalsDoNotTripBudget pins public globals entry points
// don't reject small globals slices as budget failures.
func TestSmallGlobalsDoNotTripBudget(t *testing.T) {
	// Sanity: each public constructor wires helper. Pass small
	// valid globals slice - should accept (or fail for content
	// reasons, not budget). Budget rejection here means
	// constructor short-circuits helper or wires different bound.
	tinyJB2 := []byte{0x00}
	tinyGlobals := []byte{0x00, 0x00}
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"gobig2.NewDecoderEmbedded", func() error {
			_, err := gobig2.NewDecoderEmbedded(bytes.NewReader(tinyJB2), tinyGlobals)
			return err
		}},
		{"gobig2.NewDecoderWithGlobals", func() error {
			_, err := gobig2.NewDecoderWithGlobals(bytes.NewReader(tinyJB2), tinyGlobals)
			return err
		}},
		{"gobig2.ParseGlobals", func() error {
			_, err := gobig2.ParseGlobals(tinyGlobals)
			return err
		}},
	} {
		if err := c.run(); errors.Is(err, gobig2.ErrResourceBudget) {
			t.Errorf("%s: small globals (%d bytes) rejected as gobig2.ErrResourceBudget: %v",
				c.name, len(tinyGlobals), err)
		}
	}
}

// TestDecodeFailureReleasesPageBitmap pins terminal
// gobig2.DecodeContext failure releases in-progress packed page
// bitmap, so caller retaining gobig2.Decoder for diagnostic
// inspection (e.g. GetDocument().GetSegments()) doesn't keep
// MaxImagePixels-sized allocation alive past abort.
func TestDecodeFailureReleasesPageBitmap(t *testing.T) {
	// Truncate committed MMR fixture in middle of generic-region
	// payload. parsePageInfo succeeds, allocates d.page; then
	// mmr.DecodeG4 fails on truncated codewords inside
	// parseGenericRegion. Terminal failure should drop d.page
	// before returning.
	full, err := os.ReadFile("../../testdata/serenityos/bitmap-mmr.jbig2")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	truncated := full[:100] // page-info parses, MMR body cut

	dec, err := gobig2.NewDecoder(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	_, derr := dec.Decode()
	if derr == nil {
		t.Fatal("gobig2.Decode unexpectedly succeeded on truncated MMR fixture")
	}
	// After terminal failure, document current-page bitmap should
	// be nil. Segment list should still be populated for
	// diagnostic inspection.
	doc := dec.GetDocument()
	if doc == nil {
		t.Fatal("GetDocument returned nil after failure")
	}
	if doc.Page() != nil {
		t.Errorf("doc.Page() = %v after terminal failure; want nil (packed bitmap should be released)", doc.Page())
	}
	if len(doc.GetSegments()) == 0 {
		t.Error("doc.GetSegments() empty after failure; diagnostic info lost")
	}
}

// TestRejectGarbageInput exercises embedded-mode sniff: random
// bytes not resembling JBIG2 segment header rejected immediately
// vs driving segment parser into long search.
func TestRejectGarbageInput(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xff}, 256)
	if _, err := gobig2.NewDecoderEmbedded(bytes.NewReader(garbage), nil); err == nil {
		t.Fatalf("expected error from gobig2.NewDecoderEmbedded on garbage")
	}

	// Plain ASCII text: HTTP fetch of wrong URL could deliver
	// this. Should fail loud at sniff layer, not pretend it's a
	// segment stream.
	text := []byte("This is not a JBIG2 stream; it is plain ASCII.")
	if _, err := gobig2.NewDecoderEmbedded(bytes.NewReader(text), nil); err == nil {
		t.Fatalf("expected error from gobig2.NewDecoderEmbedded on ASCII input")
	}
}
