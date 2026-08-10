package analyze

import "testing"

func TestPercentile(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(a, 50); got != 5.5 {
		t.Errorf("P50 = %v, want 5.5", got)
	}
	if got := percentile(a, 90); got != 9.1 {
		t.Errorf("P90 = %v, want 9.1", got)
	}
	if got := percentile(a, 100); got != 10 {
		t.Errorf("P100 = %v, want 10", got)
	}
	if got := percentile(a, 0); got != 1 {
		t.Errorf("P0 = %v, want 1", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestMedianOdd(t *testing.T) {
	a := []float64{5, 1, 3}
	if got := median(a); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
}

func TestBucket(t *testing.T) {
	cases := map[float64]string{
		-5: "<0%", 0: "0-2%", 1.9: "0-2%", 2: "2-5%", 4.9: "2-5%",
		5: "5-10%", 9.9: "5-10%", 10: "10-20%", 19.9: "10-20%", 20: "20%+", 999: "20%+",
	}
	for r, want := range cases {
		if got := Bucket(r); got != want {
			t.Errorf("Bucket(%v) = %q, want %q", r, got, want)
		}
	}
}
