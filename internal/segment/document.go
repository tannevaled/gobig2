package segment

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/generic"
	"github.com/tannevaled/gobig2/internal/halftone"
	"github.com/tannevaled/gobig2/internal/huffman"
	"github.com/tannevaled/gobig2/internal/intmath"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/refinement"
	"github.com/tannevaled/gobig2/internal/state"
	"github.com/tannevaled/gobig2/internal/symbol"
)

// MaxSymbolsPerDict caps SDNUMNEWSYMS / SDNUMEXSYMS at parse
// time. Symbol-dict segments declare arbitrary uint32 counts
// that drive `make([]*page.Image, n)` (8 GB at uint32 max) and
// `make([]arith.Ctx, 1<<ceil(log2(n)))` (4 GB+). 4G-symbol
// inputs OOM before any decode. 1 M well above real docs
// (largest seen: a few thousand) and 1 M x 8 B = 8 MB max
// pointer slice. 0 disables; default caps pre-decode budget
// to ~16 MB across symbol-dict allocations.
//
// Aggregate use: same cap applied to SBSYMS / SDINSYMS
// input-symbol pools from referenced segments; without that,
// chained dicts grow SBSYMS unbounded. Corpus fixtures top
// out at 308 aggregate input symbols.
var MaxSymbolsPerDict uint32 = DefaultMaxSymbolsPerDict

// DefaultMaxSymbolsPerDict is the stock cap for [MaxSymbolsPerDict].
const DefaultMaxSymbolsPerDict uint32 = 1 << 20

// MaxBytesPerSegment caps the per-segment DataLength field
// (4-byte length declaring payload bytes). Adversarial input
// sets 0xFFFFFFFE (largest non-sentinel) to claim 4 GB per
// segment; per-Decode and per-region caps bound downstream
// allocation, but rejecting the declared length up front
// fails fast on adversarial shapes.
//
// Real segments rarely exceed a few MB on the wire (1200-DPI
// A4 generic-region: 100-500 KB MMR; halftones: 1-10 MB).
// 16 MB default generous for real use, well below 4 GB worst
// case.
//
// 0xFFFFFFFF is the spec's "unknown length" streaming sentinel
// (T.88 §7.2.7) and exempt - parser advances past these without
// trusting the field. Set to 0 to disable.
var MaxBytesPerSegment uint64 = DefaultMaxBytesPerSegment

// DefaultMaxBytesPerSegment is the stock cap for [MaxBytesPerSegment].
const DefaultMaxBytesPerSegment uint64 = 16 * 1024 * 1024

// MaxPixelsPerByte caps ratio of declared page-info
// `width x height` to total input bytes. Adversarial inputs
// declare 12K x 12K (~152 MP) from 30 bytes - under
// MaxImagePixels but ~19 MB zero-fill plus full
// generic-region template loop.
//
// Real fixtures can exceed 30,000 pixels-per-byte for
// mostly-blank pages (bundled testdata/pdf-embedded/sample.jb2
// is 94 bytes declaring ~3 MP = ratio 32,247). Default 1 M
// leaves 30x margin above highest fixture and rejects the
// 30-byte-to-152-MP shape (ratio ~5 M) in microseconds.
//
// Fires only at page-info parse time; per-region allocations
// gated by [page.MaxImagePixels] / [symbol.MaxSymbolPixels].
// 0 disables.
var MaxPixelsPerByte uint64 = DefaultMaxPixelsPerByte

// DefaultMaxPixelsPerByte is the stock cap for [MaxPixelsPerByte].
const DefaultMaxPixelsPerByte uint64 = 1 << 20

// Result is a parse result code.
type Result int

const (
	// ResultSuccess indicates success.
	ResultSuccess Result = 0
	// ResultFailure indicates failure.
	ResultFailure Result = 1
	// ResultEndReached indicates the end of input was reached.
	ResultEndReached Result = 2
	// ResultDecodeToBeContinued indicates decoding should continue.
	ResultDecodeToBeContinued Result = 3
	// ResultPageCompleted indicates a page is complete.
	ResultPageCompleted Result = 4
)

// Document is the document parsing context.
type Document struct {
	stream        *bio.BitStream
	globalContext *Document
	segmentList   []*Segment
	page          *page.Image
	pageInfoList  []*PageInfo
	segment       *Segment
	offset        uint32
	inPage        bool
	randomAccess  bool
	isGlobal      bool
	Grouped       bool
	OrgMode       int
	// pageVirgin tracks whether the current page bitmap is
	// still in its post-NewImage zero state - true after
	// parsePageInfo when DefaultPixelValue is 0, cleared after
	// the first region compose (or in-place decode). Lets
	// parseGenericRegion decode a full-page region straight
	// into d.page when conditions allow, skipping the temp
	// bitmap + ComposeFrom byte walk (35 Mpx on 600 dpi A4).
	pageVirgin bool
	// lastErr records most recent failure with full context.
	// Parser Result enum carries pass/fail only; threading real
	// error through every site would touch every parser, so
	// document accumulates here; public Decoder reads on
	// ResultFailure.
	lastErr error
	// Grouped-mode resume state. Grouped layout: header-collection
	// phase, then data-walk phase. Public DecodeContext expects
	// per-page returns, so when data walk hits end-of-page/-of-file
	// we hand result up and remember resume position.
	groupedHeadersParsed bool
	groupedDataIdx       int
	groupedDataOffset    uint32
	// ctx is decode-loop cancellation context. Set via
	// [Document.SetContext]; checked between segments in
	// [Document.DecodeSequential]. nil treated as
	// [context.Background].
	ctx context.Context
}

// SetContext installs a cancellation context. Decode loop in
// [Document.DecodeSequential] checks `ctx.Err()` between
// segments and aborts as failure on cancel. nil clears
// (equivalent to [context.Background]).
func (d *Document) SetContext(ctx context.Context) {
	if d == nil {
		return
	}
	d.ctx = ctx
}

// Err returns the most recent error captured during decoding,
// or nil. Result-based parser signatures lose context, so
// failure sites stash formatted error here; public Decode
// reads it back.
func (d *Document) Err() error {
	if d == nil {
		return nil
	}
	return d.lastErr
}

// failf records a wrapped error and returns ResultFailure so
// callers write `return d.failf("parseFoo: %w", err)` instead
// of bare `return ResultFailure` (throws away context). First
// error wins - later unwinding failures don't overwrite root.
//
// Use [Document.failfMalformed] for structural/parse failures
// classifying as [errs.ErrMalformed]; failf doesn't auto-wrap
// a sentinel, so format without %w + sentinel loses public
// classification.
func (d *Document) failf(format string, args ...any) Result {
	if d != nil && d.lastErr == nil {
		d.lastErr = fmt.Errorf(format, args...)
	}
	return ResultFailure
}

// failfMalformed records a structural/parse failure with
// [errs.ErrMalformed] auto-wrapped. Use at every site that
// surfaces malformed-input - without it, parser records
// sentinel-less error and errors.Is(err, gobig2.ErrMalformed)
// returns false. Format/args describe failure without trailing
// ": %w" or sentinel.
func (d *Document) failfMalformed(format string, args ...any) Result {
	return d.failf(format+": %w", append(args, errs.ErrMalformed)...)
}

// classifyLeafErr wraps err with errs.ErrMalformed unless err
// already carries a sentinel (ErrResourceBudget /
// ErrUnsupported / ErrMalformed). Used by region parsers whose
// decode helpers return classified budget errors
// (errGenericOversize, errMMROversize, refinement oversize/alloc)
// or unclassified stream-corruption needing ErrMalformed for
// CLI exit-code classification.
//
// Without wrap, malformed MMR stream (DecodeMMR deliberately
// un-sentinel'd to distinguish from budget rejections)
// propagates through failf as plain error. CLI's classifyExit
// falls through to exit 1 (generic) instead of exit 3 (malformed).
func classifyLeafErr(err error) error {
	if errors.Is(err, errs.ErrResourceBudget) ||
		errors.Is(err, errs.ErrUnsupported) ||
		errors.Is(err, errs.ErrMalformed) {
		return err
	}
	return fmt.Errorf("%w: %w", errs.ErrMalformed, err)
}

// expandPageForStripedRegion grows d.page to fit a region
// whose bottom (ri.Y + ri.Height) exceeds current page height.
// int64 sum so negative ri.Y or positive Y near MaxInt32 can't
// wrap; naive `uint32(ri.Y) + uint32(ri.Height)` reinterprets
// negative Y as huge uint32 and forwards wrapped value to
// Expand's int32 param.
//
// Off-top regions (bottom <= height) no-op by design; bottoms
// past MaxInt32 unrepresentable in int32 height and also no-op
// (page.Image.Expand rejects them; failing fast here avoids
// wrapped-int32 cast). Called by all four region parsers
// (text/halftone/generic/refinement).
func expandPageForStripedRegion(p *page.Image, ri RegionInfo, defaultPixel bool) {
	if p == nil {
		return
	}
	bottom := int64(ri.Y) + int64(ri.Height)
	if bottom <= int64(p.Height()) {
		return
	}
	if bottom > math.MaxInt32 {
		return
	}
	p.Expand(int32(bottom), defaultPixel)
}

// GetSegments returns the segment list.
// Returns: []*Segment the segments.
func (d *Document) GetSegments() []*Segment {
	return d.segmentList
}

// PageInfo holds the page-info fields.
type PageInfo struct {
	Width             uint32
	Height            uint32
	ResolutionX       uint32
	ResolutionY       uint32
	DefaultPixelValue bool
	IsStriped         bool
	MaxStripeSize     uint16
}

// NewDocument creates a document.
// Parameters: data the bitstream bytes, globalData the global-segment bytes, randomAccess whether random-access mode is used, littleEndian whether little-endian byte order is used.
// Returns: *Document the document.
func NewDocument(data, globalData []byte, randomAccess, littleEndian bool) *Document {
	stream := bio.NewBitStream(data, 0)
	stream.SetLittleEndian(littleEndian)
	doc := &Document{
		stream:       stream,
		randomAccess: randomAccess,
	}
	if len(globalData) > 0 {
		doc.globalContext = &Document{
			stream:   bio.NewBitStream(globalData, 0),
			isGlobal: true,
		}
	}
	return doc
}

// NewGlobalsDocument creates a globals-only document. Returned
// Document holds the globals stream; expected to be drained by
// public-side helper before attachment via
// [Document.SetGlobalContext].
func NewGlobalsDocument(globalData []byte, littleEndian bool) *Document {
	stream := bio.NewBitStream(globalData, 0)
	stream.SetLittleEndian(littleEndian)
	return &Document{
		stream:   stream,
		isGlobal: true,
	}
}

// SetGlobalContext attaches a pre-parsed globals document to d.
// Used by public-side ParsedGlobals reuse path. Concurrent
// decode across parents sharing one globals Document not
// supported - bind globals once per worker goroutine.
func (d *Document) SetGlobalContext(g *Document) {
	if d == nil {
		return
	}
	d.globalContext = g
}

// ParseSegmentHeader parses a segment header.
// Parameters: segment the segment to populate.
// Returns: Result the parse result.
//
// Failure-classification: first read (segment number) is the
// "clean EOF" boundary. Truncated read there is a quiet loop
// break (no failf) so DecodeSequential/decodeGrouped detect
// end-of-stream on trailing-byte tail. Every later read or
// semantic check (flags, ref-count word, ref-count > 1024,
// ref-segment numbers, sequential forward/self refs,
// page-assoc, data-length) routes through failf with
// ErrMalformed so failures surface as classified errors, not
// silent clean EOF.
func (d *Document) ParseSegmentHeader(segment *Segment) Result {
	if d.OrgMode == 1 || !d.randomAccess {
		// Segment-number read = only quiet-EOF boundary. Caller
		// sees ResultFailure with d.lastErr nil = end of stream.
		if val, err := d.stream.ReadInteger(); err != nil {
			return ResultFailure
		} else {
			segment.Number = val
		}
	} else {
		segment.Number = 0
	}
	var flags byte
	if val, err := d.stream.Read1Byte(); err != nil {
		return d.failf("ParseSegmentHeader: segment #%d truncated at flags byte: %w", segment.Number, errs.ErrMalformed)
	} else {
		flags = val
	}
	segment.Flags.Type = flags & 0x3F
	segment.Flags.PageAssociationSize = (flags & 0x40) != 0
	segment.Flags.DeferredNonRetain = (flags & 0x80) != 0
	cTemp := d.stream.GetCurByte()
	if (cTemp >> 5) == 7 {
		var count uint32
		if val, err := d.stream.ReadInteger(); err != nil {
			return d.failf("ParseSegmentHeader: segment #%d truncated at long-form ref-count: %w", segment.Number, errs.ErrMalformed)
		} else {
			count = val
		}
		count &= 0x1FFFFFFF
		segment.ReferredToSegmentCount = int32(count)
		if segment.ReferredToSegmentCount > 1024 {
			return d.failf("ParseSegmentHeader: segment #%d long-form ref-count %d exceeds 1024 cap: %w",
				segment.Number, segment.ReferredToSegmentCount, errs.ErrMalformed)
		}
		retentionBits := segment.ReferredToSegmentCount + 1
		bytesToSkip := (retentionBits + 7) / 8
		d.stream.AddOffset(uint32(bytesToSkip))
	} else {
		if val, err := d.stream.Read1Byte(); err != nil {
			return d.failf("ParseSegmentHeader: segment #%d truncated at short-form refByte: %w", segment.Number, errs.ErrMalformed)
		} else {
			cTemp = val
		}
		segment.ReferredToSegmentCount = int32(cTemp >> 5)
	}
	cSSize := 1
	if segment.Number > 65536 {
		cSSize = 4
	} else if segment.Number > 256 {
		cSSize = 2
	}
	cPSize := 1
	if segment.Flags.PageAssociationSize {
		cPSize = 4
	}
	if segment.ReferredToSegmentCount > 0 {
		segment.ReferredToSegmentNumbers = make([]uint32, segment.ReferredToSegmentCount)
		for i := int32(0); i < segment.ReferredToSegmentCount; i++ {
			switch cSSize {
			case 1:
				if val, err := d.stream.Read1Byte(); err != nil {
					return d.failf("ParseSegmentHeader: segment #%d truncated at ref[%d] (1-byte): %w", segment.Number, i, errs.ErrMalformed)
				} else {
					segment.ReferredToSegmentNumbers[i] = uint32(val)
				}
			case 2:
				if val, err := d.stream.ReadShortInteger(); err != nil {
					return d.failf("ParseSegmentHeader: segment #%d truncated at ref[%d] (2-byte): %w", segment.Number, i, errs.ErrMalformed)
				} else {
					segment.ReferredToSegmentNumbers[i] = uint32(val)
				}
			case 4:
				if val, err := d.stream.ReadInteger(); err != nil {
					return d.failf("ParseSegmentHeader: segment #%d truncated at ref[%d] (4-byte): %w", segment.Number, i, errs.ErrMalformed)
				} else {
					segment.ReferredToSegmentNumbers[i] = val
				}
			}
			if !d.randomAccess && segment.ReferredToSegmentNumbers[i] >= segment.Number {
				return d.failf("ParseSegmentHeader: segment #%d ref[%d]=%d is self/forward in sequential mode: %w",
					segment.Number, i, segment.ReferredToSegmentNumbers[i], errs.ErrMalformed)
			}
		}
	}
	if d.OrgMode == 1 || !d.randomAccess {
		if cPSize == 1 {
			if val, err := d.stream.Read1Byte(); err != nil {
				return d.failf("ParseSegmentHeader: segment #%d truncated at page-assoc (1-byte): %w", segment.Number, errs.ErrMalformed)
			} else {
				segment.PageAssociation = uint32(val)
			}
		} else {
			if val, err := d.stream.ReadInteger(); err != nil {
				return d.failf("ParseSegmentHeader: segment #%d truncated at page-assoc (4-byte): %w", segment.Number, errs.ErrMalformed)
			} else {
				segment.PageAssociation = val
			}
		}
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return d.failf("ParseSegmentHeader: segment #%d truncated at data-length: %w", segment.Number, errs.ErrMalformed)
	} else {
		segment.DataLength = val
	}
	// Defense-in-depth on per-segment data-length cap.
	// 0xFFFFFFFF = "unknown length" streaming sentinel; parser
	// handles specially in DecodeSequential (advances past
	// segment without trusting field), let it through.
	if MaxBytesPerSegment > 0 &&
		segment.DataLength != 0xFFFFFFFF &&
		uint64(segment.DataLength) > MaxBytesPerSegment {
		return d.failf("ParseSegmentHeader: segment %d declares DataLength=%d > MaxBytesPerSegment=%d: %w",
			segment.Number, segment.DataLength, MaxBytesPerSegment, errs.ErrResourceBudget)
	}
	segment.Key = d.stream.GetKey()
	segment.DataOffset = d.stream.GetOffset()
	segment.State = state.DataUnparsed
	return ResultSuccess
}

// FindSegmentByNumber locates a segment by number.
// Parameters: number the segment number.
// Returns: *Segment the matching segment, or nil if not found.
//
// Collision policy: T.88 §7.2.2 requires distinct segment
// numbers, so a number in both globalContext and local
// segmentList is malformed. Lookup walks globals first: global
// wins, local shadowed. Keeps globals stable across embedded
// image streams; treats local duplicates as malformed instead
// of overriding shared state.
func (d *Document) FindSegmentByNumber(number uint32) *Segment {
	if d.globalContext != nil {
		if seg := d.globalContext.FindSegmentByNumber(number); seg != nil {
			return seg
		}
	}
	for _, seg := range d.segmentList {
		if seg.Number == number {
			return seg
		}
	}
	return nil
}

// ParseSegmentData parses a segment's data section.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) ParseSegmentData(segment *Segment) Result {
	res := d.parseSegmentDataInner(segment)
	if res == ResultFailure && d.lastErr == nil {
		// Inner parsers returning ResultFailure without lastErr
		// leave callers blind. Stamp minimum attribution before
		// bubbling (segment type/number/offset + ErrMalformed
		// sentinel so CLI picks exit 3 not generic exit 1).
		// Fires on truncated headers; less often as parsers
		// migrate to failf.
		d.lastErr = fmt.Errorf("segment #%d type %d at offset %d: parse failed: %w",
			segment.Number, segment.Flags.Type, d.offset, errs.ErrMalformed)
	}
	return res
}

// parseSegmentDataInner is the plain dispatch table;
// ParseSegmentData wraps it so every ResultFailure carries context.
func (d *Document) parseSegmentDataInner(segment *Segment) Result {
	switch segment.Flags.Type {
	case 0:
		return d.parseSymbolDict(segment)
	case 4, 6, 7:
		if !d.inPage {
			return d.failfMalformed("text region (type %d) seen outside a page", segment.Flags.Type)
		}
		return d.parseTextRegion(segment)
	case 16:
		return d.parsePatternDict(segment)
	case 20, 22, 23:
		if !d.inPage {
			return d.failfMalformed("halftone region (type %d) seen outside a page", segment.Flags.Type)
		}
		return d.parseHalftoneRegion(segment)
	case 36, 38, 39:
		if !d.inPage {
			return d.failfMalformed("generic region (type %d) seen outside a page", segment.Flags.Type)
		}
		return d.parseGenericRegion(segment)
	case 40, 42, 43:
		if !d.inPage {
			return d.failfMalformed("refinement region (type %d) seen outside a page", segment.Flags.Type)
		}
		return d.parseGenericRefinementRegion(segment)
	case 48:
		return d.parsePageInfo(segment)
	case 49:
		d.inPage = false
		return ResultPageCompleted
	case 50:
		d.stream.AddOffset(segment.DataLength)
	case 51:
		return ResultEndReached
	case 52:
		d.stream.AddOffset(segment.DataLength)
	case 53:
		return d.parseTable(segment)
	case 62:
		d.stream.AddOffset(segment.DataLength)
	default:
		// Naive default (silently advance past payload) lets
		// reserved/undefined types slip and downstream refs
		// resolve against ghost entries. T.88 Annex H Table H.1
		// fixes the assigned set; anything else is malformed.
		// Defined-but-skippable types (50 end-of-stripe,
		// 52 profiles, 62 extension) handled by explicit cases above.
		return d.failf("reserved/undefined segment type %d at segment #%d: %w",
			segment.Flags.Type, segment.Number, errs.ErrMalformed)
	}
	return ResultSuccess
}

// DecodeSequential decodes segments in sequential order.
// Returns: Result the parse result.
func (d *Document) DecodeSequential() Result {
	if d.stream.GetByteLeft() <= 0 {
		return ResultEndReached
	}
	if d.Grouped {
		return d.decodeGrouped()
	}
	// Cap segments per call. Real docs: tens-to-hundreds per
	// page. Cap bounds wall-clock when adversarial input
	// declares many tiny segments. Without cap, fuzz seed
	// whose random bytes all parse as valid garbage headers
	// spins forever.
	//
	// Low intentionally: callers (public Decoder.Decode loop,
	// NewDecoderWithGlobals's drainGlobals) re-enter repeatedly,
	// stacking work. 64 x 256 caller iters covers any realistic
	// doc; above that upper-level no-progress guard fires.
	// Lowered from 256 after fuzz rounds 6-10 found same shape:
	// adversarial globals headers parse OK but per-segment work
	// adds to multi-second wall clock under fuzz parallelism.
	const MaxSegmentsPerDecodeCall = 64
	parsed := 0
	for d.stream.GetByteLeft() > 0 {
		// Cancellation check between segments. Caller's context
		// cancels decode promptly without waiting for current
		// per-segment parse; cancel latency bounded by one
		// segment, itself capped by Limits.MaxImagePixels /
		// MaxSymbolPixels / MaxSymbolDictPixels.
		if d.ctx != nil {
			if err := d.ctx.Err(); err != nil {
				return d.failf("DecodeSequential: %w", err)
			}
		}
		if parsed >= MaxSegmentsPerDecodeCall {
			// Budget hit: yield. Stream cursor advanced (consumed
			// `parsed` segments) so outer stall guard won't fire;
			// cap bounds per-call wall-clock without rejecting
			// legal streams with many segments before page boundary.
			return ResultSuccess
		}
		parsed++
		if d.segment == nil {
			d.segment = NewSegment()
			ret := d.ParseSegmentHeader(d.segment)
			if ret != ResultSuccess {
				d.segment = nil
				// Distinguish "stream ended mid-header" (quiet
				// EOF from ReadInteger/Read1Byte on tail
				// padding - silent exit so trailing garbage
				// doesn't fail valid inputs) from "header
				// parser called failf" (e.g. MaxBytesPerSegment
				// cap). Error stash is the discriminator:
				// only failf sets d.lastErr.
				if d.lastErr != nil {
					return ResultFailure
				}
				// Quiet-EOF at segment boundary (1-3 trailing
				// bytes, less than segNum read needs). Return
				// ResultEndReached so public decoder hands off
				// in-flight page bitmap and returns io.EOF
				// instead of falling through to ResultSuccess
				// where no-progress guard classifies as
				// ErrMalformed on next call.
				return ResultEndReached
			}
			d.offset = d.stream.GetOffset()
		}
		ret := d.ParseSegmentData(d.segment)
		if ret == ResultEndReached {
			d.segmentList = append(d.segmentList, d.segment)
			d.segment = nil
			return ResultSuccess
		}
		if ret == ResultPageCompleted {
			d.segmentList = append(d.segmentList, d.segment)
			d.segment = nil
			return ResultPageCompleted
		}
		if ret != ResultSuccess {
			d.segment = nil
			return ret
		}
		if d.segment.DataLength != 0xFFFFFFFF {
			// Compare in int64 before narrowing: DataLength near
			// MaxUint32 would produce wrapped uint32 <= GetLength()
			// and SetOffset(wrapped) seeks backward. Default cap
			// keeps unreachable, but disabled/raised cap exposes it.
			newOffset := int64(d.offset) + int64(d.segment.DataLength)
			streamLen := int64(d.stream.GetLength())
			if newOffset < 0 || newOffset > streamLen {
				d.stream.SetOffset(d.stream.GetLength())
			} else {
				d.stream.SetOffset(uint32(newOffset))
			}
		} else {
			d.stream.AddOffset(4)
		}
		d.segmentList = append(d.segmentList, d.segment)
		d.segment = nil
	}
	return ResultSuccess
}

// decodeGrouped decodes the grouped layout (T.88 random-access
// org: all segment headers in contiguous prologue, then all
// segment data).
// Returns: Result the parse result.
//
// Hardened to mirror [DecodeSequential]: per-call segment cap
// (MaxSegmentsPerDecodeCall = 64), context cancel checks
// between iters, failf-attributed errors on real failures.
// Without these, grouped was an adversarial-input escape hatch.
//
// Header walk and data walk split across invocations so data
// phase can return ResultPageCompleted/ResultEndReached on
// type-49/-51 segments. Naive data loop swallowing both
// classifications would make outer DecodeContext see stall and
// either report io.EOF without handing bitmap or trip
// no-progress guard.
func (d *Document) decodeGrouped() Result {
	const MaxSegmentsPerDecodeCall = 64

	if !d.groupedHeadersParsed {
		parsed := 0
		for d.stream.GetByteLeft() > 0 {
			if d.ctx != nil {
				if err := d.ctx.Err(); err != nil {
					return d.failf("decodeGrouped: %w", err)
				}
			}
			if parsed >= MaxSegmentsPerDecodeCall {
				// Yield mid-header walk; re-entry resumes from
				// stream cursor (headers appended to segmentList
				// stay), so cap bounds per-call work without
				// truncating header collection for legal streams.
				return ResultSuccess
			}
			parsed++
			seg := NewSegment()
			ret := d.ParseSegmentHeader(seg)
			if ret != ResultSuccess {
				if d.lastErr != nil {
					return ResultFailure
				}
				break
			}
			d.segmentList = append(d.segmentList, seg)
			if seg.Flags.Type == 51 {
				break
			}
		}
		d.groupedHeadersParsed = true
		d.groupedDataOffset = d.stream.GetOffset()
		d.groupedDataIdx = 0
	}

	parsed := 0
	for d.groupedDataIdx < len(d.segmentList) {
		if d.ctx != nil {
			if err := d.ctx.Err(); err != nil {
				return d.failf("decodeGrouped: %w", err)
			}
		}
		if parsed >= MaxSegmentsPerDecodeCall {
			// Yield mid-data walk; groupedDataIdx tracks next
			// segment, so re-entry resumes where we stopped.
			return ResultSuccess
		}
		parsed++
		seg := d.segmentList[d.groupedDataIdx]
		if seg.DataLength == 0 {
			// Header-only segment (end-of-page, end-of-file zero
			// payload). Dispatch through ParseSegmentData so
			// type-49/-51 propagate same way as data-bearing ones.
			d.segment = seg
			d.offset = d.groupedDataOffset
			ret := d.ParseSegmentData(seg)
			d.groupedDataIdx++
			if ret == ResultFailure {
				return ResultFailure
			}
			if ret == ResultPageCompleted {
				return ResultPageCompleted
			}
			if ret == ResultEndReached {
				return ResultEndReached
			}
			continue
		}
		d.stream.SetOffset(d.groupedDataOffset)
		d.segment = seg
		d.offset = d.groupedDataOffset
		ret := d.ParseSegmentData(seg)
		if ret == ResultFailure {
			return ResultFailure
		}
		if seg.DataLength != 0xFFFFFFFF {
			// int64 sum so DataLength near MaxUint32 can't wrap
			// groupedDataOffset backward past zero. Clamp at
			// stream length, matching sequential path on
			// over-declared data lengths.
			newOffset := int64(d.groupedDataOffset) + int64(seg.DataLength)
			streamLen := int64(d.stream.GetLength())
			if newOffset < 0 || newOffset > streamLen {
				d.groupedDataOffset = d.stream.GetLength()
			} else {
				d.groupedDataOffset = uint32(newOffset)
			}
			d.stream.SetOffset(d.groupedDataOffset)
		} else {
			// Unknown-length sentinel: ParseSegmentData consumed
			// indeterminate bytes; trust stream's post-parse
			// offset, not header-declared length (= sentinel).
			// Without this, data offset stays at payload start
			// and next SetOffset re-enters same bytes -
			// overlapping payloads, mis-parsing next segment.
			d.groupedDataOffset = d.stream.GetOffset()
		}
		d.groupedDataIdx++
		if ret == ResultPageCompleted {
			return ResultPageCompleted
		}
		if ret == ResultEndReached {
			return ResultEndReached
		}
	}
	return ResultEndReached
}

// parseSymbolDict parses a symbol-dictionary segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseSymbolDict(segment *Segment) Result {
	res := d.parseSymbolDictInner(segment)
	if res == ResultFailure && d.lastErr == nil {
		// Wrap with ErrMalformed so CLI exit-code picks exit 3.
		// Without sentinel, plain ResultFailure escapes (e.g.
		// bindSymbolDictHuffmanTables false without lastErr)
		// fall to generic exit 1. Mirrors ParseSegmentData fallback.
		d.lastErr = fmt.Errorf("parseSymbolDict (segment #%d): unspecified failure at stream offset %d: %w",
			segment.Number, d.stream.GetOffset(), errs.ErrMalformed)
	}
	return res
}

func (d *Document) parseSymbolDictInner(segment *Segment) Result {
	var flags uint16
	if val, err := d.stream.ReadShortInteger(); err != nil {
		return d.failfMalformed("parseSymbolDict: read flags: %v", err)
	} else {
		flags = val
	}
	sdd := symbol.NewSDDProc()
	if ret := d.applySymbolDictFlags(sdd, flags); ret != ResultSuccess {
		return ret
	}
	if ret := d.readSymbolDictCounts(sdd); ret != ResultSuccess {
		return ret
	}
	if ret := d.collectInputSymbols(sdd, segment); ret != ResultSuccess {
		return ret
	}
	if sdd.SDHUFF {
		if !d.bindSymbolDictHuffmanTables(sdd, flags, segment) {
			return ResultFailure
		}
	}
	gbSize, grSize := symbolDictContextSizes(sdd)
	gbContexts, grContexts := d.symbolDictContexts(segment, flags, gbSize, grSize)
	var err error
	if sdd.SDHUFF {
		segment.SymbolDict, err = sdd.DecodeHuffman(d.stream, gbContexts, grContexts)
		d.stream.AlignByte()
	} else {
		arithDecoder := arith.NewDecoder(d.stream)
		segment.SymbolDict, err = sdd.DecodeArith(arithDecoder, gbContexts, grContexts)
		d.stream.AlignByte()
		d.stream.AddOffset(2)
	}
	if err != nil {
		// classifyLeafErr: SDD-side budget rejections (e.g.
		// errTRDAllocFailed, MaxSymbolPixels, MaxSymbolDictPixels)
		// keep ErrResourceBudget; plain stream-corruption gets
		// ErrMalformed. Parity with generic/refinement/pattern-dict.
		return d.failf("parseSymbolDict: decode (SDHUFF=%v SDREFAGG=%v SDTEMPLATE=%d, %d new / %d exported): %w",
			sdd.SDHUFF, sdd.SDREFAGG, sdd.SDTEMPLATE, sdd.SDNUMNEWSYMS, sdd.SDNUMEXSYMS, classifyLeafErr(err))
	}
	// BITMAP_CC_RETAINED (flags bit 9): encoder wants decoder's
	// bitmap-coding contexts to outlive segment so subsequent
	// symbol-dict with BITMAP_CC_USED resumes same model. Stash
	// context arrays on segment for FindSegmentByNumber lookups.
	// Per §7.4.2.1.1 / Annex A.10, both generic-region and
	// refinement-region context arrays retained.
	if (flags & 0x0200) != 0 {
		segment.GBContexts = gbContexts
		segment.GRContexts = grContexts
	}
	segment.ResultType = state.SymbolDictPointer
	return ResultSuccess
}

// readSymbolDictCounts reads SDNUMEXSYMS + SDNUMNEWSYMS and
// applies the [MaxSymbolsPerDict] cap.
//
// Downstream symbol-dict decoder allocates arrays sized by
// SDNUMNEWSYMS + SDNUMINSYMS (`make([]*page.Image, ...)`,
// `make([]arith.Ctx, 1<<ceil(log2(sum)))`) without validation.
// Adversarial header declaring 4G symbols drives 32+ GB
// allocation; cap rejects at parse time. Real docs: at most
// a few thousand symbols per dict.
//
// Extracted from [Document.parseSymbolDictInner] for cognitive-
// complexity. No behavior change.
func (d *Document) readSymbolDictCounts(sdd *symbol.SDDProc) Result {
	val, err := d.stream.ReadInteger()
	if err != nil {
		return d.failfMalformed("parseSymbolDict: SDNUMEXSYMS: %v", err)
	}
	sdd.SDNUMEXSYMS = val
	val, err = d.stream.ReadInteger()
	if err != nil {
		return d.failfMalformed("parseSymbolDict: SDNUMNEWSYMS: %v", err)
	}
	sdd.SDNUMNEWSYMS = val
	if MaxSymbolsPerDict > 0 &&
		(sdd.SDNUMNEWSYMS > MaxSymbolsPerDict ||
			sdd.SDNUMEXSYMS > MaxSymbolsPerDict) {
		return d.failf("parseSymbolDict: symbol count %d/%d exceeds MaxSymbolsPerDict=%d: %w",
			sdd.SDNUMNEWSYMS, sdd.SDNUMEXSYMS, MaxSymbolsPerDict, errs.ErrResourceBudget)
	}
	return ResultSuccess
}

// collectInputSymbols walks segment.ReferredToSegmentNumbers,
// finds every Type-0 (symbol-dict) ancestor, appends its
// SymbolDict.Images to sdd.SDINSYMS in declaration order.
// Decoder uses SDINSYMS as base symbol set, extending with
// new symbols this dict produces.
//
// Returns ResultFailure when a referenced segment can't be
// resolved - malformed, not optional.
//
// Extracted from [Document.parseSymbolDictInner]. No behavior change.
func (d *Document) collectInputSymbols(sdd *symbol.SDDProc, segment *Segment) Result {
	if segment.ReferredToSegmentCount == 0 {
		return ResultSuccess
	}
	var inputSymbols []*page.Image
	for _, refNum := range segment.ReferredToSegmentNumbers {
		seg := d.FindSegmentByNumber(refNum)
		if seg == nil {
			return d.failfMalformed("parseSymbolDict: referenced segment #%d not found", refNum)
		}
		if seg.Flags.Type == 0 && seg.SymbolDict != nil {
			inputSymbols = append(inputSymbols, seg.SymbolDict.Images...)
			// Aggregate cap: per-dict bounds set earlier; without
			// total-input check, adversary chains N dicts each
			// under cap. Reject as soon as running total breaches
			// MaxSymbolsPerDict; corpus fixtures top out at 308.
			if MaxSymbolsPerDict > 0 && uint32(len(inputSymbols)) > MaxSymbolsPerDict {
				return d.failf("parseSymbolDict: aggregate input symbols %d exceeds MaxSymbolsPerDict=%d: %w",
					len(inputSymbols), MaxSymbolsPerDict, errs.ErrResourceBudget)
			}
		}
	}
	sdd.SDINSYMS = inputSymbols
	sdd.SDNUMINSYMS = uint32(len(inputSymbols))
	return ResultSuccess
}

// symbolDictContextSizes returns (gbContextSize, grContextSize)
// for the SDD per active templates.
//
//   - Generic-region context (T.88 Table 4): sized only when
//     SDHUFF=0 (symbols arithmetic-coded). SDTEMPLATE=0 -> 65536,
//     else 8192.
//   - Refinement-region context (T.88 §6.3.5): sized whenever
//     SDREFAGG=1 even in SDHUFF dict - refinement is always
//     arithmetic-coded. SDRTEMPLATE -> 1024 else 8192. Previous
//     SDHUFF-gated branch left grContexts empty; refinement
//     decoder indexed the zero-length slice - symhuffrefine\*
//     fixtures panicked "index out of range [0] with length 0".
//
// Extracted from [Document.parseSymbolDictInner]. No behavior change.
func symbolDictContextSizes(sdd *symbol.SDDProc) (gbSize, grSize int) {
	if !sdd.SDHUFF {
		if sdd.SDTEMPLATE == 0 {
			gbSize = 65536
		} else {
			gbSize = 8192
		}
	}
	if sdd.SDREFAGG {
		if sdd.SDRTEMPLATE {
			grSize = 1024
		} else {
			grSize = 8192
		}
	}
	return gbSize, grSize
}

// applySymbolDictFlags decodes the symbol-dict flags word
// (T.88 §7.4.2.1.1) onto sdd and reads associated AT-array
// fields:
//
//	Bit 0     SDHUFF
//	Bit 1     SDREFAGG
//	Bits 2-3  SD_HUFF_DH    (only if SDHUFF=1; consumed by
//	                         bindSymbolDictHuffmanTables)
//	Bits 4-5  SD_HUFF_DW    (only if SDHUFF=1; same)
//	Bit 6     SD_HUFF_BMSIZE
//	Bit 7     SD_HUFF_AGGINST
//	Bit 8     BITMAP_CC_USED      (read in symbolDictContexts)
//	Bit 9     BITMAP_CC_RETAINED  (read at end of caller)
//	Bits 10-11 SDTEMPLATE   (only if SDHUFF=0)
//	Bit 12    SDRTEMPLATE   (only if SDREFAGG=1)
//	Bits 13-15 reserved
//
// SDTEMPLATE/SDRTEMPLATE live in bits 10-12 for arithmetic
// dicts. Reading from Huffman-table selector bits misaligns AT
// bytes, shifts every later field, can turn SDNUMEXSYMS /
// SDNUMNEWSYMS into huge alloc counts. PDF-embedded and
// standalone .jb2 use ISO mapping.
//
// Extracted from [Document.parseSymbolDictInner]. No behavior change.
func (d *Document) applySymbolDictFlags(sdd *symbol.SDDProc, flags uint16) Result {
	sdd.SDHUFF = (flags & 0x0001) != 0
	sdd.SDREFAGG = ((flags >> 1) & 0x0001) != 0
	sdd.SDMMR = ((flags >> 10) & 0x01) != 0
	if !sdd.SDHUFF {
		sdd.SDTEMPLATE = uint8((flags >> 10) & 0x0003)
		sdd.SDRTEMPLATE = ((flags >> 12) & 0x0001) != 0
		dwTemp := 2
		if sdd.SDTEMPLATE == 0 {
			dwTemp = 8
		}
		for i := 0; i < dwTemp; i++ {
			val, err := d.stream.Read1Byte()
			if err != nil {
				return d.failfMalformed("parseSymbolDict: SDAT[%d]: %v", i, err)
			}
			sdd.SDAT[i] = int8(val)
		}
	}
	if sdd.SDREFAGG && !sdd.SDRTEMPLATE {
		for i := 0; i < 4; i++ {
			val, err := d.stream.Read1Byte()
			if err != nil {
				return d.failfMalformed("parseSymbolDict: SDRAT[%d]: %v", i, err)
			}
			sdd.SDRAT[i] = int8(val)
		}
	}
	return ResultSuccess
}

// symbolDictContexts returns (gbContexts, grContexts) to feed
// the SDD decoder. When BITMAP_CC_USED (bit 8) is set and a
// referred-to symbol-dict retained contexts (BITMAP_CC_RETAINED,
// bit 9, set on referenced segment), copy them so new dict
// resumes referenced segment's model. Else allocate fresh
// zero-initialized arrays.
//
// Walks refs last-to-first, picking nearest Type-0 ancestor so
// chained dicts inherit from most recent retained context.
//
// Extracted from [Document.parseSymbolDictInner]. No behavior change.
func (d *Document) symbolDictContexts(segment *Segment, flags uint16, gbSize, grSize int) (gbContexts, grContexts []arith.Ctx) {
	if (flags&0x0100) != 0 && len(segment.ReferredToSegmentNumbers) > 0 {
		refs := segment.ReferredToSegmentNumbers
		var refSeg *Segment
		for i := len(refs) - 1; i >= 0; i-- {
			s := d.FindSegmentByNumber(refs[i])
			if s != nil && s.Flags.Type == 0 {
				refSeg = s
				break
			}
		}
		if refSeg != nil {
			if len(refSeg.GBContexts) == gbSize {
				gbContexts = make([]arith.Ctx, gbSize)
				copy(gbContexts, refSeg.GBContexts)
			}
			if len(refSeg.GRContexts) == grSize {
				grContexts = make([]arith.Ctx, grSize)
				copy(grContexts, refSeg.GRContexts)
			}
		}
	}
	if gbContexts == nil {
		gbContexts = make([]arith.Ctx, gbSize)
	}
	if grContexts == nil {
		grContexts = make([]arith.Ctx, grSize)
	}
	return gbContexts, grContexts
}

// bindSymbolDictHuffmanTables decodes the four Huffman-table
// selectors (SD_HUFF_DH, SD_HUFF_DW, SD_HUFF_BMSIZE,
// SD_HUFF_AGGINST) and binds [huffman.Table] pointers onto sdd.
// Each selector is either a small index into standard tables
// (T.88 Annex B) or "use a referenced table segment", consumed
// in order from segment.ReferredToSegmentNumbers (Type 53 =
// Huffman table). Returns false on selector value 2 (reserved)
// or when refs run out before selectors do; caller surfaces as
// ResultFailure.
//
// Extracted from [Document.parseSymbolDictInner]. Table
// selection mirrors T.88 §7.4.2.1.1.
func (d *Document) bindSymbolDictHuffmanTables(sdd *symbol.SDDProc, flags uint16, segment *Segment) bool {
	cSDHUFFDH := (flags >> 2) & 0x0003
	cSDHUFFDW := (flags >> 4) & 0x0003
	cSDHUFFBMSIZE := (flags >> 6) & 0x0001
	cSDHUFFAGGINST := (flags >> 7) & 0x0001
	if cSDHUFFDH == 2 || cSDHUFFDW == 2 {
		return false
	}
	// Collect type-53 Huffman-table refs in declaration order;
	// "user-defined table" selectors consume them sequentially.
	var tableSegments []*Segment
	for _, refNum := range segment.ReferredToSegmentNumbers {
		seg := d.FindSegmentByNumber(refNum)
		if seg != nil && seg.Flags.Type == 53 {
			tableSegments = append(tableSegments, seg)
		}
	}
	tableIdx := 0
	// Each binding: "standard table N" for 0/1, "next user-defined"
	// for 3. Selector 2 rejected above.
	consumeUserTable := func(target **huffman.Table) bool {
		if tableIdx >= len(tableSegments) {
			return false
		}
		*target = tableSegments[tableIdx].HuffmanTable
		tableIdx++
		return true
	}
	switch cSDHUFFDH {
	case 0:
		sdd.SDHUFFDH = huffman.NewStandardTable(4)
	case 1:
		sdd.SDHUFFDH = huffman.NewStandardTable(5)
	default:
		if !consumeUserTable(&sdd.SDHUFFDH) {
			return false
		}
	}
	switch cSDHUFFDW {
	case 0:
		sdd.SDHUFFDW = huffman.NewStandardTable(2)
	case 1:
		sdd.SDHUFFDW = huffman.NewStandardTable(3)
	default:
		if !consumeUserTable(&sdd.SDHUFFDW) {
			return false
		}
	}
	if cSDHUFFBMSIZE == 0 {
		sdd.SDHUFFBMSIZE = huffman.NewStandardTable(1)
	} else if !consumeUserTable(&sdd.SDHUFFBMSIZE) {
		return false
	}
	if sdd.SDREFAGG {
		if cSDHUFFAGGINST == 0 {
			sdd.SDHUFFAGGINST = huffman.NewStandardTable(1)
		} else if tableIdx >= len(tableSegments) {
			return false
		} else {
			sdd.SDHUFFAGGINST = tableSegments[tableIdx].HuffmanTable
		}
	}
	return true
}

// ParseRegionInfo parses the region-info header.
// Parameters: ri the region info to populate.
// Returns: Result the parse result.
func (d *Document) ParseRegionInfo(ri *RegionInfo) Result {
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		ri.Width = int32(val)
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		ri.Height = int32(val)
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		ri.X = int32(val)
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		ri.Y = int32(val)
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		ri.Flags = val
	}
	return ResultSuccess
}

// GetHuffmanTable returns the standard Huffman table at the given index.
// Parameters: idx the standard table index.
// Returns: *huffman.Table the Huffman table.
func (d *Document) GetHuffmanTable(idx int) *huffman.Table {
	return huffman.NewStandardTable(idx)
}

// DecodeSymbolIDHuffmanTable decodes the symbol-ID Huffman table.
// Parameters: SBNUMSYMS the number of symbols.
// Returns: []huffman.Code the Huffman codes.
func (d *Document) DecodeSymbolIDHuffmanTable(SBNUMSYMS uint32) []huffman.Code {
	kRunCodesSize := 35
	huffmanCodes := make([]huffman.Code, kRunCodesSize)
	for i := 0; i < kRunCodesSize; i++ {
		val, err := d.stream.ReadNBits(4)
		if err != nil {
			return nil
		}
		huffmanCodes[i].Codelen = int32(val)
	}
	if err := huffman.AssignCode(huffmanCodes); err != nil {
		return nil
	}
	SBSYMCODES := make([]huffman.Code, SBNUMSYMS)
	i := int32(0)
	loopSyms := 0
	for i < int32(SBNUMSYMS) {
		loopSyms++
		if loopSyms > int(SBNUMSYMS)*10 {
			return nil
		}
		var j int
		var nSafeVal int32
		nBits := 0
		loopInner := 0
		for {
			loopInner++
			if loopInner > 1000 {
				return nil
			}
			bit, err := d.stream.Read1Bit()
			if err != nil {
				return nil
			}
			nSafeVal = (nSafeVal << 1) | int32(bit)
			nBits++
			for j = 0; j < kRunCodesSize; j++ {
				if int32(nBits) == huffmanCodes[j].Codelen && nSafeVal == huffmanCodes[j].Code {
					break
				}
			}
			if j < kRunCodesSize {
				break
			}
		}
		runcode := int32(j)
		var run int32
		if runcode < 32 {
			SBSYMCODES[i].Codelen = runcode
			run = 0
		} else if runcode == 32 {
			val, err := d.stream.ReadNBits(2)
			if err != nil {
				return nil
			}
			run = int32(val) + 3
		} else if runcode == 33 {
			val, err := d.stream.ReadNBits(3)
			if err != nil {
				return nil
			}
			run = int32(val) + 3
		} else if runcode == 34 {
			val, err := d.stream.ReadNBits(7)
			if err != nil {
				return nil
			}
			run = int32(val) + 11
		}
		if run > 0 {
			if i+run > int32(SBNUMSYMS) {
				return nil
			}
			for k := int32(0); k < run; k++ {
				if runcode == 32 && i > 0 {
					SBSYMCODES[i+k].Codelen = SBSYMCODES[i-1].Codelen
				} else {
					SBSYMCODES[i+k].Codelen = 0
				}
			}
			i += run
		} else {
			i++
		}
	}
	if err := huffman.AssignCode(SBSYMCODES); err != nil {
		return nil
	}
	return SBSYMCODES
}

// parseTextRegion parses a text-region segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseTextRegion(segment *Segment) Result {
	var ri RegionInfo
	if d.ParseRegionInfo(&ri) != ResultSuccess {
		return ResultFailure
	}
	var flags uint16
	if val, err := d.stream.ReadShortInteger(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	pTRD := symbol.NewTRDProc()
	pTRD.SBW = uint32(ri.Width)
	pTRD.SBH = uint32(ri.Height)
	pTRD.SBHUFF = (flags & 0x0001) != 0
	pTRD.SBREFINE = ((flags >> 1) & 0x0001) != 0
	dwTemp := (flags >> 2) & 0x0003
	pTRD.SBSTRIPS = 1 << dwTemp
	pTRD.REFCORNER = symbol.Corner((flags >> 4) & 0x0003)
	pTRD.TRANSPOSED = ((flags >> 6) & 0x0001) != 0
	pTRD.SBCOMBOP = page.ComposeOp((flags >> 7) & 0x0003)
	pTRD.SBDEFPIXEL = ((flags >> 9) & 0x0001) != 0
	pTRD.SBDSOFFSET = int8((flags >> 10) & 0x001F)
	if pTRD.SBDSOFFSET >= 0x10 {
		pTRD.SBDSOFFSET -= 0x20
	}
	pTRD.SBRTEMPLATE = ((flags >> 15) & 0x0001) != 0
	var huffFlags uint16
	if pTRD.SBHUFF {
		if val, err := d.stream.ReadShortInteger(); err != nil {
			return ResultFailure
		} else {
			huffFlags = val
		}
	}
	if pTRD.SBREFINE && !pTRD.SBRTEMPLATE {
		for i := 0; i < 4; i++ {
			if val, err := d.stream.Read1Byte(); err != nil {
				return ResultFailure
			} else {
				pTRD.SBRAT[i] = int8(val)
			}
		}
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pTRD.SBNUMINSTANCES = val
	}
	// SBNUMINSTANCES drives the instance-decoding loop. No
	// big alloc itself, but 4G-iteration loop on adversarial
	// input is DoS. Cap at per-dict budget so a single text
	// region can't dominate decode timer.
	if MaxSymbolsPerDict > 0 && pTRD.SBNUMINSTANCES > MaxSymbolsPerDict*16 {
		return d.failf("parseTextRegion: SBNUMINSTANCES=%d exceeds budget: %w", pTRD.SBNUMINSTANCES, errs.ErrResourceBudget)
	}
	if segment.ReferredToSegmentCount > 0 {
		for _, refNum := range segment.ReferredToSegmentNumbers {
			if d.FindSegmentByNumber(refNum) == nil {
				return d.failfMalformed("parseTextRegion: referenced segment #%d missing", refNum)
			}
		}
	}
	// uint64 sum so inputs exceeding MaxUint32 don't wrap when
	// caller disabled the aggregate cap (MaxSymbolsPerDict=0).
	// Default positive cap makes wraparound impractical, but
	// "limits disabled" should still reject hostile inputs, not
	// allocate a wrapped-small SBSYMS and decode against it.
	totalNumSyms := uint64(0)
	for _, refNum := range segment.ReferredToSegmentNumbers {
		seg := d.FindSegmentByNumber(refNum)
		if seg != nil && seg.Flags.Type == 0 && seg.SymbolDict != nil {
			totalNumSyms += uint64(seg.SymbolDict.NumImages())
		}
	}
	if totalNumSyms > math.MaxUint32 {
		return d.failf("parseTextRegion: aggregate SBNUMSYMS=%d exceeds uint32 range: %w",
			totalNumSyms, errs.ErrResourceBudget)
	}
	dwNumSyms := uint32(totalNumSyms)
	// Aggregate cap: per-dict counts bounded earlier; this site
	// sums across referenced dicts for SBSYMS sizing. Without
	// aggregate check, chained dicts grow slice unbounded. Real
	// fixtures top out at 308 aggregate input symbols.
	if MaxSymbolsPerDict > 0 && dwNumSyms > MaxSymbolsPerDict {
		return d.failf("parseTextRegion: aggregate SBNUMSYMS=%d exceeds MaxSymbolsPerDict=%d: %w",
			dwNumSyms, MaxSymbolsPerDict, errs.ErrResourceBudget)
	}
	pTRD.SBNUMSYMS = dwNumSyms
	SBSYMS := make([]*page.Image, pTRD.SBNUMSYMS)
	dwNumSyms = 0
	for _, refNum := range segment.ReferredToSegmentNumbers {
		seg := d.FindSegmentByNumber(refNum)
		if seg != nil && seg.Flags.Type == 0 && seg.SymbolDict != nil {
			dict := seg.SymbolDict
			for j := 0; j < dict.NumImages(); j++ {
				SBSYMS[dwNumSyms+uint32(j)] = dict.GetImage(j)
			}
			dwNumSyms += uint32(dict.NumImages())
		}
	}
	pTRD.SBSYMS = SBSYMS
	if pTRD.SBHUFF {
		if encodedTable := d.DecodeSymbolIDHuffmanTable(pTRD.SBNUMSYMS); encodedTable != nil {
			d.stream.AlignByte()
			pTRD.SBSYMCODES = encodedTable
		} else {
			return ResultFailure
		}
	} else {
		// Bit width for SBSYMS index. CeilLog2U32 handles 0/1
		// edges and clamps at 32 for counts past uint32 shift
		// width - see intmath.CeilLog2U32.
		pTRD.SBSYMCODELEN = intmath.CeilLog2U32(pTRD.SBNUMSYMS)
	}
	if pTRD.SBHUFF {
		cSBHUFFFS := huffFlags & 0x0003
		cSBHUFFDS := (huffFlags >> 2) & 0x0003
		cSBHUFFDT := (huffFlags >> 4) & 0x0003
		cSBHUFFRDW := (huffFlags >> 6) & 0x0003
		cSBHUFFRDH := (huffFlags >> 8) & 0x0003
		cSBHUFFRDX := (huffFlags >> 10) & 0x0003
		cSBHUFFRDY := (huffFlags >> 12) & 0x0003
		cSBHUFFRSIZE := (huffFlags >> 14) & 0x0001
		if !pTRD.SBREFINE {
			cSBHUFFRDW = 0
			cSBHUFFRDH = 0
			cSBHUFFRDX = 0
			cSBHUFFRDY = 0
			cSBHUFFRSIZE = 0
		}
		if cSBHUFFFS == 2 || cSBHUFFRDW == 2 || cSBHUFFRDH == 2 || cSBHUFFRDX == 2 || cSBHUFFRDY == 2 {
			return ResultFailure
		}
		tableIdx := 0
		tableSegments := make([]*Segment, 0)
		for _, refNum := range segment.ReferredToSegmentNumbers {
			seg := d.FindSegmentByNumber(refNum)
			if seg != nil && seg.Flags.Type == 53 {
				tableSegments = append(tableSegments, seg)
			}
		}
		getUserTable := func() *huffman.Table {
			if tableIdx < len(tableSegments) {
				t := tableSegments[tableIdx].HuffmanTable
				tableIdx++
				return t
			}
			return nil
		}
		switch cSBHUFFFS {
		case 0:
			pTRD.SBHUFFFS = d.GetHuffmanTable(6)
		case 1:
			pTRD.SBHUFFFS = d.GetHuffmanTable(7)
		default:
			pTRD.SBHUFFFS = getUserTable()
		}
		switch cSBHUFFDS {
		case 0:
			pTRD.SBHUFFDS = d.GetHuffmanTable(8)
		case 1:
			pTRD.SBHUFFDS = d.GetHuffmanTable(9)
		case 2:
			pTRD.SBHUFFDS = d.GetHuffmanTable(10)
		default:
			pTRD.SBHUFFDS = getUserTable()
		}
		switch cSBHUFFDT {
		case 0:
			pTRD.SBHUFFDT = d.GetHuffmanTable(11)
		case 1:
			pTRD.SBHUFFDT = d.GetHuffmanTable(12)
		case 2:
			pTRD.SBHUFFDT = d.GetHuffmanTable(13)
		default:
			pTRD.SBHUFFDT = getUserTable()
		}
		switch cSBHUFFRDW {
		case 0:
			pTRD.SBHUFFRDW = d.GetHuffmanTable(14)
		case 1:
			pTRD.SBHUFFRDW = d.GetHuffmanTable(15)
		default:
			pTRD.SBHUFFRDW = getUserTable()
		}
		switch cSBHUFFRDH {
		case 0:
			pTRD.SBHUFFRDH = d.GetHuffmanTable(14)
		case 1:
			pTRD.SBHUFFRDH = d.GetHuffmanTable(15)
		default:
			pTRD.SBHUFFRDH = getUserTable()
		}
		switch cSBHUFFRDX {
		case 0:
			pTRD.SBHUFFRDX = d.GetHuffmanTable(14)
		case 1:
			pTRD.SBHUFFRDX = d.GetHuffmanTable(15)
		default:
			pTRD.SBHUFFRDX = getUserTable()
		}
		switch cSBHUFFRDY {
		case 0:
			pTRD.SBHUFFRDY = d.GetHuffmanTable(14)
		case 1:
			pTRD.SBHUFFRDY = d.GetHuffmanTable(15)
		default:
			pTRD.SBHUFFRDY = getUserTable()
		}
		if cSBHUFFRSIZE == 0 {
			pTRD.SBHUFFRSIZE = d.GetHuffmanTable(1)
		} else {
			pTRD.SBHUFFRSIZE = getUserTable()
		}
	}
	getComposeOp := func(ri *RegionInfo) page.ComposeOp {
		if (ri.Flags & 0x07) == 4 {
			return page.ComposeReplace
		}
		return page.ComposeOp(ri.Flags & 0x03)
	}
	grContexts := make([]arith.Ctx, 0)
	if pTRD.SBREFINE {
		size := 8192
		if pTRD.SBRTEMPLATE {
			size = 1024
		}
		grContexts = make([]arith.Ctx, size)
	}
	segment.ResultType = state.ImagePointer
	var err error
	if pTRD.SBHUFF {
		var img *page.Image
		img, err = pTRD.DecodeHuffman(d.stream, grContexts)
		if err == nil {
			segment.Image = img
			d.stream.AlignByte()
		}
	} else {
		arithDecoder := arith.NewDecoder(d.stream)
		var img *page.Image
		img, err = pTRD.DecodeArith(arithDecoder, grContexts, nil)
		if err == nil {
			segment.Image = img
			d.stream.AlignByte()
			d.stream.AddOffset(2)
		}
	}
	if err != nil {
		// classifyLeafErr: TRD's errTRDAllocFailed wraps
		// ErrResourceBudget (survives); raw stream-corruption
		// picks up ErrMalformed for CLI exit-code.
		return d.failf("parseTextRegion: SBHUFF=%v SBREFINE=%v SBNUMINSTANCES=%d: %w",
			pTRD.SBHUFF, pTRD.SBREFINE, pTRD.SBNUMINSTANCES, classifyLeafErr(err))
	}
	if segment.Image == nil {
		// TRD returned (nil, nil) - treat as malformed so
		// public API surfaces classified error, not generic.
		return d.failf("parseTextRegion: decoder returned nil image: %w", errs.ErrMalformed)
	}
	if segment.Flags.Type != 4 {
		if len(d.pageInfoList) > 0 {
			pi := d.pageInfoList[len(d.pageInfoList)-1]
			if pi.IsStriped {
				expandPageForStripedRegion(d.page, ri, pi.DefaultPixelValue)
			}
		}
		d.page.ComposeFrom(ri.X, ri.Y, segment.Image, getComposeOp(&ri))
		d.pageVirgin = false
		segment.Image = nil
	}
	return ResultSuccess
}

// parsePatternDict parses a pattern-dictionary segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parsePatternDict(segment *Segment) Result {
	var flags byte
	pPDD := halftone.NewPDDProc()
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		pPDD.HDPW = val
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		pPDD.HDPH = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pPDD.GRAYMAX = val
	}
	if pPDD.GRAYMAX > state.MaxPatternIndex {
		return ResultFailure
	}
	pPDD.HDMMR = (flags & 0x01) != 0
	pPDD.HDTEMPLATE = (flags >> 1) & 0x03
	segment.ResultType = state.PatternDictPointer
	var err error
	if pPDD.HDMMR {
		segment.PatternDict, err = pPDD.DecodeMMR(d.stream)
		if err != nil {
			// classifyLeafErr: PDDProc budget rejections (e.g.
			// NewPatternDict declining HDPATS budget) keep
			// ErrResourceBudget; plain stream-corruption wraps
			// with ErrMalformed. Parity with parseGenericRegion.
			return d.failf("parsePatternDict: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
	} else {
		size := 1024
		switch pPDD.HDTEMPLATE {
		case 0:
			size = 65536
		case 1:
			size = 8192
		}
		gbContexts := make([]arith.Ctx, size)
		arithDecoder := arith.NewDecoder(d.stream)
		segment.PatternDict, err = pPDD.DecodeArith(arithDecoder, gbContexts)
		if err != nil {
			// Mirror MMR branch above.
			return d.failf("parsePatternDict: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
		d.stream.AddOffset(2)
	}
	return ResultSuccess
}

// parseHalftoneRegion parses a halftone-region segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseHalftoneRegion(segment *Segment) Result {
	var ri RegionInfo
	var flags byte
	pHRD := halftone.NewHTRDProc()
	if d.ParseRegionInfo(&ri) != ResultSuccess {
		return ResultFailure
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HGW = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HGH = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HGX = int32(val)
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HGY = int32(val)
	}
	if val, err := d.stream.ReadShortInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HRX = val
	}
	if val, err := d.stream.ReadShortInteger(); err != nil {
		return ResultFailure
	} else {
		pHRD.HRY = val
	}
	pHRD.HBW = uint32(ri.Width)
	pHRD.HBH = uint32(ri.Height)
	pHRD.HMMR = (flags & 0x01) != 0
	pHRD.HTEMPLATE = (flags >> 1) & 0x03
	pHRD.HENABLESKIP = ((flags >> 3) & 0x01) != 0
	pHRD.HCOMBOP = page.ComposeOp((flags >> 4) & 0x07)
	pHRD.HDEFPIXEL = ((flags >> 7) & 0x01) != 0
	if segment.ReferredToSegmentCount != 1 {
		return ResultFailure
	}
	seg := d.FindSegmentByNumber(segment.ReferredToSegmentNumbers[0])
	if seg == nil || seg.Flags.Type != 16 || seg.PatternDict == nil {
		return ResultFailure
	}
	pPatternDict := seg.PatternDict
	if pPatternDict.NUMPATS == 0 {
		return ResultFailure
	}
	pHRD.HNUMPATS = pPatternDict.NUMPATS
	pHRD.HPATS = pPatternDict.HDPATS
	pHRD.HPW = uint8(pPatternDict.HDPATS[0].Width())
	pHRD.HPH = uint8(pPatternDict.HDPATS[0].Height())
	segment.ResultType = state.ImagePointer
	var err error
	if pHRD.HMMR {
		d.stream.AlignByte()
		segment.Image, err = pHRD.DecodeMMR(d.stream)
		if err != nil {
			// classifyLeafErr: DecodeMMR's budget-wrapped
			// errors (MMR oversize / NewImage-nil from
			// mmr.errAllocFailed) keep sentinel; malformed-
			// stream wraps with ErrMalformed. Parity with
			// parseGenericRegion and parsePatternDict.
			return d.failf("parseHalftoneRegion: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
	} else {
		size := GetHuffContextSize(pHRD.HTEMPLATE)
		gbContexts := make([]arith.Ctx, size)
		arithDecoder := arith.NewDecoder(d.stream)
		segment.Image, err = pHRD.DecodeArith(arithDecoder, gbContexts)
		if err != nil {
			// Mirror MMR branch. Out-of-range gsval rejection
			// wraps ErrMalformed; decodeImage's NewImage-nil
			// is plain error that classifyLeafErr promotes too.
			return d.failf("parseHalftoneRegion: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
		d.stream.AddOffset(2)
	}
	if segment.Flags.Type != 20 {
		if len(d.pageInfoList) > 0 {
			pi := d.pageInfoList[len(d.pageInfoList)-1]
			if pi.IsStriped {
				expandPageForStripedRegion(d.page, ri, pi.DefaultPixelValue)
			}
		}
		op := page.ComposeOp(ri.Flags & 0x03)
		if (ri.Flags & 0x07) == 4 {
			op = page.ComposeReplace
		}
		d.page.ComposeFrom(ri.X, ri.Y, segment.Image, op)
		d.pageVirgin = false
		segment.Image = nil
	}
	return ResultSuccess
}

// parseGenericRegion parses a generic-region segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseGenericRegion(segment *Segment) Result {
	var ri RegionInfo
	var flags byte
	if d.ParseRegionInfo(&ri) != ResultSuccess {
		return ResultFailure
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	pGRD := generic.NewProc()
	pGRD.GBW = uint32(ri.Width)
	pGRD.GBH = uint32(ri.Height)
	pGRD.MMR = (flags & 0x01) != 0
	pGRD.GBTEMPLATE = (flags >> 1) & 0x03
	pGRD.TPGDON = ((flags >> 3) & 0x01) != 0
	if !pGRD.MMR {
		if pGRD.GBTEMPLATE == 0 {
			for i := 0; i < 8; i++ {
				if val, err := d.stream.Read1Byte(); err != nil {
					return ResultFailure
				} else {
					pGRD.GBAT[i] = int8(val)
				}
			}
		} else {
			for i := 0; i < 2; i++ {
				if val, err := d.stream.Read1Byte(); err != nil {
					return ResultFailure
				} else {
					pGRD.GBAT[i] = int8(val)
				}
			}
		}
	}
	pGRD.USESKIP = false
	segment.ResultType = state.ImagePointer

	// In-place decode shortcut: when the region exactly covers
	// a virgin all-paper page and the compose op is Or or
	// Replace, decode straight into d.page instead of an
	// intermediate bitmap. ComposeOr against zero equals the
	// source; ComposeReplace overwrites unconditionally - either
	// way the temp + ComposeFrom byte walk reproduces what
	// in-place writes do for free. Profiling on
	// perf-text-generic puts the temp + compose at ~8 % of
	// decode time.
	//
	// Predicate is conservative: MMR uses a different decode
	// path and writes its own buffer, striped pages need the
	// expand-page step, segment.Flags.Type 36 is the intermediate
	// region type that intentionally retains segment.Image for a
	// later consumer, and any prior region write on this page
	// clears pageVirgin so we don't silently wipe earlier content.
	var inPlace *page.Image
	composeRect := pGRD.GetReplaceRect()
	_ = composeRect // captured later after decode if not in-place
	if !pGRD.MMR && d.pageVirgin && d.page != nil && segment.Flags.Type != 36 &&
		ri.X == 0 && ri.Y == 0 &&
		ri.Width == d.page.Width() && ri.Height == d.page.Height() {
		op := page.ComposeOp(ri.Flags & 0x03)
		if (ri.Flags & 0x07) == 4 {
			op = page.ComposeReplace
		}
		if op == page.ComposeOr || op == page.ComposeReplace {
			if len(d.pageInfoList) > 0 && !d.pageInfoList[len(d.pageInfoList)-1].IsStriped {
				inPlace = d.page
			}
		}
	}

	if pGRD.MMR {
		_, err := pGRD.DecodeMMR(&segment.Image, d.stream)
		if err != nil {
			// DecodeMMR returns errMMROversize /
			// errMMRAllocFailed (wrap ErrResourceBudget) or
			// raw mmr.DecodeG4 stream-corruption.
			// classifyLeafErr passes budget through, wraps
			// stream-corruption with ErrMalformed for CLI.
			return d.failf("parseGenericRegion: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
	} else {
		size := GetHuffContextSize(pGRD.GBTEMPLATE)
		gbContexts := make([]arith.Ctx, size)
		arithDecoder := arith.NewDecoder(d.stream)
		var err error
		segment.Image, err = pGRD.DecodeArithInto(inPlace, arithDecoder, gbContexts)
		if err != nil {
			// DecodeArith returns errGenericOversize /
			// errGenericAllocFailed (wrap ErrResourceBudget)
			// or catch-all "decoding error" for arith-decode
			// failures. classifyLeafErr wraps catch-all with
			// ErrMalformed.
			return d.failf("parseGenericRegion: %w", classifyLeafErr(err))
		}
		d.stream.AlignByte()
		d.stream.AddOffset(2)
	}
	if segment.Flags.Type != 36 {
		if inPlace != nil {
			// Decoded directly into d.page; the compose step
			// would be a no-op (or even worse, an OR against
			// the just-written bytes). Just clear the alias
			// and mark the page dirty so subsequent regions
			// take the standard compose path.
			d.pageVirgin = false
			segment.Image = nil
			return ResultSuccess
		}
		if len(d.pageInfoList) > 0 {
			pi := d.pageInfoList[len(d.pageInfoList)-1]
			if pi.IsStriped {
				expandPageForStripedRegion(d.page, ri, pi.DefaultPixelValue)
			}
		}
		op := page.ComposeOp(ri.Flags & 0x03)
		if (ri.Flags & 0x07) == 4 {
			op = page.ComposeReplace
		}
		rect := pGRD.GetReplaceRect()
		d.page.ComposeFrom(ri.X+rect.Left, ri.Y+rect.Top, segment.Image, op)
		d.pageVirgin = false
		segment.Image = nil
	}
	return ResultSuccess
}

// GetHuffContextSize returns the context size for a given template.
// Parameters: template the template number.
// Returns: int the context size.
func GetHuffContextSize(template byte) int {
	switch template {
	case 0:
		return 65536
	case 1:
		return 8192
	}
	return 1024
}

// parseGenericRefinementRegion parses a generic-refinement-region segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseGenericRefinementRegion(segment *Segment) Result {
	var ri RegionInfo
	var flags byte
	if d.ParseRegionInfo(&ri) != ResultSuccess {
		return ResultFailure
	}
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	pGRRD := refinement.NewProc()
	pGRRD.GRW = uint32(ri.Width)
	pGRRD.GRH = uint32(ri.Height)
	pGRRD.GRTEMPLATE = (flags & 0x01) != 0
	pGRRD.TPGRON = ((flags >> 1) & 0x01) != 0
	if !pGRRD.GRTEMPLATE {
		for i := 0; i < 4; i++ {
			if val, err := d.stream.Read1Byte(); err != nil {
				return ResultFailure
			} else {
				pGRRD.GRAT[i] = int8(val)
			}
		}
	}
	var pageSubImage *page.Image
	if segment.ReferredToSegmentCount > 0 {
		var pSeg *Segment
		for _, refNum := range segment.ReferredToSegmentNumbers {
			pSeg = d.FindSegmentByNumber(refNum)
			if pSeg == nil {
				return ResultFailure
			}
			if pSeg.Flags.Type == 4 || pSeg.Flags.Type == 20 || pSeg.Flags.Type == 36 || pSeg.Flags.Type == 40 {
				break
			}
		}
		if pSeg != nil && pSeg.Image != nil {
			pGRRD.GRREFERENCE = pSeg.Image
		} else {
			return ResultFailure
		}
	} else {
		pageSubImage = d.page.SubImage(ri.X, ri.Y, ri.Width, ri.Height)
		pGRRD.GRREFERENCE = pageSubImage
	}
	pGRRD.GRREFERENCEDX = 0
	pGRRD.GRREFERENCEDY = 0
	size := 8192
	if pGRRD.GRTEMPLATE {
		size = 1024
	}
	grContexts := make([]arith.Ctx, size)
	arithDecoder := arith.NewDecoder(d.stream)
	segment.ResultType = state.ImagePointer
	var err error
	segment.Image, err = pGRRD.Decode(arithDecoder, grContexts)
	if err != nil {
		// refinement.Decode returns ErrResourceBudget-wrapped
		// for oversize/NewImage-nil, or plain errors for
		// "GRREFERENCE nil" / "decoder complete prematurely".
		// classifyLeafErr passes budget through, wraps others
		// with ErrMalformed.
		return d.failf("parseGenericRefinementRegion: %w", classifyLeafErr(err))
	}
	d.stream.AlignByte()
	d.stream.AddOffset(2)
	if segment.Flags.Type != 40 {
		if len(d.pageInfoList) > 0 {
			pi := d.pageInfoList[len(d.pageInfoList)-1]
			if pi.IsStriped {
				expandPageForStripedRegion(d.page, ri, pi.DefaultPixelValue)
			}
		}
		op := page.ComposeOp(ri.Flags & 0x03)
		if (ri.Flags & 0x07) == 4 {
			op = page.ComposeReplace
		}
		d.page.ComposeFrom(ri.X, ri.Y, segment.Image, op)
		d.pageVirgin = false
		// Drop per-region bitmap after composing. Other
		// immediate region parsers (text/halftone/generic)
		// already do this; refinement was leaking composed
		// bitmap until page completion, raising peak memory
		// on refinement-heavy pages. Type 40 = intermediate -
		// keep its image so later segments can refer.
		segment.Image = nil
	}
	return ResultSuccess
}

// parsePageInfo parses a page-information segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parsePageInfo(segment *Segment) Result {
	pi := &PageInfo{}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pi.Width = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pi.Height = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pi.ResolutionX = val
	}
	if val, err := d.stream.ReadInteger(); err != nil {
		return ResultFailure
	} else {
		pi.ResolutionY = val
	}
	var flags byte
	if val, err := d.stream.Read1Byte(); err != nil {
		return ResultFailure
	} else {
		flags = val
	}
	var striping uint16
	if val, err := d.stream.ReadShortInteger(); err != nil {
		return ResultFailure
	} else {
		striping = val
	}
	pi.DefaultPixelValue = (flags & 4) != 0
	pi.IsStriped = (striping & 0x8000) != 0
	pi.MaxStripeSize = striping & 0x7FFF
	height := pi.Height
	if height == 0xFFFFFFFF {
		height = uint32(pi.MaxStripeSize)
	}
	// Adversarial-alloc gate: declared pixel budget vs total
	// input bytes. 30-byte input declaring 152-MP page has
	// pixels-per-byte ratio ~5M - far past any legit compression.
	// See [MaxPixelsPerByte] for threshold reasoning.
	totalBytes := uint64(d.stream.GetLength())
	declaredPixels := uint64(pi.Width) * uint64(height)
	if MaxPixelsPerByte > 0 && totalBytes > 0 &&
		declaredPixels > MaxPixelsPerByte*totalBytes {
		return d.failf("parsePageInfo: declared %d pixels exceeds MaxPixelsPerByte (%d) x inputBytes (%d): %w",
			declaredPixels, MaxPixelsPerByte, totalBytes, errs.ErrResourceBudget)
	}
	d.page = page.NewImage(int32(pi.Width), int32(height))
	if d.page == nil {
		return d.failf("parsePageInfo: page %dx%d allocation rejected (likely MaxImagePixels): %w",
			pi.Width, height, errs.ErrResourceBudget)
	}
	// page.NewImage returns a zero-initialized buffer (= paper);
	// Fill only when DefaultPixelValue is ink, otherwise the
	// call would rewrite an already-zero buffer.
	if pi.DefaultPixelValue {
		d.page.Fill(true)
	}
	d.pageInfoList = append(d.pageInfoList, pi)
	d.inPage = true
	// page.NewImage zero-inits the buffer; Fill(false) preserves
	// the zero state. Only the all-paper start qualifies for
	// the in-place-decode shortcut in parseGenericRegion (Or
	// against zero == src; Replace overwrites unconditionally).
	d.pageVirgin = !pi.DefaultPixelValue
	return ResultSuccess
}

// parseTable parses a Huffman-table segment.
// Parameters: segment the segment.
// Returns: Result the parse result.
func (d *Document) parseTable(segment *Segment) Result {
	segment.ResultType = state.HuffmanTablePointer
	huff := huffman.NewTableFromStream(d.stream)
	if !huff.IsOK() {
		return ResultFailure
	}
	segment.HuffmanTable = huff
	d.stream.AlignByte()
	return ResultSuccess
}

// ReleasePageSegments releases per-page segment data.
// Parameters: pageNumber the page number whose segments should be released.
//
// Drops the page-bitmap reference so [page.MaxImagePixels]-sized
// allocations don't linger across [Decoder.Decode] calls. Next
// page-info segment allocates fresh; caller has already copied
// pixels via [page.Image.ToGoImage].
func (d *Document) ReleasePageSegments(pageNumber uint32) {
	// Grouped mode: d.groupedDataIdx is absolute index into
	// segmentList for next data-walk segment. Compacting list
	// shifts retained entries down by count of removed entries
	// preceding cursor; without adjust, next decodeGrouped
	// resumes at wrong segment (past new list end), making
	// multi-page grouped streams short-cut to EOF after page 1.
	// Count removed below cursor and subtract.
	removedBelowCursor := 0
	n := 0
	for i, seg := range d.segmentList {
		if seg.PageAssociation != pageNumber {
			d.segmentList[n] = seg
			n++
		} else {
			if i < d.groupedDataIdx {
				removedBelowCursor++
			}
			seg.Image = nil
			seg.PatternDict = nil
			seg.SymbolDict = nil
			seg.HuffmanTable = nil
		}
	}
	for i := n; i < len(d.segmentList); i++ {
		d.segmentList[i] = nil
	}
	d.segmentList = d.segmentList[:n]
	d.page = nil
	if removedBelowCursor > 0 {
		d.groupedDataIdx -= removedBelowCursor
		if d.groupedDataIdx < 0 {
			d.groupedDataIdx = 0
		}
	}
}

// ReleaseCurrentPageBitmap drops the in-progress packed page
// bitmap without compacting the segment list. Used by
// Decoder.DecodeContext on terminal failure so callers retaining
// the Decoder for diagnostic inspection don't keep a
// MaxImagePixels-sized alloc alive past failure. Distinct from
// [Document.ReleasePageSegments] (success path; also prunes
// page-associated segments). Failure path preserves segment list
// so callers can inspect what was parsed pre-abort. Global
// context and shared globals dict left alone.
func (d *Document) ReleaseCurrentPageBitmap() {
	if d == nil {
		return
	}
	d.page = nil
	// Drop in-page flag too so retry doesn't see
	// page==nil && inPage==true (region parsers reject as
	// type-X-outside-a-page, hiding original parse failure).
	// Called from success-handoff (inPage already false) and
	// DecodeContext failure / no-progress paths (clear stale
	// mid-page state).
	d.inPage = false
}

// ClearLastErr drops sticky-error stash so a fresh decode
// attempt records its own root cause. failf keeps first error
// per attempt for accurate attribution; public
// Decoder.DecodeContext treats each call as attempt boundary
// and clears here.
func (d *Document) ClearLastErr() {
	if d != nil {
		d.lastErr = nil
	}
}

// Accessors for the public-API package. Document fields stay
// unexported so internal mutations stay disciplined; these
// shims let root *Decoder read state and signal page
// consumption without widening surface to external callers.

// GlobalContextDoc returns the global-segments Document, or nil
// when this is itself the global context.
func (d *Document) GlobalContextDoc() *Document {
	if d == nil {
		return nil
	}
	return d.globalContext
}

// InPage reports whether document is inside an open page
// (type-48 page-info consumed, no end-of-page seen yet).
func (d *Document) InPage() bool {
	if d == nil {
		return false
	}
	return d.inPage
}

// SetInPage flips in-page state. Public Decoder uses this to
// clear flag after orchestrator returns state.EndReached but
// page bitmap still pending hand-off.
func (d *Document) SetInPage(v bool) {
	if d != nil {
		d.inPage = v
	}
}

// Page returns the assembled page image, or nil if no page has
// been completed yet.
func (d *Document) Page() *page.Image {
	if d == nil {
		return nil
	}
	return d.page
}

// PageInfoList returns page-info records seen so far (one per
// page-info segment).
//
// Memory note: slice is historical state - every page-info
// parsed is appended, never trimmed. PageInfo values are small
// (few dozen bytes), so retention is O(page count) and
// negligible for typical docs. Callers on long-lived Decoder
// driving many-page streams should know it grows monotonically,
// not just current page.
func (d *Document) PageInfoList() []*PageInfo {
	if d == nil {
		return nil
	}
	return d.pageInfoList
}

// StreamOffset returns current byte offset into the document's
// segment stream. Prefer [Document.Progress] for no-progress
// guards - grouped-mode data walks advance through zero-length
// segments (end-of-stripe/-of-file/dataLen=0 page-info) without
// moving stream cursor, so StreamOffset alone misses that
// class of legal progress.
func (d *Document) StreamOffset() uint32 {
	if d == nil || d.stream == nil {
		return 0
	}
	return d.stream.GetOffset()
}

// Progress returns a monotonically-increasing token capturing
// every forward motion: stream cursor (any wire byte consumed)
// and grouped-mode data-walk index (advances even on
// zero-length segments). Public no-progress guards use this so
// legal run of many zero-length grouped segments doesn't trip
// false-stall classification.
//
// Packed as (stream offset << 32) | groupedDataIdx; both
// components well under 32 bits in any realistic input
// (MaxBytesPerSegment * MaxSegmentsPerDecodeCall x caller-iter
// cap inside uint32; groupedDataIdx bounded by segmentList).
func (d *Document) Progress() uint64 {
	if d == nil || d.stream == nil {
		return 0
	}
	idx := d.groupedDataIdx
	if idx < 0 {
		idx = 0
	}
	return uint64(d.stream.GetOffset())<<32 | uint64(idx)
}
