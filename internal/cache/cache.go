// Package cache is the on-disk block cache between the filesystem and the CDN.
//
// Content is cached in fixed blocks keyed by (uuid, operations, block number).
// Identical reads coalesce through singleflight, a per-object sequential
// detector grows a prefetch window, and every byte is accounted for.
package cache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/alxgsv/ucfs/internal/cdn"
)

const BlockSize = 1 << 20

// Key names one object: an original (Ops empty) or a CDN variant. Version
// tells apart contents a variant had over time, such as its ETag; it only
// changes where blocks are stored, never what is fetched.
type Key struct {
	UUID    string
	Ops     string
	Version string
}

// Fetcher reads object bytes and the ETag they came with; cdn.Client
// implements it.
type Fetcher interface {
	ReadRange(ctx context.Context, uuid, ops string, p []byte, off int64) (int, string, error)
}

// ChangedError is a range whose ETag is not the one recorded for its
// object, a variant the CDN rendered anew or an original served with other
// bytes, or a read that continues a stream begun before such a change. The
// object's blocks are dropped when the change is seen, so the cache never
// holds blocks of both contents; and the read fails, because a reader that
// already received blocks of the old content would otherwise get a file
// made of both. A stream that failed so is forgotten, so the reader's next
// read starts over on the new content.
type ChangedError struct {
	Key       Key
	Want, Got string
}

func (e *ChangedError) Error() string {
	if e.Got == "" {
		return fmt.Sprintf("%s/%s changed since this reader started", e.Key.UUID, e.Key.Ops)
	}
	return fmt.Sprintf("%s/%s changed while read: ETag %s, was %s", e.Key.UUID, e.Key.Ops, e.Got, e.Want)
}

const maxPrefetchWindow = 16 * BlockSize

type Options struct {
	Dir      string
	MaxBytes int64
	// MaxWindow caps the prefetch window of one sequential stream.
	MaxWindow int64
	// Limiter bounds the CDN requests in flight and orders them; nil means
	// a limiter of its own with cdn.DefaultMaxInflight slots.
	Limiter *cdn.Limiter
	// Budget caps the bytes fetched per day; nil means no cap.
	Budget *Budget
	// FetchTimeout bounds one block fetch from when it gets its slot.
	FetchTimeout time.Duration
}

type Cache struct {
	o     Options
	fetch Fetcher

	mu     sync.Mutex
	lru    *list.List // *blockEntry, most recently used at the front
	blocks map[blockID]*list.Element
	used   int64
	// streams are the sequential readers of each object, by uuid and
	// operations whatever the version, so a reader that started on one
	// content is still recognised after it changed.
	streams map[Key][]*stream
	// epochs counts the content changes seen per object, by uuid and
	// operations; a stream started in an earlier epoch is stale.
	epochs map[Key]uint64

	// etags holds the ETag of originals by object name, as the first range
	// read of each in this process returned it. Variants carry theirs in
	// Key.Version.
	etags map[string]string

	sf      singleflight.Group
	pending sync.WaitGroup

	served, hit, fetchedDemand, fetchedPrefetch, evicted, changed atomic.Int64
}

type blockID struct {
	obj string
	n   int64
}

type blockEntry struct {
	id   blockID
	size int64
}

// Open scans dir for blocks left by earlier mounts and adopts them.
func Open(fetch Fetcher, o Options) (*Cache, error) {
	if o.MaxWindow == 0 {
		o.MaxWindow = maxPrefetchWindow
	}
	if o.Limiter == nil {
		o.Limiter = cdn.NewLimiter(cdn.DefaultMaxInflight)
	}
	if o.FetchTimeout == 0 {
		o.FetchTimeout = 2 * time.Minute
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	c := &Cache{
		o: o, fetch: fetch,
		lru: list.New(), blocks: map[blockID]*list.Element{}, streams: map[Key][]*stream{},
		epochs: map[Key]uint64{}, etags: map[string]string{},
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()
	return c, nil
}

func (c *Cache) scan() error {
	type found struct {
		e     blockEntry
		mtime time.Time
	}
	var all []found
	err := filepath.WalkDir(c.o.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n, perr := strconv.ParseInt(d.Name(), 10, 64)
		if perr != nil {
			// Leftover temp file from an interrupted write.
			return os.Remove(path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		obj := filepath.Base(filepath.Dir(path))
		all = append(all, found{blockEntry{blockID{obj, n}, info.Size()}, info.ModTime()})
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].mtime.Before(all[j].mtime) })
	for _, f := range all {
		c.blocks[f.e.id] = c.lru.PushFront(&blockEntry{f.e.id, f.e.size})
		c.used += f.e.size
	}
	return err
}

func objName(k Key) string {
	if k.Ops == "" && k.Version == "" {
		return k.UUID
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(k.Ops + "\x00" + k.Version))
	return fmt.Sprintf("%s~%016x", k.UUID, h.Sum64())
}

func (c *Cache) path(id blockID) string {
	return filepath.Join(c.o.Dir, id.obj, strconv.FormatInt(id.n, 10))
}

// ReadAt reads from an object of the given size.
func (c *Cache) ReadAt(ctx context.Context, k Key, size int64, p []byte, off int64) (int, error) {
	if off >= size {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), size)
	pf, epoch, stale := c.observe(k, off, end, size)
	if stale {
		return 0, &ChangedError{Key: k}
	}
	if pf.to > pf.from {
		c.prefetch(k, size, pf)
	}
	obj := objName(k)
	n := 0
	for pos := off; pos < end; {
		b := pos / BlockSize
		inBlock := pos - b*BlockSize
		want := min(end, (b+1)*BlockSize) - pos
		dst := p[n : n+int(want)]
		id := blockID{obj, b}
		if c.readCached(id, dst, inBlock) {
			c.hit.Add(want)
		} else {
			data, err := c.fetchBlock(ctx, k, id, size)
			if err != nil {
				return n, err
			}
			copy(dst, data[inBlock:])
		}
		n += int(want)
		pos += want
	}
	// A change seen while this read ran, by any fetch, may have given it
	// blocks of both contents.
	if c.epoch(k) != epoch {
		return 0, &ChangedError{Key: k}
	}
	c.served.Add(int64(n))
	if end == size && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (c *Cache) readCached(id blockID, dst []byte, off int64) bool {
	c.mu.Lock()
	el, ok := c.blocks[id]
	if ok {
		c.lru.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	f, err := os.Open(c.path(id))
	if err != nil {
		// Evicted between the map check and the open.
		return false
	}
	defer func() { _ = f.Close() }()
	_, err = f.ReadAt(dst, off)
	return err == nil
}

func (c *Cache) cached(id blockID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.blocks[id]
	return ok
}

// fetchBlock downloads one block once, however many readers want it. The
// budget and a slot are taken inside the flight, so readers joining it are
// neither counted nor queued again. The fetch runs on its own context so a
// reader that gives up does not fail the others waiting on the same block.
func (c *Cache) fetchBlock(ctx context.Context, k Key, id blockID, size int64) ([]byte, error) {
	v, err, _ := c.sf.Do(c.path(id), func() (any, error) {
		n := blockLen(id, size)
		if !c.o.Budget.take(n, false) {
			return nil, ErrBudget
		}
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.o.FetchTimeout)
		release, err := c.o.Limiter.Acquire(qctx, id.obj)
		cancel()
		if err != nil {
			c.o.Budget.give(n)
			return nil, err
		}
		defer release()
		buf, err := c.download(ctx, k, id, size, &c.fetchedDemand)
		if err != nil {
			c.o.Budget.give(n)
			return nil, err
		}
		return buf, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func blockLen(id blockID, size int64) int64 { return min(BlockSize, size-id.n*BlockSize) }

// download fetches a block, checks its ETag and stores it; the first
// caller's counter gets the bytes.
func (c *Cache) download(ctx context.Context, k Key, id blockID, size int64, counter *atomic.Int64) ([]byte, error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.o.FetchTimeout)
	defer cancel()
	buf := make([]byte, blockLen(id, size))
	_, etag, err := c.fetch.ReadRange(fctx, k.UUID, k.Ops, buf, id.n*BlockSize)
	if err != nil {
		return nil, err
	}
	if err := c.checkETag(k, id.obj, etag); err != nil {
		return nil, err
	}
	counter.Add(int64(len(buf)))
	if k.Ops != "" {
		etag = ""
	}
	if err := c.store(id, buf, etag); err != nil {
		return nil, err
	}
	return buf, nil
}

// maxETags bounds the ETags of originals kept; past it they are
// forgotten and recorded anew by the next read of each.
const maxETags = 1 << 16

// checkETag compares a range's ETag with the object's. On a mismatch the
// object's blocks go, and an original takes the new ETag, so the next read
// starts over on the new content.
func (c *Cache) checkETag(k Key, obj, got string) error {
	if got == "" {
		return nil
	}
	want := k.Version
	if k.Ops == "" {
		c.mu.Lock()
		want = c.etags[obj]
		if want == "" {
			if len(c.etags) >= maxETags {
				clear(c.etags)
			}
			c.etags[obj] = got
		}
		c.mu.Unlock()
	}
	if want == "" || cdn.SameETag(want, got) {
		return nil
	}
	c.changed.Add(1)
	// The new ETag goes in with the drop, under one lock: a block of the old
	// content stored before is dropped, and one stored after is refused.
	c.mu.Lock()
	if k.Ops == "" {
		c.etags[obj] = got
	}
	c.dropLocked(k)
	c.mu.Unlock()
	return &ChangedError{Key: k, Want: want, Got: got}
}

// store writes a block. etag is the ETag an original's block came with: the
// block is kept only if that is still its object's, since another range may
// have found the content changed and dropped the object meanwhile. A
// variant's blocks need no such check, as their object name includes the
// ETag they were fetched for.
func (c *Cache) store(id blockID, data []byte, etag string) error {
	dir := filepath.Dir(c.path(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), c.path(id)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if want := c.etags[id.obj]; etag != "" && want != "" && !cdn.SameETag(want, etag) {
		_ = os.Remove(c.path(id))
		return &ChangedError{Key: Key{UUID: id.obj}, Want: want, Got: etag}
	}
	if el, ok := c.blocks[id]; ok {
		c.lru.MoveToFront(el)
		return nil
	}
	c.blocks[id] = c.lru.PushFront(&blockEntry{id, int64(len(data))})
	c.used += int64(len(data))
	c.evictLocked()
	return nil
}

// Put stores an object's bytes as blocks, as a written file has them before
// the CDN serves them: reading it back then costs no traffic.
func (c *Cache) Put(k Key, r io.ReaderAt, size int64) error {
	obj := objName(k)
	buf := make([]byte, BlockSize)
	for n := int64(0); n*BlockSize < size; n++ {
		got, err := r.ReadAt(buf[:min(BlockSize, size-n*BlockSize)], n*BlockSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := c.store(blockID{obj, n}, buf[:got], ""); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) evictLocked() {
	for c.used > c.o.MaxBytes && c.lru.Len() > 0 {
		el := c.lru.Back()
		e := el.Value.(*blockEntry)
		c.lru.Remove(el)
		delete(c.blocks, e.id)
		c.used -= e.size
		c.evicted.Add(e.size)
		// An open reader keeps its descriptor; a failed remove only leaks space.
		_ = os.Remove(c.path(e.id))
	}
}

// stream is one sequential reader of an object. NFS has no open handle, so
// streams are tracked per object; several streams let concurrent readers of
// one object keep their own windows.
type stream struct {
	next     int64 // end of the furthest read so far
	window   int64 // prefetch distance ahead of next; 0 until the stream turns sequential
	issued   int64 // first block not yet scheduled for prefetch
	lastUsed time.Time
	epoch    uint64
}

const (
	maxStreams        = 8
	maxTrackedObjects = 1024
	streamIdle        = 30 * time.Second
	// Reads may arrive out of order when the client keeps several in flight,
	// so a read up to this far behind or ahead still continues a stream.
	streamSlack = 4 * BlockSize
)

type blockRange struct{ from, to int64 }

// observe updates the sequential detector and returns the blocks to
// prefetch. Reads that stay within the first block of a stream prefetch
// nothing, so a small read such as head(1) costs one block. A read that
// continues a stream begun before the object's content changed is stale:
// the stream is forgotten and the read must fail. A read from offset 0
// starts over and cannot mix contents, so it starts a new stream instead.
func (c *Cache) observe(k Key, off, end, size int64) (r blockRange, epoch uint64, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	base := Key{UUID: k.UUID, Ops: k.Ops}
	epoch = c.epochs[base]
	ss := c.streams[base]
	var s *stream
	old := -1
	for i, cand := range ss {
		if off < cand.next-streamSlack || off > cand.next+max(cand.window, streamSlack) {
			continue
		}
		if cand.epoch == epoch {
			s = cand
			break
		}
		if old < 0 {
			old = i
		}
	}
	// A reader that started anew since the change has a current stream,
	// matched first; only a read no current stream explains is stale.
	if s == nil && old >= 0 {
		ss = append(ss[:old:old], ss[old+1:]...)
		c.streams[base] = ss
		if off > 0 {
			return blockRange{}, epoch, true
		}
	}
	if s == nil {
		if len(c.streams) >= maxTrackedObjects {
			c.pruneStreamsLocked(now)
		}
		s = &stream{next: end, issued: (end + BlockSize - 1) / BlockSize, lastUsed: now, epoch: epoch}
		if len(ss) >= maxStreams {
			oldest := 0
			for i, cand := range ss {
				if cand.lastUsed.Before(ss[oldest].lastUsed) {
					oldest = i
				}
			}
			ss[oldest] = s
		} else {
			ss = append(ss, s)
		}
		c.streams[base] = ss
		return blockRange{}, epoch, false
	}
	s.lastUsed = now
	// The window grows per block a reader moves into, not per read call: the
	// Linux kernel splits reads into 128 KiB requests, and growing on each
	// one would prefetch 16 MiB for a reader that only wanted 1 MiB.
	prevBlock := (s.next - 1) / BlockSize
	s.next = max(s.next, end)
	if (s.next-1)/BlockSize <= prevBlock {
		return blockRange{}, epoch, false
	}
	s.window = min(max(2*s.window, BlockSize), c.o.MaxWindow)
	lastBlock := (size + BlockSize - 1) / BlockSize
	r = blockRange{
		from: max(s.issued, (s.next+BlockSize-1)/BlockSize),
		to:   min(lastBlock, (s.next+s.window+BlockSize-1)/BlockSize),
	}
	if r.to > r.from {
		s.issued = r.to
	}
	return r, epoch, false
}

func (c *Cache) epoch(k Key) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epochs[Key{UUID: k.UUID, Ops: k.Ops}]
}

func (c *Cache) pruneStreamsLocked(now time.Time) {
	for k, ss := range c.streams {
		idle := true
		for _, s := range ss {
			if now.Sub(s.lastUsed) < streamIdle {
				idle = false
				break
			}
		}
		if idle {
			delete(c.streams, k)
		}
	}
}

// prefetch queues the blocks of r that are not cached. Each takes its
// budget and joins or starts a flight only once its slot is free, so a
// reader that catches up with a queued block fetches it at its own
// priority instead of waiting behind other prefetches.
func (c *Cache) prefetch(k Key, size int64, r blockRange) {
	obj := objName(k)
	for b := r.from; b < r.to; b++ {
		id := blockID{obj, b}
		if c.cached(id) {
			continue
		}
		if !c.o.Budget.allowPrefetch(blockLen(id, size)) {
			return
		}
		c.pending.Add(1)
		if !c.o.Limiter.Go(obj, func() {
			defer c.pending.Done()
			c.prefetchBlock(k, id, size)
		}) {
			c.pending.Done()
		}
	}
}

func (c *Cache) prefetchBlock(k Key, id blockID, size int64) {
	if c.cached(id) {
		return
	}
	n := blockLen(id, size)
	if !c.o.Budget.take(n, true) {
		return
	}
	ran := false
	_, err, _ := c.sf.Do(c.path(id), func() (any, error) {
		ran = true
		return c.download(context.Background(), k, id, size, &c.fetchedPrefetch)
	})
	if !ran || err != nil {
		c.o.Budget.give(n)
	}
}

// Wait blocks until scheduled prefetches have finished.
func (c *Cache) Wait() { c.pending.Wait() }

// Forget drops the read streams of an object, for example after it was removed.
func (c *Cache) Forget(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.streams, Key{UUID: k.UUID, Ops: k.Ops})
}

// Drop removes an object's blocks and makes its streams stale, for a
// variant whose content changed.
func (c *Cache) Drop(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(k)
}

func (c *Cache) dropLocked(k Key) {
	obj := objName(k)
	base := Key{UUID: k.UUID, Ops: k.Ops}
	if len(c.epochs) >= maxETags {
		clear(c.epochs)
	}
	c.epochs[base]++
	for id, el := range c.blocks {
		if id.obj != obj {
			continue
		}
		c.lru.Remove(el)
		delete(c.blocks, id)
		c.used -= el.Value.(*blockEntry).size
		_ = os.Remove(c.path(id))
	}
}

type Stats struct {
	// Served is what readers received; Hit is the part served from disk.
	Served, Hit int64
	// FetchedDemand and FetchedPrefetch are bytes downloaded for readers
	// waiting on them and for prefetch ahead of them.
	FetchedDemand, FetchedPrefetch int64
	Used, Max, Evicted             int64
	// Changed counts ranges that came with another ETag than their object's.
	Changed int64
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	used := c.used
	c.mu.Unlock()
	return Stats{
		Served: c.served.Load(), Hit: c.hit.Load(),
		FetchedDemand: c.fetchedDemand.Load(), FetchedPrefetch: c.fetchedPrefetch.Load(),
		Used: used, Max: c.o.MaxBytes, Evicted: c.evicted.Load(), Changed: c.changed.Load(),
	}
}
