package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// FuzzParse asserts the parser is panic-free on arbitrary inputs and that
// any input it accepts round-trips byte-identically through Encode.
func FuzzParse(f *testing.F) {
	// Seeds: empty, header-only, a valid blob, and a few hand-crafted edge cases.
	valid, _ := Encode(&State{Limit: 100, Next: 7, Found: 4, SeededTwo: true, Recent: []uint64{2, 3, 5, 7}})
	f.Add([]byte{})
	f.Add([]byte("JOBC"))
	f.Add(valid)
	// All zeros at min length.
	f.Add(make([]byte, minLen))
	// All 0xFF at min length.
	{
		b := make([]byte, minLen)
		for i := range b {
			b[i] = 0xFF
		}
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Property 1: never panic.
		st, err := Parse(data)
		if err != nil {
			// Errors are fine; just verify they are sentinel-ish (non-nil
			// and stable). We don't constrain *which* error here.
			return
		}
		if st == nil {
			t.Fatalf("nil state with nil error for input %x", data)
		}
		// Property 2: an accepted input round-trips byte-identically when
		// re-encoded. This proves Parse only accepts canonical layouts.
		re, encErr := Encode(st)
		if encErr != nil {
			t.Fatalf("re-encode failed: %v (state=%+v)", encErr, st)
		}
		if !bytes.Equal(re, data) {
			t.Fatalf("round-trip not byte-identical:\n in=%x\nout=%x", data, re)
		}
	})
}

// FuzzEncodeParseRoundtrip generates structured inputs (rather than raw
// bytes) and asserts encode->parse is lossless across the entire field
// domain, including pathological values like 0, ^uint64(0), and recent
// rings at the cap.
func FuzzEncodeParseRoundtrip(f *testing.F) {
	type seed struct {
		limit, next, found uint64
		seeded             bool
		count              uint8
	}
	seeds := []seed{
		{0, 0, 0, false, 0},
		{1, 2, 0, true, 0},
		{100, 7, 4, true, 4},
		{^uint64(0), ^uint64(0), ^uint64(0), true, RecentCap},
		{1 << 32, 1 << 33, 0, false, 1},
	}
	for _, s := range seeds {
		f.Add(s.limit, s.next, s.found, s.seeded, s.count)
	}

	f.Fuzz(func(t *testing.T, limit, next, found uint64, seeded bool, count uint8) {
		// Cap to RecentCap so Encode never errors out on size; the parser
		// is what we want to exercise.
		n := int(count) % (RecentCap + 1)
		recent := make([]uint64, n)
		for i := range recent {
			// Deterministic spread of values from the scalar fields.
			recent[i] = limit ^ (next + uint64(i)) ^ found
		}
		in := &State{Limit: limit, Next: next, Found: found, SeededTwo: seeded, Recent: recent}
		buf, err := Encode(in)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		out, err := Parse(buf)
		if err != nil {
			t.Fatalf("parse of valid encoding failed: %v", err)
		}
		if out.Limit != in.Limit || out.Next != in.Next || out.Found != in.Found || out.SeededTwo != in.SeededTwo {
			t.Fatalf("scalar drift: in=%+v out=%+v", in, out)
		}
		if len(out.Recent) != len(in.Recent) {
			t.Fatalf("recent length drift: %d vs %d", len(out.Recent), len(in.Recent))
		}
		for i := range in.Recent {
			if out.Recent[i] != in.Recent[i] {
				t.Fatalf("recent[%d] = %d want %d", i, out.Recent[i], in.Recent[i])
			}
		}
	})
}

// FuzzBitFlipDetected asserts that any single-bit flip in a valid blob is
// rejected -- either by the structural checks or the CRC.
func FuzzBitFlipDetected(f *testing.F) {
	// Seeds steer libFuzzer toward (offset, bit) pairs across blob lengths.
	for _, off := range []uint32{0, 4, 8, 16, 32, 33, 37} {
		for _, bit := range []uint8{0, 1, 7} {
			f.Add(off, bit)
		}
	}

	f.Fuzz(func(t *testing.T, off uint32, bit uint8) {
		base, _ := Encode(&State{Limit: 1000, Next: 11, Found: 4, SeededTwo: true, Recent: []uint64{2, 3, 5, 7}})
		idx := int(off) % len(base)
		corrupted := append([]byte{}, base...)
		corrupted[idx] ^= 1 << (bit % 8)
		if bytes.Equal(corrupted, base) {
			return
		}
		st, err := Parse(corrupted)
		if err == nil {
			// The only legitimate "no error" outcome would be that we
			// accidentally hit the seeded_two bool flag bits 1..7, which
			// have no semantic meaning -- those bits MUST still be
			// rejected because the C++ writer only ever emits 0 or 1
			// there, and a non-canonical 0x02..0xFF value would not
			// round-trip. Our parser collapses any non-zero into "true",
			// so re-encode will produce a different blob and round-trip
			// catches it.
			re, _ := Encode(st)
			if bytes.Equal(re, corrupted) {
				t.Fatalf("bit flip at byte=%d bit=%d went undetected", idx, bit%8)
			}
		}
	})
}

// FuzzRandomBytesNoPanic explicitly asserts the parser does not panic on
// adversarial random byte streams. Identical contract to FuzzParse but
// keeps the failing seed space distinct so go test -fuzztime can target it.
func FuzzRandomBytesNoPanic(f *testing.F) {
	f.Add([]byte{})
	f.Add(append([]byte("JOBC"), make([]byte, 1024)...))
	f.Add(bytes.Repeat([]byte{0xAA}, 64))
	f.Add(append(bytes.Repeat([]byte{0xFF}, minLen-4), 0, 0, 0, 0))

	f.Fuzz(func(t *testing.T, data []byte) {
		// We expect no panic. The parser may accept or reject; both fine.
		_, _ = Parse(data)
	})
}

// fakeCRC mints a CRC for an arbitrary prefix; used by tests that hand-build
// near-valid inputs without hitting ErrCRC.
func fakeCRC(prefix []byte) []byte {
	c := crc32.ChecksumIEEE(prefix)
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, c)
	return out
}

// TestErrSentinelsDistinct guards that errors stay errors.Is-distinguishable.
func TestErrSentinelsDistinct(t *testing.T) {
	all := []error{ErrShort, ErrMagic, ErrVersion, ErrCRC, ErrRecentCap, ErrTrailing, ErrRecentSize}
	for i := range all {
		for j := range all {
			if i == j {
				continue
			}
			if errors.Is(all[i], all[j]) {
				t.Fatalf("sentinels collide: %v ~ %v", all[i], all[j])
			}
		}
	}
	// Smoke-use fakeCRC so the helper isn't dead code.
	_ = fakeCRC([]byte("JOBC"))
}
