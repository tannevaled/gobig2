// Package arith implements JBIG2 MQ arithmetic coder and the
// integer / IAID adapters above it.
//
// MQ coder is inner loop of every context-coded JBIG2 region.
// Decoder = bit decoder, Ctx = one renormalizing state cell.
// IntDecoder / IaidDecoder = standard arithmetic-integer and
// arithmetic-IAID decoders per T.88 §A.5 and §A.5.2.
package arith

import (
	"errors"

	"github.com/tannevaled/gobig2/internal/bio"
)

// defaultAValue is the default A register value.
const defaultAValue = 0x8000

// kQeTable is the Qe state table. T.88 defines 47 states
// (indices 0-46); we hold a 256-entry array so any uint8
// Ctx.i value indexes a valid Qe row without an explicit
// bounds check on the per-pixel hot path. Indices 47..255 are
// initialized in init() to clone state 0 - a Ctx corruption
// bug walking i past the legal range therefore stalls at
// state 0 rather than panicking, matching the pre-refactor
// "Decode returns 0" backstop.
var kQeTable [256]Qe

// kQeTableLegal is the 47 T.88-defined entries; init() copies
// these into kQeTable's first 47 slots and clones index 0
// into every slot past that.
var kQeTableLegal = [47]Qe{
	{0x5601, 1, 1, true},
	{0x3401, 2, 6, false},
	{0x1801, 3, 9, false},
	{0x0AC1, 4, 12, false},
	{0x0521, 5, 29, false},
	{0x0221, 38, 33, false},
	{0x5601, 7, 6, true},
	{0x5401, 8, 14, false},
	{0x4801, 9, 14, false},
	{0x3801, 10, 14, false},
	{0x3001, 11, 17, false},
	{0x2401, 12, 18, false},
	{0x1C01, 13, 20, false},
	{0x1601, 29, 21, false},
	{0x5601, 15, 14, true},
	{0x5401, 16, 14, false},
	{0x5101, 17, 15, false},
	{0x4801, 18, 16, false},
	{0x3801, 19, 17, false},
	{0x3401, 20, 18, false},
	{0x3001, 21, 19, false},
	{0x2801, 22, 19, false},
	{0x2401, 23, 20, false},
	{0x2201, 24, 21, false},
	{0x1C01, 25, 22, false},
	{0x1801, 26, 23, false},
	{0x1601, 27, 24, false},
	{0x1401, 28, 25, false},
	{0x1201, 29, 26, false},
	{0x1101, 30, 27, false},
	{0x0AC1, 31, 28, false},
	{0x09C1, 32, 29, false},
	{0x08A1, 33, 30, false},
	{0x0521, 34, 31, false},
	{0x0441, 35, 32, false},
	{0x02A1, 36, 33, false},
	{0x0221, 37, 34, false},
	{0x0141, 38, 35, false},
	{0x0111, 39, 36, false},
	{0x0085, 40, 37, false},
	{0x0049, 41, 38, false},
	{0x0025, 42, 39, false},
	{0x0015, 43, 40, false},
	{0x0009, 44, 41, false},
	{0x0005, 45, 42, false},
	{0x0001, 45, 43, false},
	{0x5601, 46, 46, false},
}

func init() {
	for i := range kQeTable {
		if i < len(kQeTableLegal) {
			kQeTable[i] = kQeTableLegal[i]
		} else {
			kQeTable[i] = kQeTableLegal[0]
		}
	}
}

// arithIntDecodeData is one entry in the arithmetic-integer decode table.
type arithIntDecodeData struct {
	nNeedBits int
	nValue    int32
}

// kArithIntDecodeData is the arithmetic-integer decode table.
var kArithIntDecodeData = []arithIntDecodeData{
	{2, 0}, {4, 4}, {6, 20}, {8, 84}, {12, 340}, {32, 4436},
}

// Qe is one arithmetic-coding state.
type Qe struct {
	Qe     uint16
	NMPS   uint8
	NLPS   uint8
	Switch bool
}

// Ctx is the arithmetic decoding context.
type Ctx struct {
	mps bool
	i   uint8
}

// DecodeNLPS decodes the NLPS branch.
// Parameters: qe the arithmetic-coding state.
// Returns: int the decoded value.
func (c *Ctx) DecodeNLPS(qe Qe) int {
	d := 0
	if !c.mps {
		d = 1
	}
	if qe.Switch {
		c.mps = !c.mps
	}
	c.i = qe.NLPS
	return d
}

// DecodeNMPS decodes the NMPS branch.
// Parameters: qe the arithmetic-coding state.
// Returns: int the decoded value.
func (c *Ctx) DecodeNMPS(qe Qe) int {
	c.i = qe.NMPS
	if c.mps {
		return 1
	}
	return 0
}

// MPS returns the MPS bit.
// Returns: int the MPS value.
func (c *Ctx) MPS() int {
	if c.mps {
		return 1
	}
	return 0
}

// I returns the state index.
// Returns: uint8 the I value.
func (c *Ctx) I() uint8 {
	return c.i
}

// Decoder is the arithmetic decoder.
type Decoder struct {
	stream   *bio.BitStream
	b        uint8
	c        uint32
	a        uint32
	ct       uint32
	complete bool
}

// NewDecoder creates a new arithmetic decoder.
// Parameters: stream the bit stream.
// Returns: *Decoder the decoder.
func NewDecoder(stream *bio.BitStream) *Decoder {
	ad := &Decoder{stream: stream, a: defaultAValue}
	ad.b = stream.GetCurByteArith()
	ad.c = (uint32(ad.b) ^ 0xff) << 16
	ad.byteIn()
	ad.c <<= 7
	ad.ct -= 7
	return ad
}

// Decode decodes one symbol.
// Parameters: cx the context.
// Returns: int the decoded bit.
//
// kQeTable is padded to 256 entries (see its var-decl comment)
// so cx.i indexes a real Qe row without an explicit bounds
// check on this per-pixel hot path; corrupt Ctx walks past the
// 47 legal states stall at state 0 rather than panicking.
//
// Internal callers ([IntDecoder.Decode], [IaidDecoder.Decode],
// [IntDecoder.recursiveDecode]) call this directly. Hot
// per-pixel callers in [generic] use [Decoder.TryDecodeFast]
// first and fall back here only when the fast path misses; the
// body's branching keeps Decode itself over the inline budget,
// so the fast-path inlining belongs at the call site.
func (ad *Decoder) Decode(cx *Ctx) int {
	qe := kQeTable[cx.i]
	ad.a -= uint32(qe.Qe)
	if (ad.c >> 16) < ad.a {
		if (ad.a & defaultAValue) != 0 {
			return cx.MPS()
		}
		var d int
		if ad.a < uint32(qe.Qe) {
			d = cx.DecodeNLPS(qe)
		} else {
			d = cx.DecodeNMPS(qe)
		}
		ad.readValueA()
		return d
	}
	ad.c -= ad.a << 16
	var d int
	if ad.a < uint32(qe.Qe) {
		d = cx.DecodeNMPS(qe)
	} else {
		d = cx.DecodeNLPS(qe)
	}
	ad.a = uint32(qe.Qe)
	ad.readValueA()
	return d
}

// TryDecodeFast attempts the MPS-no-renorm shortcut without
// mutating decoder state on miss. On hit it commits A and
// returns (bit, true); on miss state is untouched and the
// caller must fall through to [Decoder.Decode] for the renorm /
// conditional-exchange path.
//
// Exists so hot per-pixel loops (chiefly
// [generic.Proc.decodeTemplate0Opt3]) can fold the fast path
// inline - [Decoder.Decode] is cost-311 and won't fit the
// inline budget. Keep this body under the budget; don't add
// work here.
//
// kQeTable is padded to 256 entries so the cx.i read needs no
// bounds check on uint8 input.
func (ad *Decoder) TryDecodeFast(cx *Ctx) (int, bool) {
	qe := kQeTable[cx.i]
	aNew := ad.a - uint32(qe.Qe)
	if (ad.c>>16) < aNew && (aNew&defaultAValue) != 0 {
		ad.a = aNew
		if cx.mps {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// IsComplete reports whether decoding is complete.
// Returns: bool true if complete.
func (ad *Decoder) IsComplete() bool {
	return ad.complete
}

// byteIn reads the next input byte.
func (ad *Decoder) byteIn() {
	if ad.b == 0xff {
		b1 := ad.stream.GetNextByteArith()
		if b1 > 0x8f {
			ad.ct = 8
		} else {
			ad.stream.IncByteIdx()
			ad.b = b1
			ad.c = ad.c + 0xfe00 - (uint32(ad.b) << 9)
			ad.ct = 7
		}
	} else {
		ad.stream.IncByteIdx()
		ad.b = ad.stream.GetCurByteArith()
		ad.c = ad.c + 0xff00 - (uint32(ad.b) << 8)
		ad.ct = 8
	}
	if !ad.stream.IsInBounds() {
		ad.complete = true
	}
}

// readValueA reads bits until the A register is renormalized.
func (ad *Decoder) readValueA() {
	for {
		if ad.ct == 0 {
			ad.byteIn()
		}
		ad.a <<= 1
		ad.c <<= 1
		ad.ct--
		if (ad.a & defaultAValue) != 0 {
			break
		}
	}
}

// IntDecoder is the arithmetic integer decoder.
type IntDecoder struct {
	iax []Ctx
}

// NewIntDecoder creates a new arithmetic integer decoder.
// Returns: *IntDecoder the decoder.
func NewIntDecoder() *IntDecoder {
	return &IntDecoder{iax: make([]Ctx, 512)}
}

// Decode decodes one integer.
// Parameters: decoder arithmetic decoder.
// Returns: int32 decoded value, bool ok.
//
// Last bucket (idx==5) reads 32-bit suffix. `nValue + int32(nTemp)`
// wraps 0x80000000 to large negative; all-ones suffix wraps back to
// small positive indistinguishable from legal small int. Accumulate
// magnitude in int64, reject outside int32 range; callers treat
// ok=false as malformed stream.
func (aid *IntDecoder) Decode(decoder *Decoder) (int32, bool) {
	iax := aid.iax
	prev := 1
	s := decoder.Decode(&iax[prev])
	prev = (prev << 1) | s
	idx := aid.recursiveDecode(decoder, &prev, 0)
	bucket := kArithIntDecodeData[idx]
	// Suffix into uint64: covers 32 bits without wrap. Largest
	// bucket has nNeedBits == 32; saturated suffix stays in
	// range. The prev>>=256 wrap (per T.88) is hoisted out of
	// the loop body when prev never wraps - common for small
	// values.
	var nTemp uint64
	needBits := bucket.nNeedBits
	for i := 0; i < needBits; i++ {
		d := decoder.Decode(&iax[prev])
		prev = (prev << 1) | d
		if prev >= 256 {
			prev = (prev & 511) | 256
		}
		nTemp = (nTemp << 1) | uint64(d)
	}
	magnitude := int64(bucket.nValue) + int64(nTemp)
	if magnitude > 2147483647 || magnitude < -2147483648 {
		// Magnitude past int32 -> malformed vs silent wrap.
		return 0, false
	}
	val := int32(magnitude)
	if s == 1 && val > 0 {
		val = -val
	}
	if s == 1 && val == 0 {
		return 0, false
	}
	return val, true
}

// recursiveDecode walks the prefix tree to find the value bucket.
// Parameters: decoder the arithmetic decoder, prev the previous prefix value, depth the current depth.
// Returns: int the bucket index.
//
// Iterative despite the name: prefix tree has fixed max depth of
// len(kArithIntDecodeData)-1, so the original tail recursion
// rewrites cleanly to a counted loop and lets the compiler keep
// `prev` in a register instead of a heap-escaping pointer.
func (aid *IntDecoder) recursiveDecode(decoder *Decoder, prev *int, depth int) int {
	kDepthEnd := len(kArithIntDecodeData) - 1
	p := *prev
	for depth < kDepthEnd {
		cx := &aid.iax[p]
		d := decoder.Decode(cx)
		p = (p << 1) | d
		if d == 0 {
			*prev = p
			return depth
		}
		depth++
	}
	*prev = p
	return kDepthEnd
}

// IaidDecoder is the IAID decoder.
type IaidDecoder struct {
	iaid         []Ctx
	sbsymCodeLen uint8
}

// MaxIaidCodeLen caps SBSYMCODELEN pre-allocation. IAID context
// array sizes as 1<<SBSYMCODELEN; 32-bit SBSYMCODELEN -> 2^32 Ctx
// (~16 GB) before decoding. Real symbol IDs bounded by declared
// SDNUMNEWSYMS+SDNUMINSYMS; ceil(log2) can't exceed ~24 (16M syms)
// for any real document. 30 leaves headroom, caps below 16 GiB.
const MaxIaidCodeLen = 30

// clampedIaidCodeLen returns SBSYMCODELEN used to size IAID
// context array: min(sbsymCodeLen, MaxIaidCodeLen). Split out so
// clamping math testable without 1<<MaxIaidCodeLen allocation.
func clampedIaidCodeLen(sbsymCodeLen uint8) uint8 {
	if sbsymCodeLen > MaxIaidCodeLen {
		return MaxIaidCodeLen
	}
	return sbsymCodeLen
}

// NewIaidDecoder creates a new IAID decoder.
// Parameters: sbsymCodeLen symbol code length.
// Returns: *IaidDecoder decoder.
//
// SBSYMCODELEN > MaxIaidCodeLen silently clamped: decoder still
// constructs, Decode() fails with "index out of bounds" once it
// walks past shortened context array. Cheaper than threading
// errors through every caller; symbol-dict/text-region parsers
// already validate underlying symbol counts beforehand.
//
// Memory: at MaxIaidCodeLen=30 the array is 1<<30 Ctx (~2 GiB at
// 2-byte Ctx). Test clamping math via [clampedIaidCodeLen]; test
// decode-loop bounds via [IaidDecoder] with small `iaid` slice and
// large `sbsymCodeLen`.
func NewIaidDecoder(sbsymCodeLen uint8) *IaidDecoder {
	return &IaidDecoder{
		iaid:         make([]Ctx, 1<<clampedIaidCodeLen(sbsymCodeLen)),
		sbsymCodeLen: sbsymCodeLen,
	}
}

// Decode decodes one IAID.
// Parameters: decoder the arithmetic decoder.
// Returns: uint32 the decoded id, error any error encountered.
func (aid *IaidDecoder) Decode(decoder *Decoder) (uint32, error) {
	prev := 1
	for i := uint8(0); i < aid.sbsymCodeLen; i++ {
		if prev >= len(aid.iaid) {
			return 0, errors.New("index out of bounds")
		}
		cx := &aid.iaid[prev]
		d := decoder.Decode(cx)
		prev = (prev << 1) | d
	}
	return uint32(prev - (1 << aid.sbsymCodeLen)), nil
}
