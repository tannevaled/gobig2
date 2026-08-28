package generic

import (
	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/state"
)

var (
	kOptConstant1  = []uint16{0x9b25, 0x0795, 0x00e5}
	kOptConstant9  = []uint{0x000c, 0x0009, 0x0007}
	kOptConstant10 = []uint32{0x0007, 0x000f, 0x0007}
	kOptConstant11 = []uint32{0x001f, 0x001f, 0x000f}
	kOptConstant12 = []uint32{0x000f, 0x0007, 0x0003}
)

// decodeTemplate0Opt3 is the specialized path for GBTEMPLATE=0
// with default GBAT (3, -1, -3, -1, 2, -2, -2, -2) and !USESKIP
// (T.88 §6.2.5.4). Context bits 4..10 (row h-1, columns w-3..w+3)
// and bits 11..15 (row h-2, columns w-2..w+2) ride sliding
// accumulators: line2 holds 7 bits of row h-1 around the current
// column, line1 holds 5 bits of row h-2. Each iteration shifts
// in one new bit per accumulator (the rightmost pixel about to
// enter the window) instead of doing four independent GBAT
// reads. line3 (4 bits of row h to the left of w) is unchanged.
//
// For h >= 2 the interior loop reads slide bits by direct byte
// indexing; the right edge loop falls back to
// [decodeTemplate0Opt3.getBit] when w+3 / w+4 leave the image.
// For h < 2 the slow per-pixel path runs because h-1 or h-2 dips
// below row 0.
func (g *Proc) decodeTemplate0Opt3(s *ProgressiveArithDecodeState) state.SegmentState {
	if s.Image == nil || *s.Image == nil {
		return state.Error
	}
	img := *s.Image
	gbContexts := s.GbContexts
	decoder := s.ArithDecoder

	data := img.Data()
	stride := int(img.Stride())
	width := img.Width()
	GBW := int32(g.GBW)
	GBH := int32(g.GBH)

	getBit := func(x, y int32) uint32 {
		if x < 0 || x >= width || y < 0 || y >= GBH {
			return 0
		}
		b := data[int(y)*stride+int(x>>3)]
		return uint32((b >> uint(7-(x&7))) & 1)
	}

	for ; g.loopIndex < g.GBH; g.loopIndex++ {
		h := int32(g.loopIndex)
		if g.TPGDON {
			if decoder.IsComplete() {
				return state.Error
			}
			bit := decoder.Decode(&gbContexts[kOptConstant1[0]])
			if bit != 0 {
				g.ltp ^= 1
			}
		}
		if g.ltp == 1 {
			img.CopyLine(h, h-1)
			continue
		}

		// Slow path for the first two rows: h-1 or h-2 dips
		// below row 0 so the bounds-checked getBit drives the
		// whole row.
		if h < 2 {
			if res := g.decodeTemplate0Opt3RowChecked(h, decoder, gbContexts, data, stride, width, GBW, getBit); res != state.ParseComplete {
				return res
			}
			continue
		}

		row0 := int(h) * stride
		row1 := int(h-1) * stride
		row2 := int(h-2) * stride

		// Extended accumulators carry the GBAT pixels alongside
		// the 5-pixel / 3-pixel contexts. Bit layout at the
		// start of iteration w:
		//   line2 (7 bits, mask 0x7F):
		//     bit 0 = (w+3, h-1)  bit 4 = (w-1, h-1)
		//     bit 1 = (w+2, h-1)  bit 5 = (w-2, h-1)
		//     bit 2 = (w+1, h-1)  bit 6 = (w-3, h-1)
		//     bit 3 = (w,   h-1)
		//   line1 (5 bits, mask 0x1F):
		//     bit 0 = (w+2, h-2)  bit 3 = (w-1, h-2)
		//     bit 1 = (w+1, h-2)  bit 4 = (w-2, h-2)
		//     bit 2 = (w,   h-2)
		// Init at w=0: read 4 pixels from h-1 and 3 from h-2;
		// shadow pixels (negative x) are 0 via getBit.
		line2 := getBit(3, h-1) | (getBit(2, h-1) << 1) | (getBit(1, h-1) << 2) | (getBit(0, h-1) << 3)
		line1 := getBit(2, h-2) | (getBit(1, h-2) << 1) | (getBit(0, h-2) << 2)
		line3 := uint32(0)

		// Interior runs while the slide-in reads (w+4, h-1) and
		// (w+3, h-2) stay inside the image. (We slide BEFORE
		// loop continuation, so the last iter's reads are at
		// (w+4, h-1) where w == interiorEnd-1.)
		interiorEnd := width - 4
		if interiorEnd > GBW {
			interiorEnd = GBW
		}
		if interiorEnd < 0 {
			interiorEnd = 0
		}

		// Per-pixel decoder.IsComplete() check elided here:
		// arith.Decoder.Decode returns 0 once its stream is
		// exhausted (Decode's i-bounds guard fires when
		// renormalization can't make progress), so a malformed /
		// truncated input degrades to all-zero pixels for the
		// rest of the row instead of corrupting earlier data.
		// The right edge loop and the next row's preamble pick
		// the error up on the way out.
		//
		// Slide-in reads (newLine2 from row h-1 at column w+4,
		// newLine1 from row h-2 at column w+3): the byte
		// boundary refresh below loads 16 bits from each of
		// row1 / row2 once per 8 pixels and shifts through
		// them, so per-iteration the reads collapse to two
		// shift+mask ops with no slice bounds check.
		//
		// Layout: window1 = (data[row1+byteIdx]<<8) |
		// data[row1+byteIdx+1] covers row h-1 pixels [w_start ..
		// w_start+15] MSB-first. The newLine2 read at w_start+k
		// is at column (w_start+k+4); that's bit (11-k) of the
		// uint32 window (k = 0..7). Mirror for window2 / row2
		// with offset 3 -> bit (12-k).
		//
		// Safe bounds: byteIdx + 1 <= fullBytes <= (width-4)/8
		// < stride = ceil(width/8), so data[row1+byteIdx+1]
		// stays within row h-1.
		var window1, window2 uint32
		fullBytes := interiorEnd >> 3
		for w := int32(0); w < fullBytes<<3; w++ {
			if w&7 == 0 {
				byteIdx := int(w >> 3)
				window1 = uint32(data[row1+byteIdx])<<8 | uint32(data[row1+byteIdx+1])
				window2 = uint32(data[row2+byteIdx])<<8 | uint32(data[row2+byteIdx+1])
			}
			CONTEXT := line3 | (line2 << 4) | (line1 << 11)
			cx := &gbContexts[CONTEXT]
			bVal, ok := decoder.TryDecodeFast(cx)
			if !ok {
				bVal = decoder.Decode(cx)
			}
			if bVal != 0 {
				data[row0+int(w>>3)] |= 1 << uint(7-(w&7))
			}

			k := uint(w & 7)
			newLine2 := (window1 >> (11 - k)) & 1
			newLine1 := (window2 >> (12 - k)) & 1

			line2 = ((line2 << 1) | newLine2) & 0x7F
			line1 = ((line1 << 1) | newLine1) & 0x1F
			line3 = ((line3 << 1) | uint32(bVal)) & kOptConstant12[0]
		}
		// Tail of the interior: any pixels past the last whole
		// byte but before the right-edge bounds-checked region.
		// At most 7 iterations; per-pixel byte reads here.
		for w := fullBytes << 3; w < interiorEnd; w++ {
			CONTEXT := line3 | (line2 << 4) | (line1 << 11)
			cx := &gbContexts[CONTEXT]
			bVal, ok := decoder.TryDecodeFast(cx)
			if !ok {
				bVal = decoder.Decode(cx)
			}
			if bVal != 0 {
				data[row0+int(w>>3)] |= 1 << uint(7-(w&7))
			}
			bx := w + 4
			newLine2 := uint32((data[row1+int(bx>>3)] >> uint(7-(bx&7))) & 1)
			bx = w + 3
			newLine1 := uint32((data[row2+int(bx>>3)] >> uint(7-(bx&7))) & 1)
			line2 = ((line2 << 1) | newLine2) & 0x7F
			line1 = ((line1 << 1) | newLine1) & 0x1F
			line3 = ((line3 << 1) | uint32(bVal)) & kOptConstant12[0]
		}

		// Right edge: w+4 / w+3 may leave the image.
		for w := interiorEnd; w < GBW; w++ {
			if decoder.IsComplete() {
				return state.Error
			}
			CONTEXT := line3 | (line2 << 4) | (line1 << 11)
			bVal := decoder.Decode(&gbContexts[CONTEXT])
			if bVal != 0 {
				data[row0+int(w>>3)] |= 1 << uint(7-(w&7))
			}
			newLine2 := getBit(w+4, h-1)
			newLine1 := getBit(w+3, h-2)
			line2 = ((line2 << 1) | newLine2) & 0x7F
			line1 = ((line1 << 1) | newLine1) & 0x1F
			line3 = ((line3 << 1) | uint32(bVal)) & kOptConstant12[0]
		}
	}
	return state.ParseComplete
}

// decodeTemplate0Opt3RowChecked drives one row of decode using
// only the original 3-bit / 5-bit accumulators and the
// bounds-checked GBAT reads. Used by decodeTemplate0Opt3 for
// h < 2 (where the extended-accumulator init would read pixels
// from rows above the image).
func (g *Proc) decodeTemplate0Opt3RowChecked(
	h int32,
	decoder *arith.Decoder,
	gbContexts []arith.Ctx,
	data []byte, stride int, width, GBW int32,
	getBit func(x, y int32) uint32,
) state.SegmentState {
	line1 := getBit(1, h-2) | (getBit(0, h-2) << 1)
	line2 := getBit(2, h-1) | (getBit(1, h-1) << 1) | (getBit(0, h-1) << 2)
	line3 := uint32(0)
	row0 := int(h) * stride

	for w := int32(0); w < GBW; w++ {
		if decoder.IsComplete() {
			return state.Error
		}
		var pGbat0, pGbat1, pGbat2, pGbat3 uint32
		if x := w + 3; h >= 1 && x >= 0 && x < width {
			pGbat0 = getBit(x, h-1)
		}
		if x := w - 3; h >= 1 && x >= 0 && x < width {
			pGbat1 = getBit(x, h-1)
		}
		if x := w + 2; h >= 2 && x >= 0 && x < width {
			pGbat2 = getBit(x, h-2)
		}
		if x := w - 2; h >= 2 && x >= 0 && x < width {
			pGbat3 = getBit(x, h-2)
		}

		CONTEXT := line3
		CONTEXT |= pGbat0 << 4
		CONTEXT |= line2 << 5
		CONTEXT |= line1 << 12
		CONTEXT |= pGbat1 << 10
		CONTEXT |= pGbat2 << 11
		CONTEXT |= pGbat3 << 15

		bVal := decoder.Decode(&gbContexts[CONTEXT])
		if bVal != 0 {
			data[row0+int(w>>3)] |= 1 << uint(7-(w&7))
		}
		line1 = ((line1 << 1) | pGbat2) & kOptConstant10[0]
		line2 = ((line2 << 1) | pGbat0) & kOptConstant11[0]
		line3 = ((line3 << 1) | uint32(bVal)) & kOptConstant12[0]
	}
	return state.ParseComplete
}

func (g *Proc) decodeTemplate0Unopt(s *ProgressiveArithDecodeState) state.SegmentState {
	return g.decodeTemplateUnopt(s, 0)
}

func (g *Proc) decodeTemplate1Opt3(s *ProgressiveArithDecodeState) state.SegmentState {
	return g.decodeTemplateUnopt(s, 1)
}

func (g *Proc) decodeTemplate1Unopt(s *ProgressiveArithDecodeState) state.SegmentState {
	return g.decodeTemplateUnopt(s, 1)
}

// decodeTemplate23Opt3 routes to 0/1/2 unopt or dedicated
// template-3 unopt path.
//
// decodeTemplateUnopt is parameterized for templates 0-2 only
// (kOptConstant tables have 3 entries). Template 3 has own
// 10-bit context layout -> route to decodeTemplate3Unopt; else
// opt=3 indexes kOptConstant9 / kOptConstant1 out of range,
// panics decoder mid-stream.
func (g *Proc) decodeTemplate23Opt3(s *ProgressiveArithDecodeState, opt int) state.SegmentState {
	if opt == 3 {
		return g.decodeTemplate3Unopt(s)
	}
	return g.decodeTemplateUnopt(s, opt)
}

func (g *Proc) decodeTemplateUnopt(s *ProgressiveArithDecodeState, opt int) state.SegmentState {
	if s.Image == nil || *s.Image == nil {
		return state.Error
	}
	img := *s.Image
	gbContexts := s.GbContexts
	decoder := s.ArithDecoder
	mod2 := int32(opt % 2)
	div2 := int32(opt / 2)
	shift := uint(4 - opt)
	shiftC9 := kOptConstant9[opt]
	for ; g.loopIndex < g.GBH; g.loopIndex++ {
		h := int32(g.loopIndex)
		if g.TPGDON {
			if decoder.IsComplete() {
				return state.Error
			}
			bit := decoder.Decode(&gbContexts[kOptConstant1[opt]])
			if bit != 0 {
				g.ltp ^= 1
			}
		}
		if g.ltp == 1 {
			img.CopyLine(h, h-1)
			continue
		}
		line1 := uint32(img.GetPixel(1+mod2, h-2))
		line1 |= uint32(img.GetPixel(mod2, h-2)) << 1
		if opt == 1 {
			line1 |= uint32(img.GetPixel(0, h-2)) << 2
		}
		line2 := uint32(img.GetPixel(2-div2, h-1))
		line2 |= uint32(img.GetPixel(1-div2, h-1)) << 1
		if opt < 2 {
			line2 |= uint32(img.GetPixel(0, h-1)) << 2
		}
		line3 := uint32(0)
		for w := int32(0); w < int32(g.GBW); w++ {
			bVal := 0
			skip := false
			if g.USESKIP && g.SKIP != nil && g.SKIP.GetPixel(w, h) != 0 {
				skip = true
				bVal = 0
			}
			if !skip {
				if decoder.IsComplete() {
					return state.Error
				}
				CONTEXT := line3
				CONTEXT |= uint32(img.GetPixel(w+int32(g.GBAT[0]), h+int32(g.GBAT[1]))) << shift
				CONTEXT |= line2 << (shift + 1)
				CONTEXT |= line1 << shiftC9
				if opt == 0 {
					CONTEXT |= uint32(img.GetPixel(w+int32(g.GBAT[2]), h+int32(g.GBAT[3]))) << 10
					CONTEXT |= uint32(img.GetPixel(w+int32(g.GBAT[4]), h+int32(g.GBAT[5]))) << 11
					CONTEXT |= uint32(img.GetPixel(w+int32(g.GBAT[6]), h+int32(g.GBAT[7]))) << 15
				}
				bVal = decoder.Decode(&gbContexts[CONTEXT])
			}
			if bVal != 0 {
				img.SetPixel(w, h, bVal)
			}
			line1 = ((line1 << 1) | uint32(img.GetPixel(w+2+mod2, h-2))) & kOptConstant10[opt]
			line2 = ((line2 << 1) | uint32(img.GetPixel(w+3-div2, h-1))) & kOptConstant11[opt]
			line3 = ((line3 << 1) | uint32(bVal)) & kOptConstant12[opt]
		}
	}
	return state.ParseComplete
}

func (g *Proc) decodeTemplate3Unopt(s *ProgressiveArithDecodeState) state.SegmentState {
	if s.Image == nil || *s.Image == nil {
		return state.Error
	}
	img := *s.Image
	gbContexts := s.GbContexts
	decoder := s.ArithDecoder
	for ; g.loopIndex < g.GBH; g.loopIndex++ {
		h := int32(g.loopIndex)
		if g.TPGDON {
			if decoder.IsComplete() {
				return state.Error
			}
			bit := decoder.Decode(&gbContexts[0x0195])
			if bit != 0 {
				g.ltp ^= 1
			}
		}
		if g.ltp == 1 {
			img.CopyLine(h, h-1)
			continue
		}
		line1 := uint32(img.GetPixel(1, h-1))
		line1 |= uint32(img.GetPixel(0, h-1)) << 1
		line2 := uint32(0)
		for w := int32(0); w < int32(g.GBW); w++ {
			bVal := 0
			skip := false
			if g.USESKIP && g.SKIP != nil && g.SKIP.GetPixel(w, h) != 0 {
				skip = true
				bVal = 0
			}
			if !skip {
				if decoder.IsComplete() {
					return state.Error
				}
				CONTEXT := line2
				CONTEXT |= uint32(img.GetPixel(w+int32(g.GBAT[0]), h+int32(g.GBAT[1]))) << 4
				CONTEXT |= line1 << 5
				bVal = decoder.Decode(&gbContexts[CONTEXT])
			}
			if bVal != 0 {
				img.SetPixel(w, h, bVal)
			}
			line1 = ((line1 << 1) | uint32(img.GetPixel(w+2, h-1))) & 0x1f
			line2 = ((line2 << 1) | uint32(bVal)) & 0x0f
		}
	}
	return state.ParseComplete
}
