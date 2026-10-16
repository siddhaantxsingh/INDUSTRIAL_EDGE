package tinyml

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"industrial-edge/internal/hoistsim"
)

// model.json records the SHA-256 of the integer outputs (features, int8
// inputs, logits) that the Python reference produced for the whole test set.
// The C kernel (host and QEMU Cortex-M) is compared with the same stream by
// tinyml/run_all.sh. Here the Go port regenerates the test set with the same
// seed and must produce the identical byte stream.
func TestBitExactWholeTestSet(t *testing.T) {
	b, err := os.ReadFile("../../tinyml/model/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Sha     string  `json:"test_expected_sha256"`
		AccInt8 float64 `json:"acc_int8"`
	}
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatal(err)
	}
	_, test := hoistsim.Dataset(1, 1500, 500)
	h := sha256.New()
	var c Classifier
	correct := 0
	var buf [4]byte
	for _, w := range test {
		r := c.Classify(&w.X, w.RPM, w.Current)
		for _, f := range r.Features {
			binary.LittleEndian.PutUint32(buf[:], uint32(f))
			h.Write(buf[:])
		}
		for _, q := range r.Q {
			h.Write([]byte{byte(q)})
		}
		for _, l := range r.Logits {
			h.Write([]byte{byte(l)})
		}
		if r.Class == w.Cond.Class {
			correct++
		}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != info.Sha {
		t.Fatalf("Go outputs differ from the Python reference: sha256 %s, want %s", got, info.Sha)
	}
	acc := float64(correct) / float64(len(test))
	if acc != info.AccInt8 {
		t.Fatalf("accuracy %v, Python int8 %v", acc, info.AccInt8)
	}
	t.Logf("%d windows bit-exact; int8 accuracy %.4f", len(test), acc)
}

func TestLog2Q(t *testing.T) {
	cases := map[uint64]int32{0: 0, 1: 0, 2: 256, 3: 256 + 128, 255: 7*256 + 254, 256: 2048, 1 << 40: 40 * 256,
		(1 << 40) + (1 << 39): 40*256 + 128, ^uint64(0): 63*256 + 255}
	for u, want := range cases {
		if got := Log2Q(u); got != want {
			t.Errorf("Log2Q(%d) = %d, want %d", u, got, want)
		}
	}
}

func BenchmarkClassify(b *testing.B) {
	r := hoistsim.Generate(newRand(), hoistsim.RandomCondition(newRand(), hoistsim.OuterRace, 1200, 1201, 0.5, 0.6))
	var c Classifier
	for i := 0; i < b.N; i++ {
		c.Classify(&r.X, r.RPM, r.Current)
	}
}
