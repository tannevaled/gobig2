package gobig2test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	gobig2 "github.com/tannevaled/gobig2"
)

// External corpora env-gated. ITU-T T.88 conformance data too large
// to commit. Bundled fixture corpus ships under testdata/serenityos/;
// test defaults there. Env var overrides path without source edits.
//
//	JBIG2_CONFORMANCE_DIR - ITU-T T.88 (= ISO/IEC 14492) Annex A
//	                       conformance data distributed by ITU-T
//	                       (JBIG2_ConformanceData-A20180829).
//	                       Skip cleanly when unset.
//	JBIG2_SERENITYOS_DIR  - bundled jbig2 test inputs.
//	                       Defaults to testdata/serenityos/.
const (
	envConformance = "JBIG2_CONFORMANCE_DIR"
	envSerenityOS  = "JBIG2_SERENITYOS_DIR"

	defaultSerenityOSCorpus = "../../testdata/serenityos"
)

// Per-fixture decode budget. JBIG2 streams untrusted; pathological
// inputs must fail loud, not hang/OOM the runner. Small enough that
// infinite-loop bugs surface fast, large enough that legitimate big
// fixtures finish on slow VM.
const (
	decodeTimeout = 10 * time.Second
	decodeMemory  = int64(1) << 30 // 1 GiB
)

// decodeResult is the shape decodeWithBudget returns so tests
// distinguish failure modes (timeout, oom, error, success) without
// string parsing.
type decodeResult struct {
	img      image.Image
	err      error
	timedOut bool
	elapsed  time.Duration
	peakMB   uint64
}

// decodeWithBudget runs gobig2.Decode (or DecodeEmbedded if globals
// != nil) in goroutine with wall-clock timeout + soft memory ceiling
// via runtime/debug.SetMemoryLimit. Ceiling triggers aggressive GC
// vs OOM-kill; combined with decoder's gobig2.Limits guards, catches
// runaway alloc and slow infinite loops.
func decodeWithBudget(data, globals []byte, timeout time.Duration, memLimit int64) decodeResult {
	prev := debug.SetMemoryLimit(memLimit)
	defer debug.SetMemoryLimit(prev)

	type out struct {
		img image.Image
		err error
	}
	done := make(chan out, 1)
	var startMem runtime.MemStats
	runtime.ReadMemStats(&startMem)
	start := time.Now()

	// Wall-clock budget plumbed via cancellation context. Budget
	// fires -> cancel; decoder honors ctx between segments via
	// gobig2.DecodeContext, in-flight goroutine unwinds at next
	// segment boundary instead of running to completion and pinning
	// gobig2.Decoder / Document / page state past test failure.
	// Goroutine wrapper stays for panic capture + decodes faster
	// than timeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- out{nil, fmt.Errorf("panic: %v", r)}
			}
		}()
		var (
			dec *gobig2.Decoder
			err error
		)
		if len(globals) > 0 {
			dec, err = gobig2.NewDecoderWithGlobals(bytes.NewReader(data), globals)
		} else {
			dec, err = gobig2.NewDecoder(bytes.NewReader(data))
		}
		if err != nil {
			done <- out{nil, err}
			return
		}
		// Drive decoder to io.EOF so budget walk hits every page
		// of multi-page fixture. Single-call shape would signal
		// success on fixture whose page 1 decoded fine even if a
		// later page failed/leaked/rendered wrong. Keep first
		// decoded image as res.img so pixel-hash checks compare
		// against same first-page bytes; multi-page fixtures must
		// clear budget walk end-to-end.
		var first image.Image
		for {
			img, derr := dec.DecodeContext(ctx)
			if errors.Is(derr, io.EOF) {
				// EOF before any page decoded = malformed-or-empty
				// fixture, not pass. out{nil, nil} would let corpus
				// harness record success while reference-bitmap
				// check (where present) failed against nil image.
				// Surface no-page case as real error so zero-page
				// fixtures not silently accepted.
				if first == nil {
					done <- out{nil, fmt.Errorf("decoder reached io.EOF without producing any page")}
					return
				}
				done <- out{first, nil}
				return
			}
			if derr != nil {
				done <- out{first, derr}
				return
			}
			if first == nil {
				first = img
			}
		}
	}()

	// time.NewTimer + defer Stop avoids leak from
	// `case <-time.After(timeout)`: time.After timer survives until
	// fires or GC'd. Over 206-fixture corpus run, ~206 in-flight
	// timers when early return drops them. Cheap to stop explicitly.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-done:
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peak := uint64(0)
		if ms.HeapAlloc > startMem.HeapAlloc {
			peak = (ms.HeapAlloc - startMem.HeapAlloc) / (1 << 20)
		}
		return decodeResult{
			img:     r.img,
			err:     r.err,
			elapsed: time.Since(start),
			peakMB:  peak,
		}
	case <-timer.C:
		// Cancel so decode goroutine unwinds at next segment
		// boundary vs running to completion in background.
		// Pathological inner-loop bug can keep it alive past
		// cancellation; for common "slow but well-formed" shape
		// this releases heap reference promptly.
		cancel()
		return decodeResult{
			err:      fmt.Errorf("decode exceeded %s budget", timeout),
			timedOut: true,
			elapsed:  timeout,
		}
	}
}

// listFixtures returns sorted absolute paths of files in dir whose
// names end in any suffix. Nonexistent dir returns (nil, nil) so
// test skips cleanly.
func listFixtures(dir string, suffixes ...string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		matched := false
		for _, s := range suffixes {
			if strings.HasSuffix(strings.ToLower(name), s) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths, nil
}

// TestConformanceCorpus walks ITU-T T.88 (= ISO/IEC 14492) Annex A
// conformance data, asserts each fixture decodes within budget and
// matches committed reference bitmap.
//
// Reference convention in corpus:
//
//	<base>.bmp           - source bitmap for the test
//	<base>_TT<N>.jb2     - encoded fixture
//	<base>_TT<N>_TT00.bmp - decoder output reference
//
// Compare against _TT00 reference: what an implementation should
// produce at lossless / final pass.
func TestConformanceCorpus(t *testing.T) {
	dir := os.Getenv(envConformance)
	if dir == "" {
		t.Skipf("%s not set; skipping ITU-T T.88 conformance corpus", envConformance)
	}
	fixtures, err := listFixtures(dir, ".jb2", ".jbig2")
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Skipf("no .jb2 fixtures under %s", dir)
	}

	summary := newCorpusSummary(t)
	defer summary.report()

	for _, path := range fixtures {
		path := path
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				summary.add(name, false, err.Error())
				t.Fatalf("read fixture: %v", err)
			}
			res := decodeWithBudget(data, nil, decodeTimeout, decodeMemory)
			if res.err != nil {
				summary.add(name, false, res.err.Error())
				t.Errorf("decode failed (%s, peak %d MiB): %v", res.elapsed, res.peakMB, res.err)
				return
			}
			ref, refErr := loadConformanceReference(path)
			if refErr != nil {
				if errors.Is(refErr, errNoReference) {
					summary.add(name, true, "decoded; no reference available")
					return
				}
				summary.add(name, true, "decoded; reference unreadable")
				t.Logf("reference unreadable: %v", refErr)
				return
			}
			got, err := codecOutputAsReference(res.img)
			if err != nil {
				summary.add(name, false, "decoded but output unreadable: "+err.Error())
				t.Errorf("output normalization: %v", err)
				return
			}
			if got.width != ref.width || got.height != ref.height {
				summary.add(name, false, fmt.Sprintf("size mismatch: got %dx%d want %dx%d",
					got.width, got.height, ref.width, ref.height))
				t.Errorf("dimensions: got %dx%d want %dx%d",
					got.width, got.height, ref.width, ref.height)
				return
			}
			if !bytes.Equal(got.data, ref.data) {
				mismatch, total := got.hammingDistance(ref)
				summary.add(name, false, fmt.Sprintf("%d/%d pixels differ", mismatch, total))
				t.Errorf("pixel mismatch: %d of %d", mismatch, total)
				return
			}
			summary.add(name, true, fmt.Sprintf("%dx%d exact in %s", got.width, got.height, res.elapsed))
		})
	}
}

// errNoReference returned by loadConformanceReference when no
// _TT00.bmp companion exists. Test still passes if decode
// succeeded - pixels just unverified.
var errNoReference = errors.New("no reference bitmap available")

// loadConformanceReference picks canonical reference for JBIG2
// fixture. <base>_TT<N>.jb2 maps to <base>_TT<N>_TT00.bmp; else
// fall back to <base>.bmp; else errNoReference.
func loadConformanceReference(jb2Path string) (*referenceBitmap, error) {
	dir := filepath.Dir(jb2Path)
	base := filepath.Base(jb2Path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	candidates := []string{
		filepath.Join(dir, stem+"_TT00.bmp"),
		filepath.Join(dir, stem+".bmp"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return loadReference(c)
		}
	}
	return nil, errNoReference
}

// codecOutputAsReference normalizes codec output (image.Image from
// gobig2.Decoder.Decode) to packed reference bitmap for byte-by-byte
// compare to loaded reference.
func codecOutputAsReference(img image.Image) (*referenceBitmap, error) {
	if img == nil {
		return nil, errors.New("nil image")
	}
	return imageToReference(img), nil
}

// serenityOSReferenceFixtures: growing reference subset. For
// representative fixtures across distinct decode paths, pin
// first-page dimensions + packed-bits SHA-256 so subtle arithmetic
// / MMR / halftone / refinement / composite regression producing
// plausible-but-wrong bitmap fails test. Fixtures not in map fall
// back to decode-success-within-budget. Grow incrementally vs
// solving all 107 at once.
//
// Hashes from known-good decode. Add entry: extract hash with same
// shape, verify bitmap independently, commit.
//
// 3078164f... canonical hash covers most fixtures exercising
// specific encoding mode but producing same source bitmap; other
// entries pin variants whose decoded output differs.
var serenityOSReferenceFixtures = map[string]struct {
	width, height int
	sha256        string
}{
	"bitmap.jbig2":                                 {399, 400, "3078164fcd0a780514ab3752428767aa519398c007c13ffadc62cd2ef1a6c50d"},
	"bitmap-mmr.jbig2":                             {399, 400, "3078164fcd0a780514ab3752428767aa519398c007c13ffadc62cd2ef1a6c50d"},
	"bitmap-halftone.jbig2":                        {399, 400, "3078164fcd0a780514ab3752428767aa519398c007c13ffadc62cd2ef1a6c50d"},
	"bitmap-refine.jbig2":                          {399, 400, "3078164fcd0a780514ab3752428767aa519398c007c13ffadc62cd2ef1a6c50d"},
	"bitmap-composite-and-xnor.jbig2":              {399, 400, "3078164fcd0a780514ab3752428767aa519398c007c13ffadc62cd2ef1a6c50d"},
	"annex-h.jbig2":                                {64, 56, "975e63be32f6dd9c4367dd25ae268cd5701b888717656236c98c31ee8bb35db4"},
	"bitmap-symbol-texttoprighttranspose.jbig2":    {399, 400, "0f157ec83ade0ca2c3bb34cecccdc4472e7c5ad8a25f52e9c11d1c8a88cc33f0"},
	"bitmap-symbol-textbottomlefttranspose.jbig2":  {399, 400, "6498f4e5bbd623c212291c99b63ab4a7082d96d0ee04b7af9f0c23b2943b2eac"},
	"bitmap-symbol-textbottomright.jbig2":          {399, 400, "ca28da3d30f9d68603e26fc8f6e69ac6dcda2700edf2c8ec1f5ee6feaa76bc5a"},
	"bitmap-symbol-textbottomrighttranspose.jbig2": {399, 400, "cbc6b62af4c7f03d485b8d31fc479ad00bf165d2313ce217e2785590b860ccc6"},
}

// TestSerenityOSCorpus walks bundled JBIG2 test inputs. 107 .jbig2
// fixtures under testdata/serenityos/; env var overrides path.
//
// Oracle tiering: fixtures in serenityOSReferenceFixtures pin
// dimensions + packed-bits SHA-256, so regression producing wrong
// pixels fails test even if decode succeeded. Others only assert
// decode-succeeds-within-budget. Growing reference set is
// incremental, not a blocker on corpus walk.
func TestSerenityOSCorpus(t *testing.T) {
	dir := os.Getenv(envSerenityOS)
	if dir == "" {
		dir = defaultSerenityOSCorpus
	}
	fixtures, err := listFixtures(dir, ".jb2", ".jbig2")
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Skipf("no .jbig2 fixtures under %s", dir)
	}

	summary := newCorpusSummary(t)
	defer summary.report()

	for _, path := range fixtures {
		path := path
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				summary.add(name, false, err.Error())
				t.Fatalf("read fixture: %v", err)
			}
			res := decodeWithBudget(data, nil, decodeTimeout, decodeMemory)
			if res.err != nil {
				summary.add(name, false, res.err.Error())
				t.Errorf("decode failed (%s, peak %d MiB): %v", res.elapsed, res.peakMB, res.err)
				return
			}
			// Pixel oracle: fires only for fixtures in reference
			// subset. Others stay decode-only.
			if ref, ok := serenityOSReferenceFixtures[name]; ok {
				g, gOk := res.img.(*image.Gray)
				if !gOk {
					summary.add(name, false, "decoded but not *image.Gray")
					t.Errorf("expected *image.Gray, got %T", res.img)
					return
				}
				if g.Bounds().Dx() != ref.width || g.Bounds().Dy() != ref.height {
					summary.add(name, false, fmt.Sprintf("size mismatch: got %dx%d want %dx%d",
						g.Bounds().Dx(), g.Bounds().Dy(), ref.width, ref.height))
					t.Errorf("dimensions: got %dx%d want %dx%d",
						g.Bounds().Dx(), g.Bounds().Dy(), ref.width, ref.height)
					return
				}
				if got := sha256OfGrayBits(g); got != ref.sha256 {
					summary.add(name, false, fmt.Sprintf("packed-bits hash %s != reference %s",
						got, ref.sha256))
					t.Errorf("packed-bits hash: got %s want %s", got, ref.sha256)
					return
				}
				summary.add(name, true, fmt.Sprintf("%dx%d hash-verified in %s", ref.width, ref.height, res.elapsed))
				return
			}
			summary.add(name, true, fmt.Sprintf("decoded in %s, peak %d MiB", res.elapsed, res.peakMB))
		})
	}
}

// corpusSummary collects per-fixture results, prints table at end
// of parent test. Bird's-eye view of corpus pass rate without
// grepping go test output.
type corpusSummary struct {
	t       *testing.T
	results []corpusResult
}

type corpusResult struct {
	name    string
	pass    bool
	message string
}

func newCorpusSummary(t *testing.T) *corpusSummary {
	return &corpusSummary{t: t}
}

func (s *corpusSummary) add(name string, pass bool, msg string) {
	s.results = append(s.results, corpusResult{name: name, pass: pass, message: msg})
}

func (s *corpusSummary) report() {
	if len(s.results) == 0 {
		return
	}
	pass, fail := 0, 0
	var b strings.Builder
	for _, r := range s.results {
		mark := "FAIL"
		if r.pass {
			mark = "PASS"
			pass++
		} else {
			fail++
		}
		fmt.Fprintf(&b, "  %s %-50s %s\n", mark, r.name, r.message)
	}
	s.t.Logf("\ncorpus summary: %d pass / %d fail / %d total\n%s",
		pass, fail, len(s.results), b.String())
}
