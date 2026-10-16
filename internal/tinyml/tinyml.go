// Package tinyml is the Go port of the edge fault classifier: fixed-point
// features (Q15 FFT, envelope spectrum, Q8 log2) and the INT8 MLP.
//
// It is bit-exact with the specification in tinyml/edgeml/ref.py and the C
// kernel in tinyml/c (checked on the whole synthetic test set by
// TestBitExactWholeTestSet). The model was trained on SYNTHETIC data from
// internal/hoistsim; see docs/tinyml.md for what that does and does not show.
package tinyml

import (
	"math"
	"math/bits"

	"industrial-edge/internal/hoistsim"
)

const (
	N         = hoistsim.N
	NFeatures = 22
	NClasses  = hoistsim.NumClasses
	log2N     = 10
	nBins     = N / 2
)

var bandEdges = [11]int{1, 4, 8, 16, 32, 64, 128, 192, 256, 384, 512}

// FeatureNames lists the features in order.
var FeatureNames = [NFeatures]string{"rms", "kurtosis", "crest", "ord1x", "ord2x", "ord3x",
	"band5_20Hz", "band20_40Hz", "band40_80Hz", "band80_160Hz", "band160_320Hz", "band320_640Hz",
	"band640_960Hz", "band960_1280Hz", "band1280_1920Hz", "band1920_2560Hz",
	"env_bpfo", "env_bpfi", "env_1x", "env_total", "current", "rpm"}

type layer struct {
	in, out      int
	zpIn, zpOut  int64
	w            []int8
	b, m0, shift []int64
}

// Result is one classification.
type Result struct {
	Features [NFeatures]int32
	Q        [NFeatures]int8
	Logits   [NClasses]int8
	Class    int
	// Confidence is the softmax maximum of the dequantized logits (float,
	// for monitoring only; the decision is the integer argmax).
	Confidence float64
}

// Log2Q is the Q8 log2 approximation (256*floor(log2 u) + 8 mantissa bits).
func Log2Q(u uint64) int32 {
	if u == 0 {
		return 0
	}
	k := 63 - bits.LeadingZeros64(u)
	var m uint64
	if k >= 8 {
		m = (u >> (k - 8)) & 255
	} else {
		m = (u << (8 - k)) & 255
	}
	return int32(k*256) + int32(m)
}

// Classifier holds the work buffers (not safe for concurrent use).
type Classifier struct {
	d      [N]int32
	re, im [N]int64
}

func (c *Classifier) fft() {
	for size := 2; size <= N; size <<= 1 {
		half, step := size>>1, N/size
		for start := 0; start < N; start += size {
			for j := 0; j < half; j++ {
				cs, sn := int64(cosQ15[j*step]), int64(sinQ15[j*step])
				a, b := start+j, start+j+half
				br, bi := c.re[b], c.im[b]
				tr := (br*cs + bi*sn + 16384) >> 15
				ti := (bi*cs - br*sn + 16384) >> 15
				ar, ai := c.re[a], c.im[a]
				c.re[a], c.im[a] = ar+tr, ai+ti
				c.re[b], c.im[b] = ar-tr, ai-ti
			}
		}
	}
}

func (c *Classifier) pw(k int) uint64 {
	return uint64(c.re[k]*c.re[k]) + uint64(c.im[k]*c.im[k])
}

func (c *Classifier) orderEnergy(rpm uint16, oq8 uint64) uint64 {
	const den = 60 * 5120 * 256
	b := int((uint64(rpm)*oq8*N + den/2) / den)
	var e uint64
	for k := b - 1; k <= b+1; k++ {
		if k >= 1 && k <= nBins-1 {
			e += c.pw(k)
		}
	}
	return e
}

// Features computes the 22 Q8 features of one window.
func (c *Classifier) Features(x *[N]int16, rpm, cur uint16) [NFeatures]int32 {
	var f [NFeatures]int32
	var sum int32
	for _, v := range x {
		sum += int32(v)
	}
	m := sum >> log2N
	var s2, s4 uint64
	var pk uint32
	for n, xv := range x {
		v := int32(xv) - m
		v = max(-32768, min(32767, v))
		c.d[n] = v
		e := uint32(v * v)
		s2 += uint64(e)
		e15 := uint64(e >> 15)
		s4 += e15 * e15
		a := uint32(v)
		if v < 0 {
			a = uint32(-v)
		}
		pk = max(pk, a)
	}
	l2 := Log2Q(s2)
	f[0] = l2 - log2N*256
	f[1] = Log2Q(s4) - 2*l2 + 40*256
	f[2] = Log2Q(uint64(pk)*uint64(pk)) - l2 + log2N*256
	for i := 0; i < N; i++ {
		n := bitrev[i]
		c.re[i] = (int64(c.d[n])*int64(hannQ15[n]) + 16384) >> 15
		c.im[i] = 0
	}
	c.fft()
	f[3] = Log2Q(c.orderEnergy(rpm, 256))
	f[4] = Log2Q(c.orderEnergy(rpm, 512))
	f[5] = Log2Q(c.orderEnergy(rpm, 768))
	for b := 0; b < 10; b++ {
		var e uint64
		for k := bandEdges[b]; k < bandEdges[b+1]; k++ {
			e += c.pw(k)
		}
		f[6+b] = Log2Q(e)
	}
	abs := func(v int32) int32 {
		if v < 0 {
			return -v
		}
		return v
	}
	var esum int32
	for n := 1; n < N; n++ {
		esum += abs(c.d[n] - c.d[n-1])
	}
	em := esum >> log2N
	for i := 0; i < N; i++ {
		n := bitrev[i]
		var h int32
		if n > 0 {
			h = c.d[n] - c.d[n-1]
		}
		c.re[i] = (int64(abs(h)-em)*int64(hannQ15[n]) + 16384) >> 15
		c.im[i] = 0
	}
	c.fft()
	f[16] = Log2Q(c.orderEnergy(rpm, 916))
	f[17] = Log2Q(c.orderEnergy(rpm, 1388))
	f[18] = Log2Q(c.orderEnergy(rpm, 256))
	var et uint64
	for k := 1; k < nBins; k++ {
		et += c.pw(k)
	}
	f[19] = Log2Q(et)
	f[20] = Log2Q(uint64(cur))
	f[21] = Log2Q(uint64(rpm))
	return f
}

// Quantize maps features to the int8 model inputs.
func Quantize(f *[NFeatures]int32) [NFeatures]int8 {
	var q [NFeatures]int8
	for i := range f {
		v := ((int64(f[i])-featLo[i])*featMul[i] + 32768) >> 16
		q[i] = int8(max(0, min(255, v)) - 128)
	}
	return q
}

func requant(acc, m0, shift, zp int64) int64 {
	s := 31 + shift
	return max(-128, min(127, zp+((acc*m0+(int64(1)<<(s-1)))>>s)))
}

// MLP runs the INT8 network.
func MLP(q *[NFeatures]int8) [NClasses]int8 {
	a := make([]int64, NFeatures)
	for i, v := range q {
		a[i] = int64(v)
	}
	for _, L := range layers {
		out := make([]int64, L.out)
		for o := 0; o < L.out; o++ {
			acc := L.b[o]
			w := L.w[o*L.in : (o+1)*L.in]
			for i := 0; i < L.in; i++ {
				acc += (a[i] - L.zpIn) * int64(w[i])
			}
			out[o] = requant(acc, L.m0[o], L.shift[o], L.zpOut)
		}
		a = out
	}
	var l [NClasses]int8
	for i := range l {
		l[i] = int8(a[i])
	}
	return l
}

// Classify runs the whole pipeline on one window.
func (c *Classifier) Classify(x *[N]int16, rpm, cur uint16) Result {
	var r Result
	r.Features = c.Features(x, rpm, cur)
	r.Q = Quantize(&r.Features)
	r.Logits = MLP(&r.Q)
	for i := 1; i < NClasses; i++ {
		if r.Logits[i] > r.Logits[r.Class] {
			r.Class = i
		}
	}
	var z [NClasses]float64
	mx := math.Inf(-1)
	for i, l := range r.Logits {
		z[i] = float64(int64(l)-LogitZP) * LogitScale
		mx = math.Max(mx, z[i])
	}
	var s float64
	for i := range z {
		z[i] = math.Exp(z[i] - mx)
		s += z[i]
	}
	r.Confidence = z[r.Class] / s
	return r
}
