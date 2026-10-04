// Package bio implements bit-level I/O over a byte buffer.
//
// JBIG2 mixes byte-aligned headers with bit-packed payloads. BitStream
// tracks bit position, exposes n-bit / 1-bit / byte-aligned reads named
// by spec, and arithmetic-decoder byte peeks (0xFF past EOS) so MQ
// coder skips its own buffer view.
package bio

import "errors"

// BitStream is a bit-oriented byte stream.
type BitStream struct {
	data         []byte
	byteIdx      uint32
	bitIdx       uint32
	key          uint64
	littleEndian bool
}

// MaxInputBytes caps physical buffer per BitStream. Over this,
// NewBitStream nils input -> downstream out-of-bounds errors.
// Hard cap independent of [gobig2.Limits]; defense vs pathological
// PDF /Length values. 256 MB far above legit JBIG2 (600-DPI A4 fax
// page: 100 KB-10 MB). Real per-segment/per-region budgets via
// [gobig2.Limits].
const MaxInputBytes = 256 * 1024 * 1024

// NewBitStream creates a bit stream.
// Parameters: data source bytes, key associated key.
// Returns: *BitStream stream.
//
// Inputs over [MaxInputBytes] nilled. Hard internal cap; downstream
// readers surface as out-of-bounds errors. For explicit "input too
// large", range-check len(data) vs MaxInputBytes pre-call and wrap
// as [gobig2.ErrResourceBudget].
func NewBitStream(data []byte, key uint64) *BitStream {
	if len(data) > MaxInputBytes {
		data = nil
	}
	return &BitStream{data: data, key: key}
}

// SetLittleEndian sets the byte order.
// Parameters: le true for little-endian.
func (bs *BitStream) SetLittleEndian(le bool) {
	bs.littleEndian = le
}

// ReadNBits reads an unsigned integer of the given bit width.
// Parameters: bits bit count.
// Returns: uint32 value, error.
//
// Errors if fewer than `bits` remain. Prior silent-partial behavior
// let adversarial Huffman/bit-field readers continue past truncation
// with plausible-but-wrong values. On truncation, bit position left
// wherever last advance ended; treat error as fatal for current
// parse stage.
//
// Callers needing legit peek past EOD (MMR codeword lookup in
// [internal/mmr]: 24-bit window straddles EOFB marker, decoder
// matches zero-padded EOFB entry) must use [ReadNBitsZeroPad].
func (bs *BitStream) ReadNBits(bits uint32) (uint32, error) {
	if !bs.IsInBounds() {
		return 0, errors.New("out of bounds")
	}
	// Result is uint32 -> bits > 32 is programmer error (high bits
	// silently truncated). Reject vs looping millions of iters on
	// adversarial width.
	if bits > 32 {
		return 0, errors.New("ReadNBits: width exceeds uint32 result type")
	}
	bitPos := bs.GetBitPos()
	lengthInBits := bs.lengthInBits()
	if bitPos > lengthInBits {
		return 0, errors.New("bit position out of range")
	}
	// uint64 widen: 32-bit bitPos+bits near math.MaxUint32 wraps
	// small and silently passes truncation guard.
	if uint64(bitPos)+uint64(bits) > uint64(lengthInBits) {
		return 0, errors.New("ReadNBits: truncated input - fewer bits remain than requested")
	}
	var result uint32
	for i := uint32(0); i < bits; i++ {
		result = (result << 1) | uint32((bs.data[bs.byteIdx]>>(7-bs.bitIdx))&0x01)
		bs.advanceBit()
	}
	return result, nil
}

// ReadNBitsZeroPad reads an unsigned integer of the given bit
// width, right-zero-padded when fewer than `bits` remain. Returned
// `consumed` (0..bits) is real bits read; stream advances exactly
// that many.
//
// Primitive for codeword lookups straddling EOD: JBIG2 MMR (T.6)
// needs 24-bit window past EOFB marker, table has zero-padded
// entries. Adversarial parsers should use [ReadNBits]; partial
// reads of structured fields are the silent-corruption pattern
// this helper makes explicit.
func (bs *BitStream) ReadNBitsZeroPad(bits uint32) (value, consumed uint32, err error) {
	if !bs.IsInBounds() {
		return 0, 0, errors.New("out of bounds")
	}
	// Same uint32 width contract as ReadNBits; zero-pad shift below
	// would over-shift on bits > 32.
	if bits > 32 {
		return 0, 0, errors.New("ReadNBitsZeroPad: width exceeds uint32 result type")
	}
	bitPos := bs.GetBitPos()
	lengthInBits := bs.lengthInBits()
	if bitPos > lengthInBits {
		return 0, 0, errors.New("bit position out of range")
	}
	bitsToRead := bits
	// uint64 widen: uint32 sum wraps on adversarial widths,
	// reports bitsToRead == bits despite fewer remaining.
	if uint64(bitPos)+uint64(bits) > uint64(lengthInBits) {
		bitsToRead = lengthInBits - bitPos
	}
	var result uint32
	for i := uint32(0); i < bitsToRead; i++ {
		result = (result << 1) | uint32((bs.data[bs.byteIdx]>>(7-bs.bitIdx))&0x01)
		bs.advanceBit()
	}
	// Right-pad: shift in (bits-bitsToRead) zeros to align window.
	result <<= bits - bitsToRead
	return result, bitsToRead, nil
}

// ReadNBitsInt32 reads a signed integer of the given bit width.
// Parameters: bits bit count (1..32).
// Returns: int32 sign-extended value, error.
//
// Two's-complement sign extend: for bits < 32, high bit replicated
// through bit 31. 5-bit 11111 -> -1; 10000 -> -16; 01111 -> 15.
//
// bits == 0 returns 0 no error. bits > 32 rejected by ReadNBits.
func (bs *BitStream) ReadNBitsInt32(bits uint32) (int32, error) {
	val, err := bs.ReadNBits(bits)
	if err != nil {
		return 0, err
	}
	if bits == 0 || bits >= 32 {
		return int32(val), nil
	}
	// Sign-extend: if high bit of bits-wide field set, replicate
	// through upper int32 bits.
	signBit := uint32(1) << (bits - 1)
	if val&signBit != 0 {
		mask := ^((uint32(1) << bits) - 1)
		val |= mask
	}
	return int32(val), nil
}

// Read1Bit reads a single bit.
// Returns: uint32 the bit, error any error encountered.
func (bs *BitStream) Read1Bit() (uint32, error) {
	if !bs.IsInBounds() {
		return 0, errors.New("out of bounds")
	}
	result := uint32((bs.data[bs.byteIdx] >> (7 - bs.bitIdx)) & 0x01)
	bs.advanceBit()
	return result, nil
}

// Read1BitBool reads a single bit as a bool.
// Returns: bool the bit, error any error encountered.
func (bs *BitStream) Read1BitBool() (bool, error) {
	val, err := bs.Read1Bit()
	return val != 0, err
}

// Read1Byte reads one byte.
// Returns: uint8 the byte, error any error encountered.
func (bs *BitStream) Read1Byte() (uint8, error) {
	if !bs.IsInBounds() {
		return 0, errors.New("out of bounds")
	}
	result := bs.data[bs.byteIdx]
	bs.byteIdx++
	return result, nil
}

// ReadInteger reads a 4-byte unsigned integer.
// Returns: uint32 the value, error any error encountered.
func (bs *BitStream) ReadInteger() (uint32, error) {
	if uint64(bs.byteIdx)+3 >= uint64(len(bs.data)) {
		return 0, errors.New("insufficient data")
	}
	var result uint32
	if bs.littleEndian {
		result = uint32(bs.data[bs.byteIdx]) | (uint32(bs.data[bs.byteIdx+1]) << 8) | (uint32(bs.data[bs.byteIdx+2]) << 16) | (uint32(bs.data[bs.byteIdx+3]) << 24)
	} else {
		result = (uint32(bs.data[bs.byteIdx]) << 24) | (uint32(bs.data[bs.byteIdx+1]) << 16) | (uint32(bs.data[bs.byteIdx+2]) << 8) | uint32(bs.data[bs.byteIdx+3])
	}
	bs.byteIdx += 4
	return result, nil
}

// ReadShortInteger reads a 2-byte unsigned integer.
// Returns: uint16 the value, error any error encountered.
func (bs *BitStream) ReadShortInteger() (uint16, error) {
	if uint64(bs.byteIdx)+1 >= uint64(len(bs.data)) {
		return 0, errors.New("insufficient data")
	}
	var result uint16
	if bs.littleEndian {
		result = uint16(bs.data[bs.byteIdx]) | (uint16(bs.data[bs.byteIdx+1]) << 8)
	} else {
		result = (uint16(bs.data[bs.byteIdx]) << 8) | uint16(bs.data[bs.byteIdx+1])
	}
	bs.byteIdx += 2
	return result, nil
}

// AlignByte aligns the stream to the next byte boundary.
func (bs *BitStream) AlignByte() {
	if bs.bitIdx != 0 {
		bs.AddOffset(1)
		bs.bitIdx = 0
	}
}

// GetCurByte returns the current byte.
// Returns: uint8 the current byte.
func (bs *BitStream) GetCurByte() uint8 {
	if bs.IsInBounds() {
		return bs.data[bs.byteIdx]
	}
	return 0
}

// IncByteIdx advances by one byte.
func (bs *BitStream) IncByteIdx() {
	bs.AddOffset(1)
}

// GetCurByteArith returns the current byte for arithmetic decoding.
// Returns: uint8 the current byte (0xFF when out of range).
func (bs *BitStream) GetCurByteArith() uint8 {
	if bs.IsInBounds() {
		return bs.data[bs.byteIdx]
	}
	return 0xFF
}

// GetNextByteArith returns the next byte for arithmetic decoding.
// Returns: uint8 the next byte (0xFF when out of range).
func (bs *BitStream) GetNextByteArith() uint8 {
	if uint64(bs.byteIdx)+1 < uint64(len(bs.data)) {
		return bs.data[bs.byteIdx+1]
	}
	return 0xFF
}

// GetOffset returns the current byte offset.
// Returns: uint32 the offset.
func (bs *BitStream) GetOffset() uint32 {
	return bs.byteIdx
}

// SetOffset sets the byte offset.
// Parameters: offset the new offset.
func (bs *BitStream) SetOffset(offset uint32) {
	size := uint32(len(bs.data))
	if offset > size {
		bs.byteIdx = size
	} else {
		bs.byteIdx = offset
	}
	bs.bitIdx = 0
}

// AddOffset advances the byte offset.
// Parameters: delta the delta to add.
func (bs *BitStream) AddOffset(delta uint32) {
	newOffset := uint64(bs.byteIdx) + uint64(delta)
	if newOffset <= uint64(len(bs.data)) {
		bs.SetOffset(uint32(newOffset))
	} else {
		bs.SetOffset(uint32(len(bs.data)))
	}
}

// GetBitPos returns the current absolute bit position.
// Returns: uint32 the bit position.
func (bs *BitStream) GetBitPos() uint32 {
	return (bs.byteIdx << 3) + bs.bitIdx
}

// SetBitPos sets the absolute bit position.
// Parameters: bitPos the new bit position.
func (bs *BitStream) SetBitPos(bitPos uint32) {
	bs.byteIdx = bitPos >> 3
	bs.bitIdx = bitPos & 7
}

// GetByteLeft returns the number of bytes remaining.
// Returns: uint32 the byte count.
func (bs *BitStream) GetByteLeft() uint32 {
	if bs.byteIdx >= uint32(len(bs.data)) {
		return 0
	}
	return uint32(len(bs.data)) - bs.byteIdx
}

// GetLength returns the total byte length.
// Returns: uint32 the total length.
func (bs *BitStream) GetLength() uint32 {
	return uint32(len(bs.data))
}

// GetPointer returns a slice from the current offset to the end.
// Returns: []byte the remaining slice.
func (bs *BitStream) GetPointer() []byte {
	if bs.byteIdx >= uint32(len(bs.data)) {
		return nil
	}
	return bs.data[bs.byteIdx:]
}

// GetKey returns the associated key value.
// Returns: uint64 the key.
func (bs *BitStream) GetKey() uint64 {
	return bs.key
}

// IsInBounds reports whether the stream is still in range.
// Returns: bool true if in bounds.
func (bs *BitStream) IsInBounds() bool {
	return bs.byteIdx < uint32(len(bs.data))
}

// advanceBit advances by one bit.
func (bs *BitStream) advanceBit() {
	if bs.bitIdx == 7 {
		bs.byteIdx++
		bs.bitIdx = 0
	} else {
		bs.bitIdx++
	}
}

// lengthInBits returns the total length in bits.
// Returns: uint32 the total bit count.
func (bs *BitStream) lengthInBits() uint32 {
	return uint32(len(bs.data)) * 8
}
