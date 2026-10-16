package claude

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/jhh3/pyre/internal/contracts"
)

// Transcript reads for sessions.read without parsing the whole file.
//
// sessions.read used to decode a run's whole transcript on every call: a
// 50 MB file cost ~245 ms of CPU per read, a 200 MB one about a second, and
// pyred executes hub commands one at a time, so every attach, delivery and
// read queued behind it (server, Oct 2026: sessions.read p50 595 ms, attach
// p50 393 ms, pyred at 25 % CPU). Entries are numbered from the start of
// the file (Seq = the record's position among the lines that decode,
// cursors and search hits depend on it), so a file is read once from the
// end and kept:
//
//   - the first read collects the newest tailWindow entries reading
//     backwards in tailChunk pieces, and only counts the records before
//     them (a decode of a few typed fields, in parallel for large files);
//   - later reads stat the file and decode only the appended bytes;
//   - a replaced (other inode), truncated or rewritten file (the bytes just
//     before the old end changed) is read again from the end;
//   - (offset, seq) marks every tailMarkEvery bytes let a forward page
//     behind the window start near its cursor instead of at the start.
//
// The cache is bounded by file count and by the approximate size of the
// kept entries; the least recently read file is dropped first.
const (
	tailWindow       = 500       // newest entries kept per file: the largest page sessions.read serves
	tailChunk        = 256 << 10 // backward read size
	tailMarkEvery    = 1 << 20   // bytes between (offset, seq) marks
	tailCacheFiles   = 64
	tailCacheBytes   = 24 << 20 // approximate memory of all kept entries
	tailCheckBytes   = 64       // bytes before the read end compared to detect a rewrite
	tailParallelFrom = 8 << 20  // count records with several workers from this prefix size
)

// countRecord has exactly the typed (non-RawMessage) fields of
// transcriptRecord: a line decodes into it iff it decodes into
// transcriptRecord (RawMessage fields accept any value), without copying
// the message bodies. TestCountRecordMatchesTranscriptRecord keeps the
// two in step.
type countRecord struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype,omitempty"`
	Operation        string `json:"operation,omitempty"`
	UUID             string `json:"uuid,omitempty"`
	Timestamp        string `json:"timestamp,omitempty"`
	IsMeta           bool   `json:"isMeta,omitempty"`
	IsSidechain      bool   `json:"isSidechain,omitempty"`
	IsAPIError       bool   `json:"isApiErrorMessage,omitempty"`
	DurationMs       int64  `json:"durationMs,omitempty"`
	SourceToolAssist string `json:"sourceToolAssistantUUID,omitempty"`
}

func countsAsRecord(line []byte) bool {
	var r countRecord
	return json.Unmarshal(line, &r) == nil
}

// seqMark: seq records lie in [0, off); off is a line start.
type seqMark struct{ off, seq int64 }

// tailFile is the cached state of one transcript file.
type tailFile struct {
	mu    sync.Mutex
	valid bool
	fi    os.FileInfo
	off   int64  // end of the last complete line read
	seq   int64  // records in [0, off)
	check []byte // the (up to tailCheckBytes) bytes before off
	marks []seqMark
	win   []contracts.TranscriptEntry // newest entries (RunID unset), ascending Seq
	floor int64                       // every entry with Seq > floor is in win
	size  int                         // approximate bytes held by win
}

type tailSlot struct {
	path  string
	f     *tailFile
	bytes int
}

// tailCache is usable as a zero value.
type tailCache struct {
	mu    sync.Mutex
	files map[string]*list.Element // of *tailSlot
	lru   list.List
	bytes int
	colds int // files read from the end (tests)
}

// read returns sessions.read's entries of the transcript at path: those
// with Seq > start, the newest limit of them when tail, else the oldest
// limit. A missing file has no entries.
func (c *tailCache) read(path, runID string, start int64, limit int, tail bool) ([]contracts.TranscriptEntry, error) {
	f := c.file(path)
	f.mu.Lock()
	out, err := f.read(c, path, start, limit, tail)
	size, valid := f.size, f.valid
	f.mu.Unlock()
	c.account(path, f, size, valid)
	for i := range out {
		out[i].RunID = runID
	}
	return out, err
}

func (c *tailCache) file(path string) *tailFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]*list.Element{}
	}
	if el, ok := c.files[path]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*tailSlot).f
	}
	s := &tailSlot{path: path, f: &tailFile{}}
	c.files[path] = c.lru.PushFront(s)
	c.evict()
	return s.f
}

// account records f's new size and evicts beyond the bounds; a file that
// could not be read is dropped.
func (c *tailCache) account(path string, f *tailFile, size int, valid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.files[path]
	if !ok || el.Value.(*tailSlot).f != f {
		return
	}
	if !valid {
		c.remove(el)
		return
	}
	s := el.Value.(*tailSlot)
	c.bytes += size - s.bytes
	s.bytes = size
	c.evict()
}

func (c *tailCache) evict() {
	for c.lru.Len() > tailCacheFiles || (c.bytes > tailCacheBytes && c.lru.Len() > 1) {
		c.remove(c.lru.Back())
	}
}

func (c *tailCache) remove(el *list.Element) {
	s := el.Value.(*tailSlot)
	c.bytes -= s.bytes
	delete(c.files, s.path)
	c.lru.Remove(el)
}

func (c *tailCache) stats() (files, bytes, colds int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes, c.colds
}

// reset forgets the file (f.mu stays held by the caller).
func (f *tailFile) reset() {
	f.valid, f.fi, f.off, f.seq = false, nil, 0, 0
	f.check, f.marks, f.win, f.floor, f.size = nil, nil, nil, 0, 0
}

func (f *tailFile) read(c *tailCache, path string, start int64, limit int, tail bool) ([]contracts.TranscriptEntry, error) {
	fh, err := os.Open(path)
	if err != nil {
		f.reset()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	if err := f.update(c, fh); err != nil {
		f.reset()
		return nil, err
	}
	if out, ok := f.fromWindow(start, limit, tail); ok {
		return out, nil
	}
	return f.forward(fh, start, limit, tail)
}

// update brings the cached state up to the file's current end.
func (f *tailFile) update(c *tailCache, fh *os.File) error {
	fi, err := fh.Stat()
	if err != nil {
		return err
	}
	if f.valid && os.SameFile(f.fi, fi) && fi.Size() >= f.off {
		if fi.Size() == f.off && fi.ModTime().Equal(f.fi.ModTime()) {
			return nil
		}
		ok, err := f.appendFrom(fh, fi)
		if err != nil || ok {
			return err
		}
	}
	c.mu.Lock()
	c.colds++
	c.mu.Unlock()
	return f.cold(fh, fi)
}

// appendFrom decodes the bytes appended since off; false when the bytes
// before off changed (the file was rewritten).
func (f *tailFile) appendFrom(fh *os.File, fi os.FileInfo) (bool, error) {
	k := int64(len(f.check))
	if _, err := fh.Seek(f.off-k, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReaderSize(fh, 64<<10)
	head := make([]byte, k)
	if _, err := io.ReadFull(r, head); err != nil || !bytes.Equal(head, f.check) {
		return false, nil
	}
	pos := f.off
	lastMark := f.marks[len(f.marks)-1].off
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			pos += int64(len(line))
			f.add(line[:len(line)-1])
			f.noteCheck(line)
			if pos-lastMark >= tailMarkEvery {
				f.marks = append(f.marks, seqMark{off: pos, seq: f.seq})
				lastMark = pos
			}
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
	}
	f.off, f.fi = pos, fi
	f.trim()
	return true, nil
}

// add numbers one complete line after the current end.
func (f *tailFile) add(line []byte) {
	var r transcriptRecord
	if json.Unmarshal(line, &r) != nil {
		return
	}
	f.seq++
	if e, ok := transcriptEntry("", r, f.seq); ok {
		f.win = append(f.win, e)
		f.size += entrySize(e)
	}
}

// noteCheck keeps the last tailCheckBytes bytes read (line ends with '\n').
func (f *tailFile) noteCheck(line []byte) {
	if len(line) >= tailCheckBytes {
		f.check = append(f.check[:0], line[len(line)-tailCheckBytes:]...)
		return
	}
	f.check = append(f.check, line...)
	if n := len(f.check); n > tailCheckBytes {
		f.check = append(f.check[:0], f.check[n-tailCheckBytes:]...)
	}
}

// trim drops the oldest entries beyond tailWindow (in batches, so appends
// do not copy the window every time).
func (f *tailFile) trim() {
	if len(f.win) <= tailWindow+tailWindow/4 {
		return
	}
	drop := len(f.win) - tailWindow
	for _, e := range f.win[:drop] {
		f.size -= entrySize(e)
	}
	f.floor = f.win[drop-1].Seq
	f.win = append([]contracts.TranscriptEntry(nil), f.win[drop:]...)
}

func entrySize(e contracts.TranscriptEntry) int {
	return len(e.Text) + len(e.Kind) + len(e.MessageID) + 96
}

// cold reads the file from its end: the newest tailWindow entries are
// decoded backwards, the records before them only counted.
func (f *tailFile) cold(fh *os.File, fi os.FileInfo) error {
	f.reset()
	size := fi.Size()
	var (
		buf      []byte // unsplit bytes [bufStart, bufStart+len(buf)), ending at a line end
		bufStart = size
		off      = int64(-1) // end of the last complete line
		records  int64       // decodable records after the unsplit bytes
		rev      []contracts.TranscriptEntry
	)
	for bufStart > 0 && len(rev) < tailWindow {
		n := min(int64(tailChunk), bufStart)
		chunk := make([]byte, n+int64(len(buf)))
		if _, err := fh.ReadAt(chunk[:n], bufStart-n); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		copy(chunk[n:], buf)
		buf, bufStart = chunk, bufStart-n
		if off < 0 {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				continue // no complete line yet
			}
			off = bufStart + int64(i) + 1
			buf = buf[:i+1]
		}
		for len(buf) > 0 && len(rev) < tailWindow {
			j := bytes.LastIndexByte(buf[:len(buf)-1], '\n')
			if j < 0 && bufStart > 0 {
				break // the line starts in an earlier chunk
			}
			var r transcriptRecord
			if json.Unmarshal(buf[j+1:len(buf)-1], &r) == nil {
				records++
				// Seq is fixed below, once the records before are counted.
				if e, ok := transcriptEntry("", r, -records); ok {
					rev = append(rev, e)
				}
			}
			buf = buf[:j+1]
		}
	}
	if off < 0 { // no complete line at all
		off, buf, bufStart = 0, nil, 0
	}
	split := bufStart + int64(len(buf))
	prefix, marks, err := countRecords(fh, split)
	if err != nil {
		return err
	}
	f.seq = prefix + records
	f.floor = prefix
	f.win = make([]contracts.TranscriptEntry, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		e := rev[i]
		e.Seq = f.seq + e.Seq + 1 // -k: the k-th record from the end
		f.win = append(f.win, e)
		f.size += entrySize(e)
	}
	f.marks = append(marks, seqMark{off: split, seq: prefix})
	if off > split {
		f.marks = append(f.marks, seqMark{off: off, seq: f.seq})
	}
	k := min(off, tailCheckBytes)
	f.check = make([]byte, k)
	if _, err := fh.ReadAt(f.check, off-k); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	f.off, f.fi, f.valid = off, fi, true
	f.trim()
	return nil
}

// countRecords counts the records in [0, end) (end is a line start) and
// returns (offset, seq) marks about every tailMarkEvery bytes, the first
// at 0. Large prefixes are split at line starts across a few workers.
func countRecords(fh *os.File, end int64) (int64, []seqMark, error) {
	workers := 1
	if end >= tailParallelFrom {
		workers = min(4, runtime.GOMAXPROCS(0))
	}
	bounds := []int64{0}
	for i := 1; i < workers; i++ {
		b, err := nextLineStart(fh, end*int64(i)/int64(workers), end)
		if err != nil {
			return 0, nil, err
		}
		if b > bounds[len(bounds)-1] && b < end {
			bounds = append(bounds, b)
		}
	}
	bounds = append(bounds, end)
	type part struct {
		n     int64
		marks []seqMark // seq relative to the part's start
		err   error
	}
	parts := make([]part, len(bounds)-1)
	var wg sync.WaitGroup
	for i := range parts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &parts[i]
			from, to := bounds[i], bounds[i+1]
			r := bufio.NewReaderSize(io.NewSectionReader(fh, from, to-from), 256<<10)
			pos, last := from, from
			for {
				line, err := r.ReadSlice('\n')
				if errors.Is(err, bufio.ErrBufferFull) {
					// A line longer than the buffer: gather it whole.
					long := append([]byte(nil), line...)
					for errors.Is(err, bufio.ErrBufferFull) {
						line, err = r.ReadSlice('\n')
						long = append(long, line...)
					}
					line = long
				}
				if len(line) > 0 && line[len(line)-1] == '\n' {
					pos += int64(len(line))
					if countsAsRecord(line[:len(line)-1]) {
						p.n++
					}
					if pos-last >= tailMarkEvery {
						p.marks = append(p.marks, seqMark{off: pos, seq: p.n})
						last = pos
					}
					continue
				}
				if err != nil && !errors.Is(err, io.EOF) {
					p.err = err
				}
				return
			}
		}(i)
	}
	wg.Wait()
	marks := []seqMark{{0, 0}}
	var total int64
	for _, p := range parts {
		if p.err != nil {
			return 0, nil, p.err
		}
		for _, m := range p.marks {
			marks = append(marks, seqMark{off: m.off, seq: total + m.seq})
		}
		total += p.n
	}
	return total, marks, nil
}

// nextLineStart returns the start of the first line beginning at or after
// at (end when none does before end).
func nextLineStart(fh *os.File, at, end int64) (int64, error) {
	if at <= 0 {
		return 0, nil
	}
	buf := make([]byte, 64<<10)
	for pos := at - 1; pos < end; {
		n, err := fh.ReadAt(buf[:min(int64(len(buf)), end-pos)], pos)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, nil
		}
		pos += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
	}
	return end, nil
}

// fromWindow answers from the kept entries when they hold the answer.
func (f *tailFile) fromWindow(start int64, limit int, tail bool) ([]contracts.TranscriptEntry, bool) {
	i := sort.Search(len(f.win), func(i int) bool { return f.win[i].Seq > start })
	sel := f.win[i:]
	switch {
	case tail && (start >= f.floor || len(sel) >= limit):
		if len(sel) > limit {
			sel = sel[len(sel)-limit:]
		}
	case !tail && start >= f.floor:
		if len(sel) > limit {
			sel = sel[:limit]
		}
	default:
		return nil, false
	}
	return append([]contracts.TranscriptEntry(nil), sel...), true
}

// forward reads entries with Seq > start from the nearest mark at or
// before start up to the cached end: the oldest limit of them, or the
// newest limit when tail.
func (f *tailFile) forward(fh *os.File, start int64, limit int, tail bool) ([]contracts.TranscriptEntry, error) {
	i := sort.Search(len(f.marks), func(i int) bool { return f.marks[i].seq > start }) - 1
	m := f.marks[max(i, 0)]
	if _, err := fh.Seek(m.off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(io.LimitReader(fh, f.off-m.off), 64<<10)
	seq := m.seq
	var out []contracts.TranscriptEntry
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var rec transcriptRecord
			if json.Unmarshal(line[:len(line)-1], &rec) != nil {
				continue
			}
			seq++
			if seq <= start {
				continue
			}
			if e, ok := transcriptEntry("", rec, seq); ok {
				out = append(out, e)
				if tail && len(out) > limit {
					out = out[1:]
				}
				if !tail && len(out) == limit {
					return out, nil
				}
			}
			continue
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}
