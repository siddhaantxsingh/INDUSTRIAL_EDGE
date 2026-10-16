// vibgen writes the SYNTHETIC hoist vibration dataset used to train and test
// the TinyML fault classifier (see internal/hoistsim).
//
//	vibgen -out tinyml/data -train 1500 -test 500 -seed 1
//
// Operating conditions are held out: training windows use motor speeds in
// [600, 1000) and [1300, 1750) rpm; test windows use [1000, 1300) rpm, a
// speed band the model never sees during training or calibration.
//
// Record format (little-endian, 2064 bytes): u8 label, u8 0, u16 rpm,
// u16 current (0.01 A), u16 0, f32 severity, f32 load, 1024 x i16 samples.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"log"
	"math"
	"os"
	"path/filepath"

	"industrial-edge/internal/hoistsim"
)

func write(path string, ws []hoistsim.Window) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	var hdr [16]byte
	for _, w := range ws {
		hdr[0] = byte(w.Cond.Class)
		binary.LittleEndian.PutUint16(hdr[2:], w.RPM)
		binary.LittleEndian.PutUint16(hdr[4:], w.Current)
		binary.LittleEndian.PutUint32(hdr[8:], math.Float32bits(float32(w.Cond.Severity)))
		binary.LittleEndian.PutUint32(hdr[12:], math.Float32bits(float32(w.Cond.Load)))
		bw.Write(hdr[:])
		if err := binary.Write(bw, binary.LittleEndian, w.X[:]); err != nil {
			log.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		log.Fatal(err)
	}
	f.Close()
}

func main() {
	out := flag.String("out", "tinyml/data", "output directory")
	nTrain := flag.Int("train", 1500, "training windows per class")
	nTest := flag.Int("test", 500, "test windows per class")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()
	train, test := hoistsim.Dataset(*seed, *nTrain, *nTest)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	write(filepath.Join(*out, "train.bin"), train)
	write(filepath.Join(*out, "test.bin"), test)
	log.Printf("wrote %d train and %d test windows to %s", len(train), len(test), *out)
}
