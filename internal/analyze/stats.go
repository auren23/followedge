// Package analyze implements the research CLI: it turns collected events,
// cluster samples and markouts into the tables that decide what (if anything)
// is copyable.
package analyze

import (
	"math"
	"sort"
)

// percentile returns the p-th percentile (0-100) with linear interpolation
// between ranked values (numpy-style), on a copy of a.
func percentile(a []float64, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	sort.Float64s(a)
	pos := p / 100 * float64(len(a)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return a[lo]
	}
	return a[lo] + (a[hi]-a[lo])*(pos-float64(lo))
}

func median(a []float64) float64 { return percentile(a, 50) }

func mean(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	var s float64
	for _, v := range a {
		s += v
	}
	return s / float64(len(a))
}

// Bucket returns the chase/return bucket label for a return percentage.
func Bucket(r float64) string {
	switch {
	case r < 0:
		return "<0%"
	case r < 2:
		return "0-2%"
	case r < 5:
		return "2-5%"
	case r < 10:
		return "5-10%"
	case r < 20:
		return "10-20%"
	default:
		return "20%+"
	}
}
