// Package queue is the gateway's durable store-and-forward buffer: an
// append-only log split into segment files, with a separately persisted
// "acknowledged up to" offset.
//
//   - Records are framed as [len u32][crc32c u32][payload] and carry a
//     monotonically increasing 64-bit sequence number (the offset).
//   - Append writes to the active segment. Sync() fsyncs it; the gateway
//     calls Sync on a timer (group commit), so a crash loses at most one
//     sync interval of samples. SyncEvery=1 fsyncs every append.
//   - Read returns records after the acknowledged offset without removing
//     them; Ack(offset) persists the new offset (write + fsync + rename) and
//     deletes segments that are entirely acknowledged.
//   - On open, every segment is scanned: a torn or zero-filled tail of the
//     LAST segment is truncated (crash during append); a bad record anywhere
//     else is reported as corruption.
//   - MaxBytes bounds the disk used. When exceeded, the oldest whole segments
//     are deleted even if unacknowledged, and Dropped counts the records
//     lost. Industrial gateways usually prefer losing the oldest data to
//     stopping acquisition; the counter makes the loss visible.
package queue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is returned when a non-final segment contains a bad record.
var ErrCorrupt = errors.New("queue: corrupt segment")

const (
	hdrLen    = 8
	maxRecord = 16 << 20
	segPrefix = "seg-"
	segSuffix = ".log"
	ackFile   = "ack"
)

// Options configures a queue.
type Options struct {
	SegmentBytes int64 // roll to a new segment after this size (default 4 MiB)
	MaxBytes     int64 // total disk budget; 0 = unlimited
	SyncEvery    int   // fsync after this many appends (0 = only on Sync())
}

// Record is one queued payload with its offset.
type Record struct {
	Offset  uint64
	Payload []byte
}

type segment struct {
	base  uint64 // offset of the first record
	path  string
	size  int64
	count uint64
}

// Queue is safe for concurrent use.
type Queue struct {
	dir      string
	opt      Options
	mu       sync.Mutex
	segs     []*segment
	f        *os.File // active segment
	next     uint64   // offset of the next appended record
	ack      uint64   // all offsets < ack are acknowledged
	unsynced int

	// observability
	Dropped   uint64 // records deleted unacknowledged because of MaxBytes
	Truncated int64  // bytes cut from a torn tail on open
}

// Open opens (or creates) the queue in dir.
func Open(dir string, opt Options) (*Queue, error) {
	if opt.SegmentBytes == 0 {
		opt.SegmentBytes = 4 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	q := &Queue{dir: dir, opt: opt}
	if b, err := os.ReadFile(filepath.Join(dir, ackFile)); err == nil {
		v, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("queue: bad ack file: %w", perr)
		}
		q.ack = v
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) {
			base, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(n, segPrefix), segSuffix), 10, 64)
			if err != nil {
				continue
			}
			q.segs = append(q.segs, &segment{base: base, path: filepath.Join(dir, n)})
		}
	}
	sort.Slice(q.segs, func(i, j int) bool { return q.segs[i].base < q.segs[j].base })
	for i, s := range q.segs {
		last := i == len(q.segs)-1
		count, good, total, err := scan(s.path)
		if err != nil {
			return nil, err
		}
		if good < total {
			if !last {
				return nil, fmt.Errorf("%w: %s at byte %d", ErrCorrupt, s.path, good)
			}
			if err := os.Truncate(s.path, good); err != nil {
				return nil, err
			}
			q.Truncated += total - good
		}
		s.size, s.count = good, count
		if i > 0 {
			prev := q.segs[i-1]
			if prev.base+prev.count != s.base {
				return nil, fmt.Errorf("%w: gap between %s and %s", ErrCorrupt, prev.path, s.path)
			}
		}
	}
	if len(q.segs) > 0 {
		l := q.segs[len(q.segs)-1]
		q.next = l.base + l.count
	} else {
		q.next = q.ack
	}
	if q.ack > q.next {
		return nil, fmt.Errorf("%w: ack offset %d beyond end %d", ErrCorrupt, q.ack, q.next)
	}
	if len(q.segs) > 0 && q.segs[0].base > q.ack {
		// records were dropped by the size cap while unacknowledged
		q.ack = q.segs[0].base
	}
	if err := q.openActive(); err != nil {
		return nil, err
	}
	return q, nil
}

// scan validates a segment; returns record count, bytes of valid prefix, file size.
func scan(path string) (uint64, int64, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	var off int64
	var n uint64
	for int(off) < len(b) {
		rest := b[off:]
		if len(rest) < hdrLen {
			break
		}
		ln := binary.LittleEndian.Uint32(rest)
		crc := binary.LittleEndian.Uint32(rest[4:])
		if ln == 0 || ln > maxRecord || int(ln) > len(rest)-hdrLen {
			break
		}
		if crc32.Checksum(rest[hdrLen:hdrLen+int(ln)], castagnoli) != crc {
			break
		}
		off += hdrLen + int64(ln)
		n++
	}
	return n, off, int64(len(b)), nil
}

func (q *Queue) openActive() error {
	if len(q.segs) == 0 || q.segs[len(q.segs)-1].size >= q.opt.SegmentBytes {
		s := &segment{base: q.next, path: filepath.Join(q.dir, fmt.Sprintf("%s%020d%s", segPrefix, q.next, segSuffix))}
		q.segs = append(q.segs, s)
	}
	s := q.segs[len(q.segs)-1]
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	q.f = f
	return syncDir(q.dir)
}

func syncDir(d string) error {
	f, err := os.Open(d)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Append adds a record and returns its offset.
func (q *Queue) Append(payload []byte) (uint64, error) {
	if len(payload) == 0 || len(payload) > maxRecord {
		return 0, fmt.Errorf("queue: record size %d out of range", len(payload))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.segs[len(q.segs)-1]
	if s.size >= q.opt.SegmentBytes {
		if err := q.f.Sync(); err != nil {
			return 0, err
		}
		_ = q.f.Close()
		if err := q.openActive(); err != nil {
			return 0, err
		}
		s = q.segs[len(q.segs)-1]
	}
	buf := make([]byte, hdrLen+len(payload))
	binary.LittleEndian.PutUint32(buf, uint32(len(payload)))
	binary.LittleEndian.PutUint32(buf[4:], crc32.Checksum(payload, castagnoli))
	copy(buf[hdrLen:], payload)
	if _, err := q.f.Write(buf); err != nil {
		return 0, err
	}
	s.size += int64(len(buf))
	s.count++
	off := q.next
	q.next++
	q.unsynced++
	if q.opt.SyncEvery > 0 && q.unsynced >= q.opt.SyncEvery {
		if err := q.f.Sync(); err != nil {
			return 0, err
		}
		q.unsynced = 0
	}
	q.enforceCap()
	return off, nil
}

// Sync makes all appended records durable.
func (q *Queue) Sync() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.unsynced == 0 {
		return nil
	}
	q.unsynced = 0
	return q.f.Sync()
}

func (q *Queue) enforceCap() {
	if q.opt.MaxBytes <= 0 {
		return
	}
	for len(q.segs) > 1 && q.totalBytes() > q.opt.MaxBytes {
		s := q.segs[0]
		end := s.base + s.count
		if end > q.ack {
			lost := end - q.ack
			if s.base > q.ack {
				lost = s.count
			}
			q.Dropped += lost
			q.ack = end
		}
		_ = os.Remove(s.path)
		q.segs = q.segs[1:]
	}
}

func (q *Queue) totalBytes() int64 {
	var t int64
	for _, s := range q.segs {
		t += s.size
	}
	return t
}

// Read returns up to max unacknowledged records starting at offset `from`
// (use AckedOffset() for the first call).
func (q *Queue) Read(from uint64, max int) ([]Record, error) {
	q.mu.Lock()
	if from < q.ack {
		from = q.ack
	}
	var segs []*segment
	for _, s := range q.segs {
		if s.base+s.count > from {
			segs = append(segs, &segment{base: s.base, path: s.path, size: s.size, count: s.count})
		}
	}
	q.mu.Unlock()
	var out []Record
	for _, s := range segs {
		f, err := os.Open(s.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted by the size cap meanwhile
			}
			return nil, err
		}
		r := io.LimitReader(f, s.size)
		off := s.base
		hdr := make([]byte, hdrLen)
		for off < s.base+s.count && len(out) < max {
			if _, err := io.ReadFull(r, hdr); err != nil {
				f.Close()
				return nil, err
			}
			ln := binary.LittleEndian.Uint32(hdr)
			p := make([]byte, ln)
			if _, err := io.ReadFull(r, p); err != nil {
				f.Close()
				return nil, err
			}
			if off >= from {
				out = append(out, Record{Offset: off, Payload: p})
			}
			off++
		}
		f.Close()
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// Ack marks every offset < upTo as delivered, persists it, and deletes
// fully acknowledged segments.
func (q *Queue) Ack(upTo uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if upTo <= q.ack {
		return nil
	}
	if upTo > q.next {
		return fmt.Errorf("queue: ack %d beyond end %d", upTo, q.next)
	}
	tmp := filepath.Join(q.dir, ackFile+".tmp")
	if err := writeSync(tmp, []byte(strconv.FormatUint(upTo, 10))); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(q.dir, ackFile)); err != nil {
		return err
	}
	if err := syncDir(q.dir); err != nil {
		return err
	}
	q.ack = upTo
	for len(q.segs) > 1 && q.segs[0].base+q.segs[0].count <= q.ack {
		_ = os.Remove(q.segs[0].path)
		q.segs = q.segs[1:]
	}
	return nil
}

func writeSync(p string, b []byte) error {
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// AckedOffset is the first unacknowledged offset.
func (q *Queue) AckedOffset() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ack
}

// NextOffset is the offset the next Append will get.
func (q *Queue) NextOffset() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.next
}

// Backlog is the number of unacknowledged records.
func (q *Queue) Backlog() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.next - q.ack
}

// DiskBytes is the total size of the segment files.
func (q *Queue) DiskBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.totalBytes()
}

// Close syncs and closes the active segment.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.f == nil {
		return nil
	}
	err := q.f.Sync()
	_ = q.f.Close()
	q.f = nil
	return err
}
