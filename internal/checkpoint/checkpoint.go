// Package checkpoint provides a Go-side reader for the binary checkpoint
// format produced by the C++ worker. The format is identical to the layout
// declared in worker/src/checkpoint.h:
//
//	[u32 magic = 0x4A4F4243 ("JOBC")]
//	[u32 version = 2]
//	[u64 limit] [u64 next] [u64 found]
//	[u8  seeded_two]
//	[u32 recent_count] [recent_count * u64 recent values]
//	[u32 crc32 (IEEE)]
//
// The parser is structurally validating: any malformed input produces an
// error rather than a panic. It is fuzzed by checkpoint_fuzz_test.go to
// guarantee that property even on adversarial inputs.
package checkpoint

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Magic is the four-byte file signature ("JOBC", little-endian u32).
const Magic uint32 = 0x4A4F4243

// Version is the on-disk schema version. Bump when the layout changes.
const Version uint32 = 2

// RecentCap mirrors SieveState::kRecentCap in the C++ worker.
const RecentCap = 32

// Header errors. They are sentinel values so callers can switch on them.
var (
	ErrShort      = errors.New("checkpoint: input too short")
	ErrMagic      = errors.New("checkpoint: bad magic")
	ErrVersion    = errors.New("checkpoint: bad version")
	ErrCRC        = errors.New("checkpoint: crc mismatch")
	ErrRecentCap  = errors.New("checkpoint: recent ring exceeds cap")
	ErrTrailing   = errors.New("checkpoint: trailing bytes after parse")
	ErrRecentSize = errors.New("checkpoint: recent count overflows buffer")
)

// State is the parsed view of a checkpoint file. It mirrors SieveState in
// the worker minus the in-process epoch counter (which is intentionally not
// persisted; see worker/src/checkpoint.h).
type State struct {
	Limit     uint64
	Next      uint64
	Found     uint64
	SeededTwo bool
	Recent    []uint64
}

// minLen is the minimum valid file length: header + zero-length recent ring + crc.
const minLen = 4 /*magic*/ + 4 /*version*/ + 8*3 /*limit/next/found*/ + 1 /*seeded*/ + 4 /*count*/ + 4 /*crc*/

// Parse decodes a checkpoint blob. It validates magic, version, framing, and
// CRC before returning. It never panics on malformed input.
func Parse(buf []byte) (*State, error) {
	if len(buf) < minLen {
		return nil, ErrShort
	}
	// CRC covers everything except the trailing 4-byte CRC itself.
	crcOff := len(buf) - 4
	want := binary.LittleEndian.Uint32(buf[crcOff:])
	got := crc32.ChecksumIEEE(buf[:crcOff])
	if want != got {
		return nil, ErrCRC
	}

	p := buf[:crcOff]
	if binary.LittleEndian.Uint32(p[0:4]) != Magic {
		return nil, ErrMagic
	}
	if binary.LittleEndian.Uint32(p[4:8]) != Version {
		return nil, ErrVersion
	}
	s := &State{
		Limit: binary.LittleEndian.Uint64(p[8:16]),
		Next:  binary.LittleEndian.Uint64(p[16:24]),
		Found: binary.LittleEndian.Uint64(p[24:32]),
	}
	s.SeededTwo = p[32] != 0
	count := binary.LittleEndian.Uint32(p[33:37])
	if count > RecentCap {
		return nil, ErrRecentCap
	}
	bodyOff := 37
	wantBytes := int(count) * 8
	if bodyOff+wantBytes != len(p) {
		// Either too short for the declared count, or trailing bytes.
		if bodyOff+wantBytes > len(p) {
			return nil, ErrRecentSize
		}
		return nil, ErrTrailing
	}
	s.Recent = make([]uint64, count)
	for i := uint32(0); i < count; i++ {
		s.Recent[i] = binary.LittleEndian.Uint64(p[bodyOff+int(i)*8:])
	}
	return s, nil
}

// Encode produces a valid checkpoint blob for s. It is the inverse of Parse
// for the persisted fields and is used by tests/fuzz to round-trip values.
func Encode(s *State) ([]byte, error) {
	if len(s.Recent) > RecentCap {
		return nil, fmt.Errorf("encode: recent ring length %d exceeds cap %d", len(s.Recent), RecentCap)
	}
	out := make([]byte, 0, minLen+len(s.Recent)*8)
	var u4 [4]byte
	var u8 [8]byte
	binary.LittleEndian.PutUint32(u4[:], Magic)
	out = append(out, u4[:]...)
	binary.LittleEndian.PutUint32(u4[:], Version)
	out = append(out, u4[:]...)
	binary.LittleEndian.PutUint64(u8[:], s.Limit)
	out = append(out, u8[:]...)
	binary.LittleEndian.PutUint64(u8[:], s.Next)
	out = append(out, u8[:]...)
	binary.LittleEndian.PutUint64(u8[:], s.Found)
	out = append(out, u8[:]...)
	if s.SeededTwo {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(u4[:], uint32(len(s.Recent)))
	out = append(out, u4[:]...)
	for _, v := range s.Recent {
		binary.LittleEndian.PutUint64(u8[:], v)
		out = append(out, u8[:]...)
	}
	c := crc32.ChecksumIEEE(out)
	binary.LittleEndian.PutUint32(u4[:], c)
	out = append(out, u4[:]...)
	return out, nil
}
