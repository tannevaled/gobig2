// Package generic implements JBIG2 generic-region decoding
// (T.88 §6.2). Workhorse mode: most text-region symbols and plain
// halftone-pattern bitmaps route through the arithmetic paths here.
// Also exposes MMR (Group 4) entry for fax-style bilevel pages.
package generic

import (
	"errors"
	"fmt"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/mmr"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// errGenericOversize is DecodeArith's resource-budget rejection
// when StartDecodeArith trips the state.MaxImageSize per-side
// check. Distinct from catch-all "decoding error" so document.go
// routes oversize to ErrResourceBudget, not ErrMalformed.
var errGenericOversize = fmt.Errorf("generic: GBW/GBH exceeds MaxImageSize: %w", errs.ErrResourceBudget)

// errGenericAllocFailed is DecodeArith's resource-budget
// rejection when page.NewImage returns nil (MaxImagePixels cap
// or int32 overflow).
var errGenericAllocFailed = fmt.Errorf("generic: failed to create image: %w", errs.ErrResourceBudget)

// errMMROversize, errMMRAllocFailed mirror arithmetic-path
// siblings for DecodeMMR's two resource-budget rejections.
// Distinct from mmr.DecodeG4 stream-decode error (malformed-input,
// stays unwrapped).
var (
	errMMROversize    = fmt.Errorf("generic: MMR GBW/GBH exceeds MaxImageSize: %w", errs.ErrResourceBudget)
	errMMRAllocFailed = fmt.Errorf("generic: MMR failed to create image: %w", errs.ErrResourceBudget)
)

// Proc is the generic-region decoding procedure.
type Proc struct {
	MMR         bool
	GBW         uint32
	GBH         uint32
	GBTEMPLATE  uint8
	TPGDON      bool
	USESKIP     bool
	SKIP        *page.Image
	GBAT        [8]int8
	loopIndex   uint32
	line        []byte
	ltp         int
	replaceRect state.Rect
}

// NewProc creates a new generic-region decoder.
func NewProc() *Proc { return &Proc{} }

// ProgressiveArithDecodeState bundles per-decode arithmetic
// state so [Proc.ProgressiveDecodeArith]'s template-dispatch
// loop routes GBTEMPLATE branches without redeclaring inputs.
type ProgressiveArithDecodeState struct {
	Image        **page.Image
	ArithDecoder *arith.Decoder
	GbContexts   []arith.Ctx
}

// StartDecodeArith begins arithmetic decoding from a fresh
// state.
//
// Image contract: when *s.Image is nil this function allocates
// a fresh zero-initialized bitmap. When non-nil the caller
// guarantees the bitmap is zero-initialized (the per-pixel
// decode path writes via `data[i] |= ...` and so relies on
// zero defaults outside the just-written bit positions).
// `parseGenericRegion`'s in-place decode passes a virgin
// `d.page`; `page.NewImage` zero-inits when caller passes
// nil. Both paths satisfy the precondition without a fresh
// `Fill(false)`.
func (g *Proc) StartDecodeArith(s *ProgressiveArithDecodeState) state.SegmentState {
	if g.GBW > state.MaxImageSize || g.GBH > state.MaxImageSize {
		// Adversarial dims past state.MaxImageSize. ParseComplete
		// would let DecodeArith return (nil, nil) -> silent
		// success, all-zero bitmap downstream. Fail explicit so
		// segment parser surfaces ErrResourceBudget.
		return state.Error
	}
	if *s.Image == nil {
		*s.Image = page.NewImage(int32(g.GBW), int32(g.GBH))
	}
	if *s.Image == nil {
		return state.Error
	}
	g.ltp = 0
	g.line = nil
	g.loopIndex = 0
	return g.ProgressiveDecodeArith(s)
}

// StartDecodeMMR begins MMR decoding. Retained for callers that
// only need segment-state signal; new callers prefer
// [Proc.DecodeMMR] for classified error.
func (g *Proc) StartDecodeMMR(image **page.Image, stream *bio.BitStream) state.SegmentState {
	if _, err := g.DecodeMMR(image, stream); err != nil {
		return state.Error
	}
	return state.ParseComplete
}

// DecodeMMR is the one-shot MMR decode helper. Parallel to
// [Proc.DecodeArith], returns one of three errors for caller
// classification:
//
//   - errMMROversize: GBW/GBH > state.MaxImageSize (resource
//     budget; parity with StartDecodeArith).
//   - errMMRAllocFailed: page.NewImage returns nil (resource
//     budget; MaxImagePixels or int32 overflow).
//   - raw mmr.DecodeG4 error on malformed stream (unwrapped;
//     surfaces as ErrMalformed via document Err() fallback).
//
// Image written through `image` on every successful alloc, even
// if DecodeG4 then fails. Detect partial-image case via
// `*image != nil && err != nil`.
func (g *Proc) DecodeMMR(image **page.Image, stream *bio.BitStream) (*page.Image, error) {
	if g.GBW > state.MaxImageSize || g.GBH > state.MaxImageSize {
		return nil, errMMROversize
	}
	*image = page.NewImage(int32(g.GBW), int32(g.GBH))
	if *image == nil {
		return nil, errMMRAllocFailed
	}
	if err := mmr.DecodeG4(stream, *image); err != nil {
		return nil, fmt.Errorf("generic: MMR decode: %w", err)
	}
	data := (*image).Data()
	for i := range data {
		data[i] = ^data[i]
	}
	g.replaceRect = state.Rect{Left: 0, Top: 0, Right: (*image).Width(), Bottom: (*image).Height()}
	return *image, nil
}

// DecodeArith is the one-shot arithmetic decode helper for
// callers that don't need the progressive path.
//
// Failure classification on state.Error: per-side MaxImageSize
// check, page.NewImage nil, or real arithmetic decode failure.
// First two surface as errGenericOversize / errGenericAllocFailed
// so parseGenericRegion routes to ErrResourceBudget. Rest keeps
// catch-all "decoding error" string -> ErrMalformed.
func (g *Proc) DecodeArith(decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	return g.DecodeArithInto(nil, decoder, contexts)
}

// DecodeArithInto is [Proc.DecodeArith] with a caller-supplied
// target bitmap. When target is non-nil it must match GBW x GBH
// and start zero-filled (StartDecodeArith calls Fill(false) so
// any non-zero prior content is wiped). The caller-supplied
// path lets [segment.Document.parseGenericRegion] decode a
// full-page region straight into the page bitmap, skipping the
// 35 Mpx temp + ComposeFrom byte walk on 600 dpi pages.
func (g *Proc) DecodeArithInto(target *page.Image, decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	if g.GBW > state.MaxImageSize || g.GBH > state.MaxImageSize {
		return nil, errGenericOversize
	}
	img := target
	s := &ProgressiveArithDecodeState{
		Image:        &img,
		ArithDecoder: decoder,
		GbContexts:   contexts,
	}
	res := g.StartDecodeArith(s)
	if res == state.Error {
		if *s.Image == nil {
			return nil, errGenericAllocFailed
		}
		return nil, errors.New("decoding error")
	}
	return *s.Image, nil
}

// GetReplaceRect returns rect written by last decode call.
// Callers compose this rect onto destination bitmap.
func (g *Proc) GetReplaceRect() state.Rect {
	return g.replaceRect
}

// ProgressiveDecodeArith dispatches one decode step against the
// active GBTEMPLATE.
func (g *Proc) ProgressiveDecodeArith(s *ProgressiveArithDecodeState) state.SegmentState {
	img := *s.Image
	g.replaceRect = state.Rect{Left: 0, Top: int32(g.loopIndex), Right: img.Width(), Bottom: int32(g.loopIndex)}
	var res state.SegmentState
	switch g.GBTEMPLATE {
	case 0:
		if g.useTemplate0Opt3() {
			res = g.decodeTemplate0Opt3(s)
		} else {
			res = g.decodeTemplate0Unopt(s)
		}
	case 1:
		if g.useTemplate1Opt3() {
			res = g.decodeTemplate1Opt3(s)
		} else {
			res = g.decodeTemplate1Unopt(s)
		}
	case 2:
		if g.useTemplate23Opt3() {
			res = g.decodeTemplate23Opt3(s, 2)
		} else {
			res = g.decodeTemplateUnopt(s, 2)
		}
	default:
		if g.useTemplate23Opt3() {
			res = g.decodeTemplate23Opt3(s, 3)
		} else {
			res = g.decodeTemplate3Unopt(s)
		}
	}
	g.replaceRect.Bottom = int32(g.loopIndex)
	if res == state.ParseComplete {
		g.loopIndex = 0
	}
	return res
}

func (g *Proc) useTemplate0Opt3() bool {
	return g.GBAT[0] == 3 && g.GBAT[1] == -1 && g.GBAT[2] == -3 &&
		g.GBAT[3] == -1 && g.GBAT[4] == 2 && g.GBAT[5] == -2 &&
		g.GBAT[6] == -2 && g.GBAT[7] == -2 && !g.USESKIP
}

func (g *Proc) useTemplate1Opt3() bool {
	return g.GBAT[0] == 3 && g.GBAT[1] == -1 && !g.USESKIP
}

func (g *Proc) useTemplate23Opt3() bool {
	return g.GBAT[0] == 2 && g.GBAT[1] == -1 && !g.USESKIP
}
