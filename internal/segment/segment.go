// Package segment owns the JBIG2 segment table and the
// document orchestrator (Document) that walks it. Decoder
// packages (generic, refinement, symbol, halftone) supply
// Procs; this package parses each header, dispatches the
// right Proc against its payload, and stitches resulting
// bitmaps onto the page image.
package segment

import (
	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/halftone"
	"github.com/tannevaled/gobig2/internal/huffman"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
	"github.com/tannevaled/gobig2/internal/symbol"
)

// SegmentFlags holds the segment-header flag bits.
type SegmentFlags struct {
	Type                uint8
	PageAssociationSize bool
	DeferredNonRetain   bool
}

// Segment is one parsed JBIG2 segment.
type Segment struct {
	Number                   uint32
	Flags                    SegmentFlags
	ReferredToSegmentCount   int32
	ReferredToSegmentNumbers []uint32
	PageAssociation          uint32
	DataLength               uint32
	HeaderLength             uint32
	DataOffset               uint32
	Key                      uint64
	State                    state.SegmentState
	ResultType               state.ResultType
	SymbolDict               *symbol.Dict
	PatternDict              *halftone.PatternDict
	Image                    *page.Image
	HuffmanTable             *huffman.Table
	GBContexts               []arith.Ctx
	GRContexts               []arith.Ctx
}

// NewSegment creates a segment.
func NewSegment() *Segment {
	return &Segment{
		State:      state.HeaderUnparsed,
		ResultType: state.VoidPointer,
	}
}

// RegionInfo holds region geometry and flags. Region parsers
// feed geometry to a Proc.
type RegionInfo struct {
	Width  int32
	Height int32
	X      int32
	Y      int32
	Flags  uint8
}
