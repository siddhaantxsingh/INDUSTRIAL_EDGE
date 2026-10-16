package queue

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func payload(i int) []byte {
	return []byte(fmt.Sprintf("sample-%06d-%s", i, bytes.Repeat([]byte("x"), i%37)))
}

func TestAppendReadAckAcrossSegmentsAndReopen(t *testing.T) {
	d := t.TempDir()
	q, err := Open(d, Options{SegmentBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		off, err := q.Append(payload(i))
		if err != nil || off != uint64(i) {
			t.Fatalf("append %d: off=%d err=%v", i, off, err)
		}
	}
	recs, _ := q.Read(0, 1000)
	if len(recs) != 300 {
		t.Fatalf("read %d", len(recs))
	}
	if err := q.Ack(120); err != nil {
		t.Fatal(err)
	}
	nseg := len(q.segs)
	q.Close()
	q2, err := Open(d, Options{SegmentBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if q2.AckedOffset() != 120 || q2.NextOffset() != 300 || q2.Backlog() != 180 {
		t.Fatalf("reopen: ack=%d next=%d", q2.AckedOffset(), q2.NextOffset())
	}
	recs, _ = q2.Read(0, 1000)
	if len(recs) != 180 || recs[0].Offset != 120 || !bytes.Equal(recs[0].Payload, payload(120)) {
		t.Fatalf("after reopen: %d records, first %d", len(recs), recs[0].Offset)
	}
	if len(q2.segs) > nseg {
		t.Fatal("segments grew on reopen")
	}
	// fully acked segments are deleted
	if err := q2.Ack(300); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(d)
	segs := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".log" {
			segs++
		}
	}
	if segs != 1 {
		t.Fatalf("%d segment files left after full ack", segs)
	}
}

// Crash during append: cut the active segment at every byte; reopening must
// recover exactly the complete records (a prefix) and keep accepting appends.
func TestTornTailAtEveryByte(t *testing.T) {
	src := t.TempDir()
	q, _ := Open(src, Options{SegmentBytes: 1 << 20})
	var sizes []int64
	for i := 0; i < 20; i++ {
		q.Append(payload(i))
		sizes = append(sizes, q.segs[0].size)
	}
	q.Close()
	full, _ := os.ReadFile(q.segs[0].path)
	for cut := 0; cut <= len(full); cut++ {
		d := t.TempDir()
		name := filepath.Base(q.segs[0].path)
		os.WriteFile(filepath.Join(d, name), full[:cut], 0o644)
		r, err := Open(d, Options{SegmentBytes: 1 << 20})
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		want := 0
		for _, s := range sizes {
			if int64(cut) >= s {
				want++
			}
		}
		recs, _ := r.Read(0, 100)
		if len(recs) != want {
			t.Fatalf("cut %d: recovered %d records, want %d", cut, len(recs), want)
		}
		for i, rec := range recs {
			if !bytes.Equal(rec.Payload, payload(i)) {
				t.Fatalf("cut %d: record %d corrupted", cut, i)
			}
		}
		if off, err := r.Append(payload(99)); err != nil || off != uint64(want) {
			t.Fatalf("cut %d: append after recovery off=%d err=%v", cut, off, err)
		}
		r.Close()
	}
}

func TestZeroFilledTailIsTruncated(t *testing.T) {
	d := t.TempDir()
	q, _ := Open(d, Options{})
	q.Append(payload(1))
	q.Append(payload(2))
	q.Close()
	f, _ := os.OpenFile(q.segs[0].path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(make([]byte, 4096))
	f.Close()
	r, err := Open(d, Options{})
	if err != nil || r.NextOffset() != 2 || r.Truncated != 4096 {
		t.Fatalf("next=%d truncated=%d err=%v", r.NextOffset(), r.Truncated, err)
	}
}

func TestCorruptionInOlderSegmentIsAnError(t *testing.T) {
	d := t.TempDir()
	q, _ := Open(d, Options{SegmentBytes: 256})
	for i := 0; i < 50; i++ {
		q.Append(payload(i))
	}
	q.Close()
	b, _ := os.ReadFile(q.segs[0].path)
	b[20] ^= 0xFF
	os.WriteFile(q.segs[0].path, b, 0o644)
	if _, err := Open(d, Options{SegmentBytes: 256}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
}

func TestSizeCapDropsOldestAndCounts(t *testing.T) {
	d := t.TempDir()
	q, _ := Open(d, Options{SegmentBytes: 1024, MaxBytes: 4096})
	for i := 0; i < 1000; i++ {
		if _, err := q.Append(payload(i)); err != nil {
			t.Fatal(err)
		}
	}
	if q.DiskBytes() > 4096+1024 {
		t.Fatalf("disk %d over budget", q.DiskBytes())
	}
	if q.Dropped == 0 || q.Dropped+q.Backlog() != 1000 {
		t.Fatalf("dropped %d + backlog %d != 1000", q.Dropped, q.Backlog())
	}
	recs, _ := q.Read(0, 10000)
	if uint64(len(recs)) != q.Backlog() || recs[len(recs)-1].Offset != 999 {
		t.Fatalf("read %d, backlog %d", len(recs), q.Backlog())
	}
	q.Close()
	r, _ := Open(d, Options{SegmentBytes: 1024, MaxBytes: 4096})
	if r.AckedOffset() != q.AckedOffset() {
		t.Fatalf("reopen: dropped region not skipped (%d vs %d)", r.AckedOffset(), q.AckedOffset())
	}
}
