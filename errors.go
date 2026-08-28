package gobig2

import "github.com/tannevaled/gobig2/internal/errs"

// Sentinel errors for `errors.Is` classification. Every
// decode/parse failure from public Decoder API wraps one of
// these (or `context.Canceled` / `context.DeadlineExceeded`
// for cancellation). Categories partition by caller action:
// ErrMalformed = input not legal JBIG2 (skip image);
// ErrResourceBudget = configured Limits cap fired (raise cap
// or accept rejection); ErrUnsupported = legal input, gobig2
// path unimplemented (fall back to another decoder if able).
//
// Scope. Sentinel-wrap covers errors decoder produces during
// segment parsing and bitmap allocation. Errors before parser
// sees input - chiefly `io.Reader` failures constructors
// surface from source (dropped network, EIO file, etc.) -
// return as-is with source type. Treat app I/O failures
// separately from decode classification; "unwrapped error"
// branch below does not imply gobig2 bug if source io.Reader
// is fallible.
//
// Typical PDF-reader pattern:
//
//	img, err := dec.DecodeContext(ctx)
//	switch {
//	case errors.Is(err, io.EOF):
//	    // no more pages - multi-page Decode reached the end
//	case errors.Is(err, context.DeadlineExceeded):
//	    // budget exhausted - caller policy
//	case errors.Is(err, gobig2.ErrResourceBudget):
//	    // input declared a region past Limits - skip / raise cap
//	case errors.Is(err, gobig2.ErrMalformed):
//	    // bad JBIG2 - skip image
//	case errors.Is(err, gobig2.ErrUnsupported):
//	    // valid but unimplemented variant - fall back if possible
//	case err != nil:
//	    // unwrapped error - application-side I/O failure
//	    // (or, less likely, a gobig2 bug). Inspect with the
//	    // application's own io.Reader / source-specific
//	    // matchers first.
//	}
//
// [Decoder.Decode] / [Decoder.DecodeContext] return [io.EOF]
// after final page on multi-page input; idiomatic end-of-stream
// signal, not wrapped by any gobig2 sentinel.
var (
	// ErrMalformed wraps every parser-side failure where the
	// input bytes do not conform to JBIG2 (truncation, bad
	// segment header, out-of-bounds segment reference, etc.).
	ErrMalformed = errs.ErrMalformed

	// ErrResourceBudget wraps every failure caused by a
	// configured [Limits] cap firing. The wrapped error names
	// the specific cap.
	ErrResourceBudget = errs.ErrResourceBudget

	// ErrUnsupported wraps failures where the input is legal
	// JBIG2 but uses a feature gobig2 does not implement.
	ErrUnsupported = errs.ErrUnsupported
)
