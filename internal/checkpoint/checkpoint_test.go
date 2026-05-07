package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestEncodeParseRoundtrip(t *testing.T) {
	in := &State{
		Limit:     1000,
		Next:      17,
		Found:     6,
		SeededTwo: true,
		Recent:    []uint64{2, 3, 5, 7, 11, 13},
	}
	buf, err := Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := Parse(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Limit != in.Limit || out.Next != in.Next || out.Found != in.Found || out.SeededTwo != in.SeededTwo {
		t.Fatalf("scalar mismatch: %+v vs %+v", in, out)
	}
	if len(out.Recent) != len(in.Recent) {
		t.Fatalf("recent length mismatch: %d vs %d", len(out.Recent), len(in.Recent))
	}
	for i := range in.Recent {
		if out.Recent[i] != in.Recent[i] {
			t.Fatalf("recent[%d] = %d want %d", i, out.Recent[i], in.Recent[i])
		}
	}
}

func TestEncodeMatchesCXXLayout(t *testing.T) {
	// Hand-build a known-good blob that any correct C++ reader must accept.
	// Layout:
	//   4B magic | 4B version | 8B limit=50 | 8B next=7 | 8B found=4
	//   1B seeded=1 | 4B count=4 | 4*8B recent | 4B crc
	want := bytes.Buffer{}
	wb := func(v any) {
		_ = binary.Write(&want, binary.LittleEndian, v)
	}
	wb(Magic)
	wb(Version)
	wb(uint64(50))
	wb(uint64(7))
	wb(uint64(4))
	want.WriteByte(1)
	wb(uint32(4))
	wb(uint64(2))
	wb(uint64(3))
	wb(uint64(5))
	wb(uint64(7))
	wb(crc32.ChecksumIEEE(want.Bytes()))

	got, err := Encode(&State{Limit: 50, Next: 7, Found: 4, SeededTwo: true, Recent: []uint64{2, 3, 5, 7}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(want.Bytes(), got) {
		t.Fatalf("layout mismatch:\nwant=%x\n got=%x", want.Bytes(), got)
	}
}

func TestParseRejectsShort(t *testing.T) {
	if _, err := Parse([]byte{0x01, 0x02}); !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v want ErrShort", err)
	}
}

func TestParseRejectsBadMagic(t *testing.T) {
	in := &State{Limit: 1, Next: 2, Found: 0}
	buf, _ := Encode(in)
	// Stomp magic, recompute crc so we cleanly hit ErrMagic (not ErrCRC).
	binary.LittleEndian.PutUint32(buf[0:4], 0xDEADBEEF)
	c := crc32.ChecksumIEEE(buf[:len(buf)-4])
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], c)
	if _, err := Parse(buf); !errors.Is(err, ErrMagic) {
		t.Fatalf("err = %v want ErrMagic", err)
	}
}

func TestParseRejectsBadVersion(t *testing.T) {
	in := &State{Limit: 1, Next: 2, Found: 0}
	buf, _ := Encode(in)
	binary.LittleEndian.PutUint32(buf[4:8], 99)
	c := crc32.ChecksumIEEE(buf[:len(buf)-4])
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], c)
	if _, err := Parse(buf); !errors.Is(err, ErrVersion) {
		t.Fatalf("err = %v want ErrVersion", err)
	}
}

func TestParseRejectsBadCRC(t *testing.T) {
	buf, _ := Encode(&State{Limit: 1, Next: 2, Found: 0})
	buf[len(buf)-1] ^= 0xFF
	if _, err := Parse(buf); !errors.Is(err, ErrCRC) {
		t.Fatalf("err = %v want ErrCRC", err)
	}
}

func TestParseRejectsRecentCapOverflow(t *testing.T) {
	// Build a buffer that claims 9999 recent entries but only has zero bytes.
	// The recent-count check must fire before any out-of-bounds read.
	buf := make([]byte, minLen)
	binary.LittleEndian.PutUint32(buf[0:4], Magic)
	binary.LittleEndian.PutUint32(buf[4:8], Version)
	binary.LittleEndian.PutUint32(buf[33:37], 9999)
	c := crc32.ChecksumIEEE(buf[:len(buf)-4])
	binary.LittleEndian.PutUint32(buf[len(buf)-4:], c)
	if _, err := Parse(buf); !errors.Is(err, ErrRecentCap) {
		t.Fatalf("err = %v want ErrRecentCap", err)
	}
}

func TestParseRejectsTrailingBytes(t *testing.T) {
	buf, _ := Encode(&State{Limit: 1, Next: 2, Found: 0, Recent: []uint64{2, 3}})
	// Splice an extra zero byte before the crc, then recompute crc.
	body := append([]byte{}, buf[:len(buf)-4]...)
	body = append(body, 0)
	c := crc32.ChecksumIEEE(body)
	cb := make([]byte, 4)
	binary.LittleEndian.PutUint32(cb, c)
	bad := append(body, cb...)
	if _, err := Parse(bad); err == nil {
		t.Fatalf("expected error on trailing bytes, got nil")
	}
}
