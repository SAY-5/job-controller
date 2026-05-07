package main

import (
	"math"
	"testing"
)

func TestPercentileEmpty(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
}

func TestPercentileMonotonic(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		p    float64
		want float64
	}{
		{0.0, 1},
		{0.5, 5.5},
		{1.0, 10},
	}
	for _, c := range cases {
		got := percentile(xs, c.p)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("p=%v got=%v want=%v", c.p, got, c.want)
		}
	}
}

func TestMaxF(t *testing.T) {
	if maxF(nil) != 0 {
		t.Fatalf("nil should yield 0")
	}
	if got := maxF([]float64{3, 1, 4, 1, 5, 9, 2, 6}); got != 9 {
		t.Fatalf("got %v want 9", got)
	}
}

func TestParseFoundLastWins(t *testing.T) {
	out := []byte(`{"type":"started"}` + "\n" + `{"type":"checkpoint","found":42}` + "\n" + `{"type":"completed","found":97,"epoch":1}`)
	if got := parseFound(out); got != 97 {
		t.Fatalf("got %v want 97", got)
	}
}

func TestParseFoundMissing(t *testing.T) {
	if got := parseFound([]byte("nothing here")); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
}
