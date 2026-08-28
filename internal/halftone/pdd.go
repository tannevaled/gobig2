package halftone

import (
	"errors"
	"fmt"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/generic"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// PDDProc is the pattern-dictionary decoding procedure.
type PDDProc struct {
	HDMMR      bool
	HDPW, HDPH uint8
	GRAYMAX    uint32
	HDTEMPLATE uint8
}

// NewPDDProc creates a new pattern-dictionary decoder.
func NewPDDProc() *PDDProc { return &PDDProc{} }

// createGRDProc constructs a generic-region decoder for the dictionary.
func (p *PDDProc) createGRDProc() *generic.Proc {
	width := (p.GRAYMAX + 1) * uint32(p.HDPW)
	height := uint32(p.HDPH)
	if width > state.MaxImageSize || height > state.MaxImageSize {
		return nil
	}
	grd := generic.NewProc()
	grd.MMR = p.HDMMR
	grd.GBW = width
	grd.GBH = height
	return grd
}

// DecodeArith performs arithmetic decoding.
func (p *PDDProc) DecodeArith(arithDecoder *arith.Decoder, gbContexts []arith.Ctx) (*PatternDict, error) {
	grd := p.createGRDProc()
	if grd == nil {
		return nil, errors.New("failed to create grdproc")
	}
	grd.GBTEMPLATE = p.HDTEMPLATE
	grd.TPGDON = false
	grd.USESKIP = false
	grd.GBAT[0] = -int8(p.HDPW)
	grd.GBAT[1] = 0
	if grd.GBTEMPLATE == 0 {
		grd.GBAT[2] = -3
		grd.GBAT[3] = -1
		grd.GBAT[4] = 2
		grd.GBAT[5] = -2
		grd.GBAT[6] = -2
		grd.GBAT[7] = -2
	}
	var bhdc *page.Image
	st := &generic.ProgressiveArithDecodeState{
		Image:        &bhdc,
		ArithDecoder: arithDecoder,
		GbContexts:   gbContexts,
	}
	status := grd.StartDecodeArith(st)
	if status == state.Error || bhdc == nil {
		return nil, errors.New("arith decoding failure")
	}
	dict := NewPatternDict(p.GRAYMAX + 1)
	if dict == nil {
		// GRAYMAX+1 past MaxPatternsPerDict - adversarial header.
		// Error not panic on dict.HDPATS[...] below.
		return nil, fmt.Errorf("pattern dict size exceeds budget: %w", errs.ErrResourceBudget)
	}
	hdpw := int32(p.HDPW)
	hdph := int32(p.HDPH)
	for gray := uint32(0); gray <= p.GRAYMAX; gray++ {
		subImg := bhdc.SubImage(int32(gray)*hdpw, 0, hdpw, hdph)
		dict.HDPATS[gray] = subImg
	}
	return dict, nil
}

// DecodeMMR performs MMR decoding.
func (p *PDDProc) DecodeMMR(stream *bio.BitStream) (*PatternDict, error) {
	grd := p.createGRDProc()
	if grd == nil {
		return nil, errors.New("failed to create grdproc")
	}
	var bhdc *page.Image
	status := grd.StartDecodeMMR(&bhdc, stream)
	if status == state.Error || bhdc == nil {
		return nil, errors.New("mmr decoding failure")
	}
	dict := NewPatternDict(p.GRAYMAX + 1)
	if dict == nil {
		return nil, fmt.Errorf("pattern dict size exceeds budget: %w", errs.ErrResourceBudget)
	}
	hdpw := int32(p.HDPW)
	hdph := int32(p.HDPH)
	for gray := uint32(0); gray <= p.GRAYMAX; gray++ {
		subImg := bhdc.SubImage(int32(gray)*hdpw, 0, hdpw, hdph)
		dict.HDPATS[gray] = subImg
	}
	return dict, nil
}
