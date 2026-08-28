// Package gobig2 decodes ITU-T T.88 / ISO/IEC 14492 JBIG2 streams.
//
// Built first for PDF readers (JBIG2Decode filter is dominant
// in the wild), then general-purpose JBIG2 decode.
//
// # Three entry points, one per input shape
//
// JBIG2 has two on-the-wire forms. Standalone .jb2 / .jbig2
// start with 8-byte magic + flags byte (and optional 4-byte
// page-count); embedded streams - PDF /JBIG2Decode shape
// (PDF §7.4.7) - start directly at first segment header. PDF
// /JBIG2Decode also strips end-of-page (segment type 49) and
// end-of-file (type 51) markers a standalone file carries.
//
// Pick constructor matching input:
//
//   - [NewDecoder] - standalone file. Probes magic, locks onto
//     right organization mode. Auto-registered with
//     [image.Decode] under format name "jbig2".
//
//   - [NewDecoderEmbedded] - PDF-embedded segment stream. Skips
//     header probing. Pass nil globals when stream is
//     self-contained; pass decoded JBIG2Globals bytes when image
//     dict references external context.
//
//   - [NewDecoderWithGlobals] - auto-detect with globals
//     fallback. With file header: behaves like NewDecoder,
//     globals supplement. Header missing + globals non-empty:
//     falls back to embedded mode. Use when input could be
//     either shape (e.g. PDF reader hitting stream still
//     wrapped in standalone form).
//
// # PDF integration
//
// A PDF reader walking image XObjects gets two byte streams per
// /JBIG2Decode-filtered image:
//
//   - Image stream itself - segment-stream form.
//   - Optional /JBIG2Globals via /DecodeParms - separate JBIG2
//     segment stream holding symbol-dict contexts shared across
//     document.
//
// Both bytes in hand:
//
//	dec, err := gobig2.NewDecoderEmbedded(imageStream, globalsBytes)
//	if err != nil {
//	    // adversarial / non-JBIG2 input is rejected up front
//	    return nil, err
//	}
//	img, err := dec.Decode()
//
// Returned image.Image is *image.Gray; ink = 0 (black),
// paper = 255 (white). Dimensions match page-information
// segment inside stream - will not necessarily equal PDF image
// dict's /Width and /Height (PDF readers should trust JBIG2
// stream's own dimensions, let PDF /Width and /Height drive CTM
// scaling around them).
//
// # Resource budgets
//
// JBIG2 is a DoS vector: attacker declares 30 GB region in
// 100-byte segment header, naive decoder OOMs. Every length /
// count / dimension derived from input bytes gated against
// configurable cap before allocation. Nine caps on [Limits];
// always start from [DefaultLimits] and tweak fields you want:
//
//	limits := gobig2.DefaultLimits()
//	limits.MaxImagePixels = 100 * 1024 * 1024 // tighten one cap
//	limits.Apply()
//
// Bare literal `gobig2.Limits{MaxImagePixels: 1<<20}.Apply()`
// silently disables every other cap (zero = "no cap") - see
// [Limits.Apply] footgun. Apply is process-wide, not safe
// concurrent with active decodes - configure once at startup,
// then spawn workers. Concurrent Decoder instances calling
// Decode / DecodeContext on independent inputs safe (each owns
// own Document; Limits read-only post-Apply). See [Limits] doc
// for per-field reference.
//
// Pair with wall-clock budget on call site. Decoder honors
// cancellation via [Decoder.DecodeContext] - internal
// segment-parser loop checks ctx.Err() between segments,
// aborts as failure on cancel. Cancellation latency bounded by
// cost of one segment, itself gated by per-region Limits above.
//
// # Multi-page streams
//
// Standalone .jb2 can declare multiple pages; PDF streams
// always one page (page = image XObject). [Decoder.Decode]
// returns one page at a time, io.EOF when no more pages.
// [Decoder.DecodeAll] is the convenience wrapper.
//
// [DecodeConfig] reports first page's dimensions of a
// standalone .jb2 stream - function image.DecodeConfig
// dispatches to, requires T.88 Annex E file header.
// PDF-embedded streams (/JBIG2Decode filter bytes) omit header
// and fail DecodeConfig with ErrMalformed; PDF readers wanting
// page-info from /JBIG2Decode must build Decoder via
// [NewDecoderEmbedded] or [NewDecoderEmbeddedWithGlobals] and
// walk to first page-info segment themselves (typically via
// [Decoder.GetDocument].[Document.PageInfoList] after first
// [Decoder.DecodeContext] call).
//
// # Error handling
//
// All public entry points return error on malformed/truncated
// input; codec does not panic, even on adversarial bytes.
// Errors carry context (segment number, parser stage) for
// triage with --inspect.
//
// Errors wrap one of three sentinels for [errors.Is]:
// [ErrMalformed] (input not legal JBIG2), [ErrResourceBudget]
// (configured [Limits] cap fired), [ErrUnsupported] (legal but
// uses unimplemented feature). Cancellation paths additionally
// wrap [context.Canceled] / [context.DeadlineExceeded]. See
// errors.go for switch idiom.
package gobig2

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"

	"github.com/tannevaled/gobig2/internal/input"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/probe"
	"github.com/tannevaled/gobig2/internal/segment"
)

// Version is the gobig2 module version. Bumped at release time
// alongside the matching git tag. Runtime read is the supported
// way to feature-detect across versions; pre-1.0 value is
// "0.0.0-dev" (repo ships no tagged releases yet).
const Version = "0.0.0-dev"

// MaxInputBytes is constructor-level hard cap on physical bytes
// a single JBIG2 input can occupy. Larger inputs rejected up
// front by public constructors with error wrapping
// ErrResourceBudget, before any bitmap allocation. 256 MiB far
// above legit JBIG2 (600-DPI A4 fax page typically 100 KB-10 MB
// on wire); real per-region budgets via [Limits]. CLI tools or
// framing readers (PDF, fax) that pre-slurp input should apply
// same cap.
const MaxInputBytes = input.MaxBytes

// Type + constructor aliases gobig2 root needs to keep public
// API readable without re-importing internal/segment in every
// file. Each alias has at least one caller here; if caller
// removed, drop alias rather than let it accumulate as legacy.

// Document is the document-parsing context returned by
// [Decoder.GetDocument].
type Document = segment.Document

// NewDocument creates a Document. Used internally by every
// constructor; exported for rare callers building a Document
// directly (most go through [NewDecoder] / [NewDecoderEmbedded]).
//
// Stability: package-level var bound to [segment.NewDocument],
// not function declaration. Indirection is implementation
// detail of how gobig2 re-exports internal/segment types; do
// not reassign - runtime swap to different constructor is not
// supported extension point, breaks surprisingly (decode-loop
// callers dispatch through it). Future major release will
// likely replace with real function wrapper; treat as
// call-only.
var NewDocument = segment.NewDocument

// newGlobalsDocument is globals-only Document constructor.
// Lowercase to stay out of public API surface; [ParseGlobals]
// is the supported entry point.
var newGlobalsDocument = segment.NewGlobalsDocument

// Result is the document parser's per-step result code, returned
// by [Document.DecodeSequential]. The public Decoder loop in
// [Decoder.Decode] / [Decoder.DecodeContext] switches on it.
type Result = segment.Result

// Result codes from [Document.DecodeSequential]. Callers walking
// a Document directly switch on these; the [Decoder] wrappers
// handle them internally and surface the appropriate
// [image.Image] / error.
const (
	// ResultSuccess: a segment parsed successfully; decode loop
	// should call DecodeSequential again to advance.
	ResultSuccess = segment.ResultSuccess
	// ResultFailure: a parse or resource-budget failure; the
	// concrete error is on [Document.Err].
	ResultFailure = segment.ResultFailure
	// ResultEndReached: input is exhausted with no more segments
	// to parse. After this, the next Decode call returns io.EOF.
	ResultEndReached = segment.ResultEndReached
	// ResultPageCompleted: a page-info segment closed the
	// current page; the page bitmap is ready on
	// [Document.Page].
	ResultPageCompleted = segment.ResultPageCompleted
)

// Decoder is a JBIG2 decoder bound to one input stream. A single
// Decoder yields one or more pages via [Decoder.Decode] /
// [Decoder.DecodeContext] / [Decoder.DecodePacked] /
// [Decoder.DecodePackedContext]; [Decoder.Reset] rebinds the
// decoder to a fresh stream (only on Decoders built with
// [NewDecoderEmbeddedWithGlobals], to reuse parsed globals).
//
// Decoder is NOT safe for concurrent decode calls on the same
// instance; spawn one Decoder per worker. Resource budgets are
// process-wide and come from [Limits.Apply] - configure once at
// startup, before any Decoder runs.
type Decoder struct {
	doc       *Document
	pageIndex uint32
	// resetGlobals is ParsedGlobals captured when Decoder built
	// with [NewDecoderEmbeddedWithGlobals]. [Decoder.Reset]
	// rebuilds Document for new input stream while keeping
	// these globals attached. nil on Decoders built without
	// that constructor - Reset returns [ErrUnsupported].
	resetGlobals *ParsedGlobals
}

// NewDecoder creates a decoder.
//
// SWF / Flash CWS container scanning intentionally out of
// scope: JBIG2 codec should not drag compress/zlib into import
// graph for payload shape no fixture exercises. Callers
// needing SWF-wrapped JBIG2 should strip SWF container in own
// layer, feed inner stream to NewDecoder / NewDecoderEmbedded
// directly.
func NewDecoder(r io.Reader) (*Decoder, error) {
	data, err := input.ReadBounded(r)
	if err != nil {
		return nil, err
	}
	cfg := probe.Configs(data)
	if cfg == nil {
		return nil, fmt.Errorf("no valid jbig2 configuration found: %w", ErrMalformed)
	}
	if err := probe.RejectUnsupportedOrg(cfg.RandomAccess, cfg.OrgMode); err != nil {
		return nil, err
	}
	doc := NewDocument(cfg.Data, nil, cfg.RandomAccess, cfg.LittleEndian)
	doc.OrgMode = cfg.OrgMode
	doc.Grouped = cfg.Grouped
	return &Decoder{doc: doc, pageIndex: 0}, nil
}

// NewDecoderEmbedded creates a decoder for JBIG2 stream with no
// file header - "embedded" stream starting directly at first
// segment header. PDF /JBIG2Decode delivers this shape
// (PDF §7.4.7: "the file header, end-of-page segment, and
// end-of-file segment shall not be present"). Pass empty
// globals for self-contained streams, or decoded /JBIG2Globals
// bytes when referencing external symbol-dict context.
//
// Auto-detect path in NewDecoder / NewDecoderWithGlobals needs
// 8-byte JBIG2 magic; PDF strips it, probing fails.
// NewDecoderEmbedded skips probing, sets embedded-mode params
// directly: sequential organization, no random access,
// big-endian byte order with small little-endian heuristic on
// first 4 bytes (matches NewDecoderWithGlobals fallback when
// probing fails AND globals non-empty).
//
// Cheap plausibility sniff on first segment header before
// document loop - random ASCII or non-JBIG2 input otherwise
// drives segment parser into long stalls (spec requires no
// specific byte at offset 0, so parser has nothing to
// short-circuit on). Up-front reject = clean error path
// instead of hang.
func NewDecoderEmbedded(r io.Reader, globals []byte) (*Decoder, error) {
	if err := input.CheckGlobals(globals); err != nil {
		return nil, err
	}
	data, err := input.ReadBounded(r)
	if err != nil {
		return nil, err
	}
	if err := probe.SniffEmbeddedJBIG2(data); err != nil {
		return nil, err
	}
	littleEndian := probe.DetectEmbeddedEndianness(data)
	doc := NewDocument(data, globals, false, littleEndian)
	doc.OrgMode = 0
	doc.Grouped = false
	if err := drainGlobals(doc); err != nil {
		return nil, err
	}
	return &Decoder{doc: doc, pageIndex: 0}, nil
}

// NewDecoderWithGlobals creates a decoder using an external
// globals stream.
//
// For PDF-shaped flow where input may carry JBIG2 file header
// (probe.Configs locks on) or be raw embedded segment stream
// (fall back to embedded-mode params, rely on globals for
// referenced symbol dicts). SWF / Flash container scanning
// out of scope; see [NewDecoder] rationale.
func NewDecoderWithGlobals(r io.Reader, globals []byte) (*Decoder, error) {
	if err := input.CheckGlobals(globals); err != nil {
		return nil, err
	}
	data, err := input.ReadBounded(r)
	if err != nil {
		return nil, err
	}
	cfg := probe.Configs(data)
	if cfg == nil {
		if len(globals) == 0 {
			return nil, fmt.Errorf("no valid jbig2 configuration found: %w", ErrMalformed)
		}
		// Embedded fallback: no file header but globals present.
		// Run same preflight sniff [NewDecoderEmbedded] uses
		// so garbage input rejected up front rather than
		// driving segment parser into long stalls.
		if err := probe.SniffEmbeddedJBIG2(data); err != nil {
			return nil, err
		}
		cfg = &probe.Config{
			Data:         data,
			RandomAccess: false,
			OrgMode:      0,
			Grouped:      false,
			LittleEndian: probe.DetectEmbeddedEndianness(data),
		}
	} else {
		if err := probe.RejectUnsupportedOrg(cfg.RandomAccess, cfg.OrgMode); err != nil {
			return nil, err
		}
	}
	doc := NewDocument(cfg.Data, globals, cfg.RandomAccess, cfg.LittleEndian)
	doc.OrgMode = cfg.OrgMode
	doc.Grouped = cfg.Grouped
	if err := drainGlobals(doc); err != nil {
		return nil, err
	}
	return &Decoder{doc: doc, pageIndex: 0}, nil
}

// ParsedGlobals holds a pre-parsed JBIG2 globals stream
// shareable across many [NewDecoderEmbeddedWithGlobals] calls.
// Use when single PDF /JBIG2Globals referenced from multiple
// image XObjects: parse once with [ParseGlobals], bind into
// each image's Decoder.
//
// Read-only after construction. Not safe for concurrent
// decode - bind from single goroutine processing images
// sequentially. Concurrent PDF readers should ParseGlobals
// once per worker, not once per process.
type ParsedGlobals struct {
	doc *Document
}

// ParseGlobals parses a JBIG2 globals stream and returns a
// reusable handle. Empty / nil slice creates a no-globals handle
// (equivalent to passing nil to a non-Parsed constructor).
//
// Bytes are typically the decoded /JBIG2Globals stream object
// referenced from a PDF image XObject's /DecodeParms.
//
// The returned handle retains a reference to the input `globals`
// slice for the lifetime of the handle. Do not mutate the slice
// after this call; callers that need to free the source bytes
// should pass a copy.
//
// Errors wrap [ErrResourceBudget] when the slice exceeds
// [MaxInputBytes]; otherwise parse failures wrap [ErrMalformed].
// Match either sentinel via [errors.Is] to keep the
// budget-vs-malformed distinction downstream callers rely on.
func ParseGlobals(globals []byte) (*ParsedGlobals, error) {
	if len(globals) == 0 {
		return &ParsedGlobals{}, nil
	}
	if err := input.CheckGlobals(globals); err != nil {
		return nil, err
	}
	littleEndian := probe.DetectEmbeddedEndianness(globals)
	g := newGlobalsDocument(globals, littleEndian)
	if err := drainGlobalsDoc(g); err != nil {
		return nil, err
	}
	return &ParsedGlobals{doc: g}, nil
}

// NewDecoderEmbeddedWithGlobals creates a Decoder for a
// PDF-embedded JBIG2 stream sharing pre-parsed globals.
// Equivalent to [NewDecoderEmbedded] but skips per-decode
// globals re-parse - useful when same /JBIG2Globals
// referenced from many image XObjects.
//
// Pass nil globals (or no-op [ParseGlobals](nil)) for
// self-contained streams. Either form produces a Decoder
// supporting [Decoder.Reset] - constructor stamps no-op handle
// so resettable property holds regardless of whether caller
// had globals to bind.
func NewDecoderEmbeddedWithGlobals(r io.Reader, globals *ParsedGlobals) (*Decoder, error) {
	data, err := input.ReadBounded(r)
	if err != nil {
		return nil, err
	}
	if err := probe.SniffEmbeddedJBIG2(data); err != nil {
		return nil, err
	}
	littleEndian := probe.DetectEmbeddedEndianness(data)
	doc := NewDocument(data, nil, false, littleEndian)
	doc.OrgMode = 0
	doc.Grouped = false
	if globals != nil && globals.doc != nil {
		doc.SetGlobalContext(globals.doc)
	}
	// Normalize nil to no-op handle. Decoder.Reset uses
	// resetGlobals == nil as sentinel for 'this Decoder not
	// built by NewDecoderEmbeddedWithGlobals'; without
	// normalization a PDF reader calling constructor with nil
	// globals for self-contained image gets ErrUnsupported on
	// very next Reset.
	if globals == nil {
		globals = &ParsedGlobals{}
	}
	return &Decoder{doc: doc, pageIndex: 0, resetGlobals: globals}, nil
}

// drainGlobals walks document's globalContext (if any) to
// EndReached. Bounded two ways:
//
//   - Stream-position progress. Two consecutive
//     DecodeSequential calls not advancing globals stream =
//     loop aborts. Same defensive bound public Decoder.Decode
//     applies to its own segment loop.
//   - Iteration cap (256). Real globals = handful of
//     symbol-dict segments, max. 256 well above legit; backstops
//     progress check if adversarial bytes still advance stream
//     few bits/call indefinitely. Cap tightened from fuzz
//     coverage finding slow-decode shapes within larger
//     budgets; actual cap in [drainGlobalsDoc] uses this value.
func drainGlobals(doc *Document) error {
	return drainGlobalsDoc(doc.GlobalContextDoc())
}

// drainGlobalsDoc runs same drain loop directly on a
// globals-only Document - used by [ParseGlobals] for
// pre-parsed-globals reuse path. Pass nil for "no globals".
func drainGlobalsDoc(g *Document) error {
	if g == nil {
		return nil
	}
	// 256 well above real globals stream segment count
	// (typical: 1-5 symbol-dict) but tight enough that
	// adversarial input can't burn multi-second wall clock
	// before drainGlobals returns. Tightened from larger prior
	// bound after fuzz found shapes decoding legal segments
	// slowly inside it.
	const maxIter = 256
	prevProgress := g.Progress()
	stalled := 0
	for i := 0; i < maxIter; i++ {
		res := g.DecodeSequential()
		if res == ResultEndReached {
			return nil
		}
		if res == ResultFailure {
			return fmt.Errorf("failed to parse global segments: %w", ErrMalformed)
		}
		p := g.Progress()
		if p == prevProgress {
			stalled++
			if stalled >= 2 {
				return fmt.Errorf("globals decode made no progress (likely malformed globals): %w", ErrMalformed)
			}
		} else {
			stalled = 0
			prevProgress = p
		}
	}
	return fmt.Errorf("globals decode loop exceeded iteration cap (likely malformed globals): %w", ErrMalformed)
}

// Decode decodes the next page.
//
// Equivalent to [Decoder.DecodeContext] with
// [context.Background]. Use DecodeContext when canceling a
// long-running decode (e.g. via [context.WithTimeout]).
//
// Internal loop tracks progress via [Document.Progress]
// (stream-cursor advances OR grouped-mode index advances) so
// adversarial input driving [Document.DecodeSequential] into
// non-terminal state without forward motion can't hang decoder.
// Second consecutive call not advancing progress token aborts
// as malformed - same defensive bound global-segments loop
// applies in [drainGlobals].
func (d *Decoder) Decode() (image.Image, error) {
	return d.DecodeContext(context.Background())
}

// DecodeContext decodes the next page, honoring ctx for
// cancellation. nil ctx treated as [context.Background]. On
// cancel, error wraps `ctx.Err()`; check via
// `errors.Is(err, context.Canceled)` or
// `errors.Is(err, context.DeadlineExceeded)`.
//
// Cancellation checked between segments inside segment-parser
// loop, so latency bounded by cost of one segment (itself
// capped by per-region [Limits]).
//
// Peak-memory note: on success, packed 1-bpp page bitmap
// converted to dense `*image.Gray` (one byte/pixel) before
// packed released. Short window both representations live;
// gray copy ~8x packed footprint. 256-megapixel page (default
// `MaxImagePixels`) = ~32 MiB packed + ~256 MB gray = peak
// ~288 MiB during conversion, driven almost entirely by gray
// output. Plan `runtime/debug.SetMemoryLimit` or per-call
// wall-clock budgets around peak, not steady-state packed.
func (d *Decoder) DecodeContext(ctx context.Context) (image.Image, error) {
	p, err := d.decodePageContext(ctx)
	if err != nil {
		return nil, err
	}
	img := p.ToGoImage()
	d.doc.ReleasePageSegments(d.pageIndex)
	return img, nil
}

// decodePageContext drives the segment-parser loop until a
// page completes (or end-of-file / failure / stall), returning
// the just-completed packed page bitmap. Caller chooses what
// to do with it: [Decoder.DecodeContext] hands it to
// [page.Image.ToGoImage] for the public *image.Gray API;
// [Decoder.DecodePackedContext] returns the packed bytes
// directly. Either way the caller must call
// `d.doc.ReleasePageSegments(d.pageIndex)` after copying or
// snapshotting the bitmap before the next decode call clobbers
// it.
//
// Cancellation, stall detection, and failure-path packed-page
// cleanup match the contract documented on
// [Decoder.DecodeContext]; both wrappers inherit it through
// this helper.
func (d *Decoder) decodePageContext(ctx context.Context) (*page.Image, error) {
	if d.doc == nil {
		return nil, errors.New("decoder not initialized")
	}
	// Drop sticky-error stash so retry after prior failure
	// records own cause instead of re-reporting first.
	// In-page flag reset by ReleaseCurrentPageBitmap on failure
	// paths below, so successful page handoffs leave
	// multi-page state intact.
	d.doc.ClearLastErr()
	d.doc.SetContext(ctx)
	// Clear bound context on return so subsequent decode
	// without ctx isn't bound to stale one. context.Background()
	// is documented "no cancellation" stand-in; contextcheck
	// flags it as not-derived because we intentionally break
	// the parent chain on cleanup.
	defer d.doc.SetContext(context.Background()) //nolint:contextcheck
	prevProgress := d.doc.Progress()
	stalled := 0
	for {
		res := d.doc.DecodeSequential()
		if res == ResultEndReached {
			if d.doc.InPage() && d.doc.Page() != nil {
				d.doc.SetInPage(false)
				d.pageIndex++
				return d.doc.Page(), nil
			}
			return nil, io.EOF
		}
		if res == ResultPageCompleted {
			if d.doc.Page() == nil {
				return nil, fmt.Errorf("page completed but no image found: %w", ErrMalformed)
			}
			d.pageIndex++
			return d.doc.Page(), nil
		}
		if res == ResultFailure {
			// Drop in-progress packed page bitmap before
			// returning terminal error. On success
			// ReleasePageSegments already runs; on failure,
			// caller retaining Decoder for diagnostic inspection
			// would otherwise keep MaxImagePixels-sized
			// allocation alive past abort. Segment list (and
			// global context) stays intact so failure
			// inspection still works.
			d.doc.ReleaseCurrentPageBitmap()
			if err := d.doc.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("decoding failed: %w", ErrMalformed)
		}
		p := d.doc.Progress()
		if p == prevProgress {
			stalled++
			if stalled >= 2 {
				// Same packed-page cleanup as the
				// ResultFailure branch above.
				d.doc.ReleaseCurrentPageBitmap()
				return nil, fmt.Errorf("decoding made no progress (likely malformed input): %w", ErrMalformed)
			}
		} else {
			stalled = 0
			prevProgress = p
		}
	}
}

// PackedPage is a 1-bit-per-pixel page bitmap in MSB-first
// packed bytes. Returned by [Decoder.DecodePacked] for callers
// that consume the bilevel data directly - PBM writers, 1-bpp
// PNG encoders, bit-blit pipelines - without paying for the
// 8-bpp *image.Gray conversion [Decoder.Decode] performs
// (~12 ms + ~35 MB alloc on a 600 dpi A4 page).
//
// Pixel layout: row r starts at Data[r*Stride]. Within a byte,
// bit 7 (MSB) is the leftmost pixel; ink = 1, paper = 0. Same
// polarity and packing as PBM (P4) and as the bytes a
// /JBIG2Decode filter delivers.
//
// Data aliases the Decoder's internal page buffer. It stays
// valid until the next Decode / DecodeContext / DecodePacked /
// DecodePackedContext / Reset call on the same Decoder; copy
// it before that boundary if you need it to outlive the
// Decoder's next operation.
type PackedPage struct {
	// Data is the packed bitmap. Aliases the Decoder's internal
	// page buffer; see PackedPage doc for lifetime.
	Data []byte
	// Width is the page width in pixels.
	Width int
	// Height is the page height in pixels.
	Height int
	// Stride is the byte offset between consecutive rows
	// (== ceil(Width/8)).
	Stride int
}

// DecodePacked is [Decoder.Decode] for bilevel-aware consumers.
// Returns the packed internal bitmap directly via [PackedPage],
// skipping the *image.Gray conversion that [Decoder.Decode]
// performs. On a 600 dpi A4 page that saves ~12 ms wall + ~35
// MB allocation; downstream PBM / 1-bpp-PNG writers want the
// packed form anyway.
//
// Equivalent to [Decoder.DecodePackedContext] with
// [context.Background].
func (d *Decoder) DecodePacked() (PackedPage, error) {
	return d.DecodePackedContext(context.Background())
}

// DecodePackedContext is [Decoder.DecodeContext] for bilevel-
// aware consumers - same cancellation contract, returns a
// [PackedPage] instead of an *image.Gray. See [PackedPage] for
// byte layout and aliasing lifetime.
func (d *Decoder) DecodePackedContext(ctx context.Context) (PackedPage, error) {
	p, err := d.decodePageContext(ctx)
	if err != nil {
		return PackedPage{}, err
	}
	pp := PackedPage{
		Data:   p.Data(),
		Width:  int(p.Width()),
		Height: int(p.Height()),
		Stride: int(p.Stride()),
	}
	// The PackedPage.Data slice keeps the underlying byte
	// array reachable; setting d.doc.page = nil only drops the
	// *page.Image. Caller's PackedPage outlives the release.
	d.doc.ReleasePageSegments(d.pageIndex)
	return pp, nil
}

// DecodeAll decodes all remaining pages.
//
// Equivalent to [Decoder.DecodeAllContext] with
// [context.Background].
func (d *Decoder) DecodeAll() ([]image.Image, error) {
	return d.DecodeAllContext(context.Background())
}

// DecodeAllContext decodes all remaining pages, honoring ctx
// for cancellation. nil ctx treated as [context.Background].
// Partial results kept on cancel; returned error is first
// failure encountered, wrapping `ctx.Err()` on cancel. Check
// via `errors.Is(err, context.Canceled)` or
// `errors.Is(err, context.DeadlineExceeded)`.
//
// Context consulted between every segment of every page; see
// [Decoder.DecodeContext] for per-page cancellation contract.
//
// Memory note: each decoded page appended to returned slice,
// held in caller memory until slice out of scope. Each entry
// 8-bpp `*image.Gray` (one byte/pixel), so 100-page document
// of 8-megapixel pages keeps ~800 MiB alive simultaneously.
// Internal packed page bitmap released after each page (see
// [Decoder.DecodeContext]), so gray slice is only per-page
// retention but unbounded by [Limits]. Prefer
// [Decoder.DecodeContext] in loop, processing/writing each
// page before requesting next, when document size is large or
// attacker-controlled.
func (d *Decoder) DecodeAllContext(ctx context.Context) ([]image.Image, error) {
	var images []image.Image
	for {
		img, err := d.DecodeContext(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return images, err
		}
		images = append(images, img)
	}
	return images, nil
}

// GetDocument returns the underlying document.
//
// Stability: advanced / unstable. Returned *Document is
// internal parse orchestrator Decoder owns. Exposed so bundled
// `--inspect` tool and low-level callers can walk segment
// metadata, but method surface is internal/segment and not
// part of gobig2's public API contract. In particular:
//
//   - Calling DecodeSequential, SetContext, ReleasePageSegments,
//     or any state-mutating method directly is unsupported
//     while parent Decoder still in use. Mixing those with
//     Decode / DecodeContext yields undefined behavior.
//   - Exposed type, fields, and methods may change between
//     versions without deprecation cycle.
//
// Treat result as read-only inspection handle. Use for
// segment-table dumps and similar tooling; route every decode
// through Decoder.
func (d *Decoder) GetDocument() *Document {
	return d.doc
}

// Reset reinitializes Decoder for a new PDF-embedded JBIG2
// stream while keeping previously bound [ParsedGlobals]
// attached. Hot path for PDF reader iterating over image
// XObjects sharing a /JBIG2Globals: parse once, build one
// Decoder with [NewDecoderEmbeddedWithGlobals], then Reset
// between images instead of re-allocating fresh Decoder +
// re-parsing globals.
//
// Reset returns [ErrUnsupported] when called on Decoder not
// built via [NewDecoderEmbeddedWithGlobals]; re-invoke those
// constructors instead.
//
// Errors wrap [ErrMalformed] when r yields non-JBIG2 input.
func (d *Decoder) Reset(r io.Reader) error {
	if d.resetGlobals == nil {
		return fmt.Errorf("Decoder.Reset: only supported on Decoders built with NewDecoderEmbeddedWithGlobals: %w", ErrUnsupported)
	}
	data, err := input.ReadBounded(r)
	if err != nil {
		return err
	}
	if err := probe.SniffEmbeddedJBIG2(data); err != nil {
		return err
	}
	littleEndian := probe.DetectEmbeddedEndianness(data)
	doc := NewDocument(data, nil, false, littleEndian)
	doc.OrgMode = 0
	doc.Grouped = false
	if d.resetGlobals.doc != nil {
		doc.SetGlobalContext(d.resetGlobals.doc)
	}
	d.doc = doc
	d.pageIndex = 0
	return nil
}

// Decode decodes the first page in the JBIG2 data.
//
// Equivalent to [DecodeContext] with [context.Background].
func Decode(r io.Reader) (image.Image, error) {
	dec, err := NewDecoder(r)
	if err != nil {
		return nil, err
	}
	return dec.Decode()
}

// DecodeContext decodes the first page in JBIG2 data, honoring
// ctx for cancellation. nil ctx treated as
// [context.Background]. On cancel, error wraps `ctx.Err()`.
//
// Convenience wrapper: `NewDecoder(r)` then
// `Decoder.DecodeContext(ctx)`. Use explicit [Decoder] form
// when reading [Decoder.GetDocument] or calling
// [Decoder.DecodeAllContext] for multi-page input.
//
// Scope of ctx. Supplied context bounds segment parsing after
// NewDecoder has read input from r - constructor slurps bytes
// through bounded [io.LimitedReader] first, so cancellation NOT
// observed during initial read. For network or slow-reader
// sources, apply deadlines at io.Reader / request layer
// (e.g. http.Request.Context) too, not just at this call.
func DecodeContext(ctx context.Context, r io.Reader) (image.Image, error) {
	dec, err := NewDecoder(r)
	if err != nil {
		return nil, err
	}
	return dec.DecodeContext(ctx)
}

// DecodeAll decodes every remaining page in JBIG2 data, returns
// in order. Partial results kept on failure.
//
// Equivalent to [DecodeAllContext] with [context.Background].
func DecodeAll(r io.Reader) ([]image.Image, error) {
	dec, err := NewDecoder(r)
	if err != nil {
		return nil, err
	}
	return dec.DecodeAll()
}

// DecodeAllContext decodes every remaining page, honoring ctx
// for cancellation. nil ctx treated as [context.Background].
// Partial results kept on cancel; error wraps `ctx.Err()` on
// cancel.
//
// Convenience wrapper: `NewDecoder(r)` then
// `Decoder.DecodeAllContext(ctx)`.
func DecodeAllContext(ctx context.Context, r io.Reader) ([]image.Image, error) {
	dec, err := NewDecoder(r)
	if err != nil {
		return nil, err
	}
	return dec.DecodeAllContext(ctx)
}

// DecodeConfig returns the JBIG2 image configuration.
//
// Convenience wrapper around [DecodeConfigContext] with
// [context.Background]. stdlib's `image.RegisterFormat` hook
// calls this signature; server callers wanting request-scoped
// cancellation should use [DecodeConfigContext] directly.
func DecodeConfig(r io.Reader) (image.Config, error) {
	return DecodeConfigContext(context.Background(), r)
}

// DecodeConfigContext returns the JBIG2 image configuration,
// honoring ctx for cancellation between segments.
//
// Standalone streams only. Calls [NewDecoder] internally,
// requiring T.88 Annex E file-header magic; PDF-embedded
// /JBIG2Decode streams omit header and fail with ErrMalformed.
// PDF readers wanting page-info from embedded stream should
// build Decoder via [NewDecoderEmbedded] /
// [NewDecoderEmbeddedWithGlobals] and read
// [Document.PageInfoList] after first [Decoder.DecodeContext].
//
// First page only. Returned config = dimensions of first
// page-info segment seen. Standalone files may declare more
// pages with different dimensions; consumers needing per-page
// config iterate decoder and inspect each page's bounds after
// decoding.
//
// Loop safeguards mirror [Decoder.DecodeContext]: a
// [Document.Progress] stall guard so adversarial input
// looping DecodeSequential without forward motion can't hang
// probe. Resource-budget rejections during probe preserve
// ErrResourceBudget classification, not collapse to generic
// ErrMalformed wrap.
//
// Scope of ctx. Like package-level [DecodeContext], supplied
// ctx bounds segment parsing after [NewDecoder] has read input
// from r - bounded io.LimitedReader inside constructor doesn't
// observe cancellation. For network or slow-reader sources,
// apply deadlines at io.Reader / request layer too.
func DecodeConfigContext(ctx context.Context, r io.Reader) (image.Config, error) {
	dec, err := NewDecoder(r)
	if err != nil {
		return image.Config{}, err
	}
	dec.doc.SetContext(ctx)
	//nolint:contextcheck // Same intentional break-the-chain
	// pattern Decoder.DecodeContext uses: clear bound context
	// on return so later decode against same Document isn't
	// bound to stale ctx.
	defer dec.doc.SetContext(context.Background())

	prevProgress := dec.doc.Progress()
	stalled := 0
	for {
		if pi := dec.doc.PageInfoList(); len(pi) > 0 {
			info := pi[0]
			return image.Config{
				ColorModel: color.GrayModel,
				Width:      int(info.Width),
				Height:     int(info.Height),
			}, nil
		}
		res := dec.doc.DecodeSequential()
		if res == ResultEndReached {
			break
		}
		if res == ResultFailure {
			// Preserve underlying classification segment parser
			// stamped via failf - same pattern DecodeContext
			// uses. Falling through to bare ErrMalformed wrap
			// loses ErrResourceBudget / ErrUnsupported /
			// context-cancel when caps fire during page-info
			// probe.
			if err := dec.doc.Err(); err != nil {
				return image.Config{}, err
			}
			return image.Config{}, fmt.Errorf("decoding failed while looking for config: %w", ErrMalformed)
		}
		p := dec.doc.Progress()
		if p == prevProgress {
			stalled++
			if stalled >= 2 {
				return image.Config{}, fmt.Errorf("config probe made no progress (likely malformed input): %w", ErrMalformed)
			}
		} else {
			stalled = 0
			prevProgress = p
		}
	}
	return image.Config{}, fmt.Errorf("page information not found: %w", ErrMalformed)
}

// init wires codec into stdlib image package so callers can use
// image.Decode / image.DecodeConfig transparently.
//
// Side-effect-on-import scope:
//
//   - Importing gobig2 registers "jbig2" with
//     image.RegisterFormat under standalone JBIG2 magic
//     (T.88 Annex E: 97 4A 42 32 0D 0A 1A 0A). Process-wide
//     global state; no opt-out. Embedders importing multiple
//     JBIG2 implementations: image.Decode walks registered
//     formats in append order, first matching prefix wins -
//     whichever decoder's package init runs first wins for
//     that exact magic.
//   - Magic only matches standalone .jb2 / .jbig2 files.
//     PDF-embedded streams (/JBIG2Decode filter bytes) lack
//     file-header magic and will NOT route through
//     image.Decode - use explicit NewDecoderEmbedded /
//     NewDecoderEmbeddedWithGlobals constructors.
func init() {
	image.RegisterFormat("jbig2", "\x97\x4A\x42\x32\x0D\x0A\x1A\x0A", Decode, DecodeConfig)
}

// ToGoImage is defined on *page.Image (see internal/page/image.go);
// the Image alias above inherits it for the public API.
