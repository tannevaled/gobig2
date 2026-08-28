package segment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/page"
)

// TestParseSegmentHeaderClassifiesMidHeaderTruncations pins
// mid-header failure classification: ParseSegmentHeader stamps
// d.lastErr with ErrMalformed for every mid-header read fail
// or semantic violation, so DecodeSequential / decodeGrouped
// can't mis-route malformed header as quiet EOF.
//
// segNumber-read failure stays the only quiet-EOF boundary -
// natural end-of-stream signal for sequential walk. Every read
// past that sets lastErr.
func TestParseSegmentHeaderClassifiesMidHeaderTruncations(t *testing.T) {
	cases := []struct {
		name        string
		input       []byte
		wantQuietOK bool // true if we expect ResultFailure WITHOUT lastErr
	}{
		{
			// Empty - segNumber read fails. Quiet-EOF boundary:
			// callers treat as 'no more segments', not malformed.
			name:        "empty input - segNumber read fails (quiet EOF)",
			input:       nil,
			wantQuietOK: true,
		},
		{
			// 4-byte segNumber then EOF. Flags-byte read fails
			// mid-header; without classification, indistinguishable
			// from segNumber-EOF.
			name:        "segNumber present, flags byte truncated",
			input:       []byte{0x00, 0x00, 0x00, 0x01},
			wantQuietOK: false,
		},
		{
			// segNumber + flags(0x00=symbol-dict) + 1-byte refByte
			// but no pageAssoc/dataLen - page-assoc read truncates.
			name:        "page-assoc byte truncated",
			input:       []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00},
			wantQuietOK: false,
		},
		{
			// segNumber + flags + refByte + pageAssoc, dataLen
			// truncated.
			name:        "dataLen word truncated",
			input:       []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00},
			wantQuietOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDocument(c.input, nil, false, false)
			seg := NewSegment()
			res := d.ParseSegmentHeader(seg)
			if res != ResultFailure {
				t.Fatalf("ParseSegmentHeader = %v, want ResultFailure", res)
			}
			lastErr := d.Err()
			if c.wantQuietOK {
				if lastErr != nil {
					t.Errorf("quiet-EOF case set lastErr = %v; want nil so callers can treat as end-of-stream", lastErr)
				}
				return
			}
			if lastErr == nil {
				t.Fatalf("mid-header truncation produced no lastErr; without classification this is indistinguishable from clean EOF")
			}
			if !errors.Is(lastErr, errs.ErrMalformed) {
				t.Errorf("mid-header truncation err = %v, want ErrMalformed wrap", lastErr)
			}
		})
	}
}

// TestParseSegmentHeaderRejectsForwardReference pins
// semantic-violation classification: referred-to segment
// number self/forward in sequential mode surfaces as
// ErrMalformed, not quiet end-of-stream.
func TestParseSegmentHeaderRejectsForwardReference(t *testing.T) {
	// Layout: segNumber=2, flags=0x07 (text region, 1 byte refByte),
	//   refByte = 1 ref (high 3 bits = 001 << 5 = 0x20),
	//   ref[0] = 5 (forward of segNumber=2, illegal in sequential).
	input := []byte{
		0x00, 0x00, 0x00, 0x02, // segNumber = 2
		0x07,                   // flags = type 7 = text region
		0x20,                   // refByte: refCount = 1
		0x05,                   // ref[0] = 5 (forward)
		0x01,                   // page assoc
		0x00, 0x00, 0x00, 0x00, // data length
	}
	d := NewDocument(input, nil, false, false)
	seg := NewSegment()
	res := d.ParseSegmentHeader(seg)
	if res != ResultFailure {
		t.Fatalf("ParseSegmentHeader = %v, want ResultFailure", res)
	}
	lastErr := d.Err()
	if lastErr == nil {
		t.Fatal("forward reference produced no lastErr")
	}
	if !errors.Is(lastErr, errs.ErrMalformed) {
		t.Errorf("forward ref err = %v, want ErrMalformed wrap", lastErr)
	}
}

// TestExpandPageForStripedRegion pins wrap-safe arithmetic:
// expandPageForStripedRegion uses int64 so negative Y or
// positive Y+Height near math.MaxInt32 can't wrap. Naive
// `uint32(ri.Y) + uint32(ri.Height)` would reinterpret
// negative Y as huge uint32 and forward wrapped value to
// Expand's int32 param.
func TestExpandPageForStripedRegion(t *testing.T) {
	cases := []struct {
		name       string
		startW     int32
		startH     int32
		ri         RegionInfo
		wantHeight int32
	}{
		{
			name:       "regular below current - expand",
			startW:     16,
			startH:     100,
			ri:         RegionInfo{X: 0, Y: 50, Width: 16, Height: 100},
			wantHeight: 150,
		},
		{
			name:       "bottom at current - no-op",
			startW:     16,
			startH:     100,
			ri:         RegionInfo{X: 0, Y: 50, Width: 16, Height: 50},
			wantHeight: 100,
		},
		{
			name:       "negative Y above page top - effective bottom < height, no-op",
			startW:     16,
			startH:     100,
			ri:         RegionInfo{X: 0, Y: -20, Width: 16, Height: 10},
			wantHeight: 100,
		},
		{
			name:       "negative Y with large height - bottom past current, expand to bottom",
			startW:     16,
			startH:     100,
			ri:         RegionInfo{X: 0, Y: -20, Width: 16, Height: 200},
			wantHeight: 180, // -20 + 200 = 180
		},
		{
			name: "Y near MaxInt32 with positive Height - bottom > MaxInt32, no-op",
			// Without int64 widening, uint32(MaxInt32) + uint32(1) =
			// MaxInt32+1 casts back to negative int32, passed to
			// Expand; helper rejects this case.
			startW:     16,
			startH:     100,
			ri:         RegionInfo{X: 0, Y: math.MaxInt32, Width: 16, Height: 1},
			wantHeight: 100,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := page.NewImage(c.startW, c.startH)
			if p == nil {
				t.Fatalf("setup NewImage(%d, %d) failed", c.startW, c.startH)
			}
			expandPageForStripedRegion(p, c.ri, false)
			if p.Height() != c.wantHeight {
				t.Errorf("height = %d, want %d", p.Height(), c.wantHeight)
			}
		})
	}
}

// TestExpandPageForStripedRegionNilPage exercises nil-page
// guard - must not panic on nil page, matching parsers'
// nil-tolerance.
func TestExpandPageForStripedRegionNilPage(t *testing.T) {
	expandPageForStripedRegion(nil, RegionInfo{Y: 0, Height: 10}, false)
}

// TestClassifyLeafErr pins leaf-error classification: region
// parsers route DecodeMMR / DecodeArith / refinement.Decode
// through classifyLeafErr, which passes classified
// resource-budget errors through and wraps unclassified
// stream-corruption with ErrMalformed. Without wrap, CLI
// exit-code falls through to exit 1 (generic) instead of
// exit 3 (malformed) for bad MMR streams.
func TestClassifyLeafErr(t *testing.T) {
	cases := []struct {
		name        string
		in          error
		wantMalf    bool
		wantBudget  bool
		wantUnsupp  bool
		wantPasstru bool
	}{
		{
			name:        "resource-budget passes through unchanged",
			in:          fmt.Errorf("oversize: %w", errs.ErrResourceBudget),
			wantBudget:  true,
			wantPasstru: true,
		},
		{
			name:        "unsupported passes through unchanged",
			in:          fmt.Errorf("not implemented: %w", errs.ErrUnsupported),
			wantUnsupp:  true,
			wantPasstru: true,
		},
		{
			name:        "already-malformed passes through unchanged",
			in:          fmt.Errorf("bad bytes: %w", errs.ErrMalformed),
			wantMalf:    true,
			wantPasstru: true,
		},
		{
			name:     "raw stream-corruption error gets ErrMalformed wrap",
			in:       errors.New("mmr: out of bounds"),
			wantMalf: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyLeafErr(c.in)
			if errors.Is(got, errs.ErrMalformed) != c.wantMalf {
				t.Errorf("malformed=%v, want %v (err=%v)",
					errors.Is(got, errs.ErrMalformed), c.wantMalf, got)
			}
			if errors.Is(got, errs.ErrResourceBudget) != c.wantBudget {
				t.Errorf("budget=%v, want %v (err=%v)",
					errors.Is(got, errs.ErrResourceBudget), c.wantBudget, got)
			}
			if errors.Is(got, errs.ErrUnsupported) != c.wantUnsupp {
				t.Errorf("unsupported=%v, want %v (err=%v)",
					errors.Is(got, errs.ErrUnsupported), c.wantUnsupp, got)
			}
			// Pass-through check needs pointer equality
			// (classifyLeafErr returned same value, not a wrap).
			// errors.Is short-circuits through any wrap; identity
			// is the signal here.
			if c.wantPasstru && got != c.in { //nolint:errorlint // intentional identity check
				t.Errorf("expected pass-through, got wrapped: in=%v out=%v", c.in, got)
			}
		})
	}
}

// Direct unit tests for Document accessors + plumbing. Big
// parser bodies (parseSymbolDictInner, parseTextRegion, etc.)
// exercised via corpus tests in root; these cover tiny helpers
// corpus misses - primarily nil-receiver guards + the
// FindSegmentByNumber walk.

// TestDocumentNilReceivers exercises every accessor with a
// `if d == nil` guard. Calling on nil Document is documented
// contract (constructors return nil on early reject; downstream
// walks accessors without separate nil check); guards keep
// the contract alive.
func TestDocumentNilReceivers(t *testing.T) {
	var d *Document // intentionally nil

	// SetContext should be a no-op, not a panic.
	d.SetContext(context.Background())

	// SetGlobalContext should also no-op.
	d.SetGlobalContext(&Document{})

	if got := d.Err(); got != nil {
		t.Errorf("nil.Err() = %v, want nil", got)
	}
	if got := d.GlobalContextDoc(); got != nil {
		t.Errorf("nil.GlobalContextDoc() = %v, want nil", got)
	}
	if got := d.InPage(); got {
		t.Errorf("nil.InPage() = true, want false")
	}
	if got := d.Page(); got != nil {
		t.Errorf("nil.Page() = %v, want nil", got)
	}
	if got := d.PageInfoList(); got != nil {
		t.Errorf("nil.PageInfoList() = %v, want nil", got)
	}
	if got := d.StreamOffset(); got != 0 {
		t.Errorf("nil.StreamOffset() = %d, want 0", got)
	}
}

// TestDocumentSetContext: SetContext stores value; subsequent
// Err() returns nil when no failure recorded.
func TestDocumentSetContext(t *testing.T) {
	d := NewDocument(nil, nil, false, false)
	d.SetContext(context.Background())

	if err := d.Err(); err != nil {
		t.Errorf("Err() before any decode = %v, want nil", err)
	}

	// TODO context exercises the swap path without
	// SetContext(nil) (staticcheck flags). Decoder treats both
	// as "no cancellation".
	d.SetContext(context.TODO())
}

// TestReleaseCurrentPageBitmapClearsInPage: dropping in-flight
// page bitmap on failure path also drops in-page flag, so
// retry doesn't see inconsistent (page==nil && inPage==true)
// state region parsers reject as type-X-outside-a-page.
func TestReleaseCurrentPageBitmapClearsInPage(t *testing.T) {
	d := NewDocument(nil, nil, false, false)
	d.SetInPage(true)
	d.ReleaseCurrentPageBitmap()
	if d.InPage() {
		t.Error("InPage() still true after ReleaseCurrentPageBitmap; " +
			"want false so retry-after-error starts clean")
	}
}

// TestFindSegmentByNumberLocal walks the local segmentList
// branch (the dominant path during decode).
func TestFindSegmentByNumberLocal(t *testing.T) {
	d := NewDocument(nil, nil, false, false)
	a := NewSegment()
	a.Number = 7
	b := NewSegment()
	b.Number = 42
	d.segmentList = []*Segment{a, b}

	if got := d.FindSegmentByNumber(7); got != a {
		t.Errorf("FindSegmentByNumber(7) = %v, want segment a", got)
	}
	if got := d.FindSegmentByNumber(42); got != b {
		t.Errorf("FindSegmentByNumber(42) = %v, want segment b", got)
	}
	if got := d.FindSegmentByNumber(99); got != nil {
		t.Errorf("FindSegmentByNumber(99) = %v, want nil", got)
	}
}

// TestFindSegmentByNumberGlobalContext exercises globals
// fallthrough: if not found locally, lookup recurses into
// bound globalContext. How real images bound to /JBIG2Globals
// resolve symbol-dict refs.
func TestFindSegmentByNumberGlobalContext(t *testing.T) {
	g := NewDocument(nil, nil, false, false)
	gSeg := NewSegment()
	gSeg.Number = 100
	g.segmentList = []*Segment{gSeg}

	d := NewDocument(nil, nil, false, false)
	d.SetGlobalContext(g)

	if got := d.FindSegmentByNumber(100); got != gSeg {
		t.Errorf("FindSegmentByNumber(100) = %v, want global seg", got)
	}
}

// TestFindSegmentByNumberGlobalShadowsLocal pins global-first
// collision policy on [Document.FindSegmentByNumber]: number
// in both globalContext and local segmentList -> globals win,
// local shadowed. T.88 §7.2.2 forbids the collision so any
// hit is malformed; test pins resolution choice so a future
// refactor flipping order (or rejecting) breaks this test
// deliberately rather than silently changing semantics.
func TestFindSegmentByNumberGlobalShadowsLocal(t *testing.T) {
	g := NewDocument(nil, nil, false, false)
	gSeg := NewSegment()
	gSeg.Number = 5
	g.segmentList = []*Segment{gSeg}

	d := NewDocument(nil, nil, false, false)
	d.SetGlobalContext(g)
	localSeg := NewSegment()
	localSeg.Number = 5
	d.segmentList = []*Segment{localSeg}

	if got := d.FindSegmentByNumber(5); got != gSeg {
		t.Errorf("FindSegmentByNumber(5) = %v, want global seg "+
			"(implementation walks globals first)", got)
	}
}

// TestParseSegmentDataInner_Type48Page exercises page-info
// dispatch in parseSegmentDataInner: type 48 routes through
// parsePageInfo and stores dims on Document.page.
func TestParseSegmentDataInner_Type48Page(t *testing.T) {
	// Page-info payload (T.88 §7.4.8): 19 bytes:
	//   width (4) + height (4) + xres (4) + yres (4) +
	//   flags (1) + striping (2). Minimal 100x100, no stripes.
	payload := []byte{
		0, 0, 0, 100, // width
		0, 0, 0, 100, // height
		0, 0, 0, 72, // xres
		0, 0, 0, 72, // yres
		0,          // flags
		0x00, 0x00, // striping
	}
	d := NewDocument(payload, nil, false, false)
	seg := NewSegment()
	seg.Flags.Type = 48
	seg.DataLength = uint32(len(payload))

	if r := d.parseSegmentDataInner(seg); r != ResultSuccess {
		t.Fatalf("parseSegmentDataInner = %v, want ResultSuccess; err=%v", r, d.Err())
	}
	pages := d.PageInfoList()
	if len(pages) != 1 {
		t.Fatalf("PageInfoList len = %d, want 1", len(pages))
	}
	if pages[0].Width != 100 || pages[0].Height != 100 {
		t.Errorf("page dims = %dx%d, want 100x100",
			pages[0].Width, pages[0].Height)
	}
	if !d.InPage() {
		t.Error("InPage() = false after parsePageInfo, want true")
	}
}

// TestFailfRecordsFirstError: failf records first error,
// ignores later ones. Original cause survives later failures.
func TestFailfRecordsFirstError(t *testing.T) {
	d := NewDocument(nil, nil, false, false)
	d.failf("first: %s", "cause")
	d.failf("second: %s", "decoy")

	got := d.Err()
	if got == nil {
		t.Fatal("Err() = nil after failf, want first cause")
	}
	if got.Error() != "first: cause" {
		t.Errorf("Err() = %q, want %q (first wins)", got, "first: cause")
	}
}

// TestParseSegmentDataInner_RegionInfoOOB: a region-segment
// header that runs out of bytes mid-RegionInfo must surface
// failure, not panic.
func TestParseSegmentDataInner_RegionInfoOOB(t *testing.T) {
	// type 38 = generic region; payload too short for region info
	d := NewDocument([]byte{0x01}, nil, false, false)
	seg := NewSegment()
	seg.Flags.Type = 38
	seg.DataLength = 1

	r := d.parseSegmentDataInner(seg)
	if r != ResultFailure {
		t.Errorf("truncated region-info parse = %v, want ResultFailure", r)
	}
}

// TestNewGlobalsDocumentEmpty: NewGlobalsDocument with empty
// data produces valid (but exhausted) Document, not panic.
// Public ParseGlobals(nil) short-circuits before this; this
// is defense-in-depth.
func TestNewGlobalsDocumentEmpty(t *testing.T) {
	g := NewGlobalsDocument(nil, false)
	if g == nil {
		t.Fatal("NewGlobalsDocument(nil) returned nil")
	}
	if !g.isGlobal {
		t.Error("isGlobal flag not set on globals doc")
	}
}

// buildGroupedSinglePage builds minimal grouped-layout stream:
// three headers (page-info, end-of-page, end-of-file) then
// their data. Matches T.88 random-access org (all headers
// first, then data). end-of-file terminates header walk;
// without it parser reads past data block as more headers and
// trips per-segment byte cap.
func buildGroupedSinglePage() []byte {
	hdr := func(num uint32, flags, ref, pageAssoc byte, dataLen uint32) []byte {
		return []byte{
			byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num),
			flags, ref, pageAssoc,
			byte(dataLen >> 24), byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		}
	}
	pageInfoData := []byte{
		0, 0, 0, 8, // width
		0, 0, 0, 8, // height
		0, 0, 0, 0, // xres
		0, 0, 0, 0, // yres
		0,    // flags
		0, 0, // striping
	}
	var buf []byte
	buf = append(buf, hdr(1, 48, 0, 1, uint32(len(pageInfoData)))...)
	buf = append(buf, hdr(2, 49, 0, 1, 0)...)
	buf = append(buf, hdr(3, 51, 0, 0, 0)...)
	buf = append(buf, pageInfoData...)
	return buf
}

// TestDecodeGroupedPropagatesPageAndEnd pins grouped-mode
// result propagation: data walk hands ResultPageCompleted /
// ResultEndReached back to public Decoder so completed bitmap
// gets delivered. Naive data loop that always returned
// ResultSuccess would make DecodeContext report io.EOF without
// releasing page image.
func TestDecodeGroupedPropagatesPageAndEnd(t *testing.T) {
	data := buildGroupedSinglePage()
	d := NewDocument(data, nil, true, false)
	d.OrgMode = 1
	d.Grouped = true

	// First call: header walk completes, data walk parses
	// page-info (allocates page) and reaches end-of-page.
	// Expect ResultPageCompleted.
	res := d.DecodeSequential()
	if res != ResultPageCompleted {
		if err := d.Err(); err != nil {
			t.Fatalf("DecodeSequential: res=%v err=%v", res, err)
		}
		t.Fatalf("first DecodeSequential = %v, want ResultPageCompleted", res)
	}
	if d.Page() == nil {
		t.Fatal("page image not allocated after page-info segment")
	}
	if d.InPage() {
		t.Error("InPage() still true after end-of-page; type-49 dispatch did not clear it")
	}

	// Second call: no segments left. Expect ResultEndReached
	// so public loop returns io.EOF, not spinning.
	res = d.DecodeSequential()
	if res != ResultEndReached {
		t.Errorf("second DecodeSequential = %v, want ResultEndReached", res)
	}
}

// buildGroupedTwoPage extends buildGroupedSinglePage with a
// second page (page-info + end-of-page) before end-of-file.
// Header walk collects all five headers; data walk: parse
// page 1, ResultPageCompleted at p1 end-of-page, parse page 2
// next call, ResultPageCompleted at p2 end-of-page, then
// ResultEndReached at end-of-file.
func buildGroupedTwoPage() []byte {
	hdr := func(num uint32, flags, ref, pageAssoc byte, dataLen uint32) []byte {
		return []byte{
			byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num),
			flags, ref, pageAssoc,
			byte(dataLen >> 24), byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		}
	}
	pageInfoData := func(w, h byte) []byte {
		return []byte{
			0, 0, 0, w, // width
			0, 0, 0, h, // height
			0, 0, 0, 0, // xres
			0, 0, 0, 0, // yres
			0,    // flags
			0, 0, // striping
		}
	}
	page1 := pageInfoData(8, 8)
	page2 := pageInfoData(4, 4)
	var buf []byte
	buf = append(buf, hdr(1, 48, 0, 1, uint32(len(page1)))...)
	buf = append(buf, hdr(2, 49, 0, 1, 0)...)
	buf = append(buf, hdr(3, 48, 0, 2, uint32(len(page2)))...)
	buf = append(buf, hdr(4, 49, 0, 2, 0)...)
	buf = append(buf, hdr(5, 51, 0, 0, 0)...)
	buf = append(buf, page1...)
	buf = append(buf, page2...)
	return buf
}

// TestDecodeGroupedMultiPageResumesAfterRelease pins grouped-mode
// cursor-adjustment invariant: grouped tracks data-walk position
// as absolute index into segmentList, and page handoff
// (Decoder.DecodeContext) calls ReleasePageSegments to compact.
// Without cursor adjust, p1 release shifts p2's segments below
// stale cursor; next call hits ResultEndReached without handing
// p2's bitmap. ReleasePageSegments subtracts removed-below-cursor
// from d.groupedDataIdx so resume index lines up with compacted
// list.
func TestDecodeGroupedMultiPageResumesAfterRelease(t *testing.T) {
	data := buildGroupedTwoPage()
	d := NewDocument(data, nil, true, false)
	d.OrgMode = 1
	d.Grouped = true

	// Page 1.
	res := d.DecodeSequential()
	if res != ResultPageCompleted {
		if err := d.Err(); err != nil {
			t.Fatalf("page 1 DecodeSequential: res=%v err=%v", res, err)
		}
		t.Fatalf("page 1 DecodeSequential = %v, want ResultPageCompleted", res)
	}
	if d.Page() == nil {
		t.Fatal("page 1 image not allocated")
	}
	if w, h := d.Page().Width(), d.Page().Height(); w != int32(8) || h != int32(8) {
		t.Errorf("page 1 dims = %dx%d, want 8x8", w, h)
	}

	// Public Decoder simulates: page hand-off triggers
	// ReleasePageSegments. Regression hook: cursor adjust lives here.
	d.ReleasePageSegments(1)

	// Page 2 should now resume cleanly.
	res = d.DecodeSequential()
	if res != ResultPageCompleted {
		if err := d.Err(); err != nil {
			t.Fatalf("page 2 DecodeSequential: res=%v err=%v", res, err)
		}
		t.Fatalf("page 2 DecodeSequential = %v, want ResultPageCompleted "+
			"(regression: ReleasePageSegments must adjust groupedDataIdx)", res)
	}
	if d.Page() == nil {
		t.Fatal("page 2 image not allocated")
	}
	if w, h := d.Page().Width(), d.Page().Height(); w != int32(4) || h != int32(4) {
		t.Errorf("page 2 dims = %dx%d, want 4x4", w, h)
	}

	d.ReleasePageSegments(2)

	// End-of-file segment.
	res = d.DecodeSequential()
	if res != ResultEndReached {
		t.Errorf("final DecodeSequential = %v, want ResultEndReached", res)
	}
}

// TestDecodeSequentialQuietTailReturnsEndReached pins behavior
// at segment-boundary quiet EOF: 1-3 trailing bytes (too few
// for segNum read) make ParseSegmentHeader fail quietly (no
// lastErr stamp). DecodeSequential surfaces as ResultEndReached
// so public loop returns io.EOF, not falling through to
// ResultSuccess and tripping no-progress guard as ErrMalformed
// on next iter.
func TestDecodeSequentialQuietTailReturnsEndReached(t *testing.T) {
	// 3 bytes total - less than 4-byte segNum read needs.
	// ParseSegmentHeader's first ReadInteger fails with no
	// lastErr stamped (documented quiet-EOF boundary).
	data := []byte{0xAA, 0xBB, 0xCC}
	d := NewDocument(data, nil, false, false)

	res := d.DecodeSequential()
	if res != ResultEndReached {
		t.Errorf("DecodeSequential on quiet tail = %v, want ResultEndReached", res)
	}
	if err := d.Err(); err != nil {
		t.Errorf("quiet tail recorded lastErr = %v, want nil", err)
	}
}

// TestDecodeGroupedZeroLengthSegmentsTrackProgress: public
// no-progress guard observes grouped-mode data-walk index
// advances, not only stream-cursor. Per-call cap yields
// ResultSuccess after 64 segments; in grouped, run of
// zero-length segments (end-of-stripe/-of-file/page-info
// dataLen=0) advances groupedDataIdx but not stream cursor,
// so guard checking only StreamOffset() sees "no progress"
// and classifies two cap-yields as ErrMalformed.
//
// Fixture: >128 zero-length type-50 header-only segments
// before end-of-file. First DecodeSequential consumes 64
// (yields cap), second consumes next 64 (yields again), third
// consumes rest through end-of-file. Progress() returns
// strictly increasing token across every call even though
// stream cursor only changes inside header walk.
func TestDecodeGroupedZeroLengthSegmentsTrackProgress(t *testing.T) {
	hdr := func(num uint32, flags, ref, pageAssoc byte, dataLen uint32) []byte {
		return []byte{
			byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num),
			flags, ref, pageAssoc,
			byte(dataLen >> 24), byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		}
	}
	var buf []byte
	const total = 150
	for i := uint32(1); i < total; i++ {
		// Type 50 = end-of-stripe; legal dispatchable type with
		// zero payload. Each segment occupies only 11-byte header.
		buf = append(buf, hdr(i, 50, 0, 0, 0)...)
	}
	// End-of-file segment terminates the header walk.
	buf = append(buf, hdr(total, 51, 0, 0, 0)...)

	d := NewDocument(buf, nil, true, false)
	d.OrgMode = 1
	d.Grouped = true

	prevProgress := d.Progress()
	stalled := 0
	for iter := 0; iter < 16; iter++ {
		res := d.DecodeSequential()
		if res == ResultFailure {
			t.Fatalf("iter %d: DecodeSequential = ResultFailure, err=%v", iter, d.Err())
		}
		if res == ResultEndReached {
			return // success
		}
		p := d.Progress()
		if p == prevProgress {
			stalled++
			if stalled >= 2 {
				t.Fatalf("iter %d: Progress() did not advance for two consecutive calls (stalled, no-progress would fire)", iter)
			}
		} else {
			stalled = 0
			prevProgress = p
		}
	}
	t.Fatal("never reached ResultEndReached")
}

// TestRegionOutsidePageWrapsErrMalformed: dispatcher's
// region-without-page-info failure carries errs.ErrMalformed
// in lastErr chain so public errors.Is(err, gobig2.ErrMalformed)
// match holds. Previous failf shape stamped sentinel-less
// error that bypassed inline wrap and ParseSegmentData
// fallback (which only fires when lastErr is nil), leaving
// public match false.
func TestRegionOutsidePageWrapsErrMalformed(t *testing.T) {
	// Type 38 = immediate generic region dispatched outside
	// a page (d.inPage == false at construction). Payload
	// irrelevant; gate fires before any byte read.
	d := NewDocument(nil, nil, false, false)
	seg := NewSegment()
	seg.Flags.Type = 38

	if r := d.parseSegmentDataInner(seg); r != ResultFailure {
		t.Fatalf("parseSegmentDataInner = %v, want ResultFailure", r)
	}
	err := d.Err()
	if err == nil {
		t.Fatal("parseSegmentDataInner stamped no lastErr")
	}
	if !errors.Is(err, errs.ErrMalformed) {
		t.Errorf("err = %v, want errors.Is(err, ErrMalformed)", err)
	}
}

// TestDecodeSequentialYieldsAtPerCallCap: hitting per-call
// segment budget yields ResultSuccess (no error stamp), not
// failing the whole decode. Real streams with many tiny
// segments before a page boundary (or many globals) must
// re-enter DecodeSequential and keep going.
//
// Stream of 80 zero-length type-50 segments (legal
// payload-skippable, dispatcher passes through no-alloc).
// Cap at 64: first call consumes 64, yields ResultSuccess;
// second consumes remaining 16 to ResultEndReached.
func TestDecodeSequentialYieldsAtPerCallCap(t *testing.T) {
	hdr := func(num uint32, flags, ref, pageAssoc byte, dataLen uint32) []byte {
		return []byte{
			byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num),
			flags, ref, pageAssoc,
			byte(dataLen >> 24), byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		}
	}
	var buf []byte
	const total = 80
	for i := uint32(1); i <= total; i++ {
		// Type 50 = end-of-stripe; AddOffset(0) is no-op so
		// segment consumes only 11-byte header.
		buf = append(buf, hdr(i, 50, 0, 0, 0)...)
	}
	d := NewDocument(buf, nil, false, false)

	res := d.DecodeSequential()
	if res != ResultSuccess {
		t.Fatalf("first DecodeSequential at cap = %v, want ResultSuccess (yield)", res)
	}
	if err := d.Err(); err != nil {
		t.Fatalf("yield stamped lastErr = %v, want nil", err)
	}
	prevOff := d.StreamOffset()
	if prevOff == 0 {
		t.Fatal("stream cursor did not advance during yield call")
	}

	// Re-enter until EndReached. Remaining 16 consume one more
	// Success call (loop exits on GetByteLeft == 0), then
	// EndReached on the next.
	for iter := 0; iter < 8; iter++ {
		res = d.DecodeSequential()
		if res == ResultEndReached {
			break
		}
		if res != ResultSuccess {
			t.Fatalf("re-entry %d DecodeSequential = %v, want ResultSuccess or ResultEndReached", iter, res)
		}
	}
	if res != ResultEndReached {
		t.Errorf("never reached ResultEndReached after multiple re-entries (got %v)", res)
	}
}
