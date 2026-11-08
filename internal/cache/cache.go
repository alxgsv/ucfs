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

// Fetcher reads object bytes; cdn.Client implements it.
type Fetcher interface {
	ReadRange(ctx context.Context, uuid, ops string, p []byte, off int64) (int, error)
}

type Options struct {
	Dir      string
	MaxBytes int64
	// MaxWindow caps the prefetch window of one sequential stream.
	MaxWindow int64
	// Prefetchers bounds concurrent background fetches across all objects.
	Prefetchers  int
	FetchTimeout time.Duration
}

type Cache struct {
	o     Options
	fetch Fetcher

	mu      sync.Mutex
	lru     *list.List // *blockEntry, most recently used at the front
	blocks  map[blockID]*list.Element
	used    int64
	streams map[Key][]*stream

	sf      singleflight.Group
	sem     chan struct{}
	pending sync.WaitGroup

	served, hit, fetchedDemand, fetchedPrefetch, evicted atomic.Int64
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
		o.MaxWindow = 16 * BlockSize
	}
	if o.Prefetchers == 0 {
		o.Prefetchers = 16
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
		sem: make(chan struct{}, o.Prefetchers),
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
	if pf := c.observe(k, off, end, size); pf.to > pf.from {
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
			data, err := c.fetchBlock(ctx, k, id, size, &c.fetchedDemand)
			if err != nil {
				return n, err
			}
			copy(dst, data[inBlock:])
		}
		n += int(want)
		pos += want
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
// fetch runs on its own context so a reader that gives up does not fail the
// others waiting on the same block. The first caller's counter gets the bytes.
func (c *Cache) fetchBlock(ctx context.Context, k Key, id blockID, size int64, counter *atomic.Int64) ([]byte, error) {
	v, err, _ := c.sf.Do(c.path(id), func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.o.FetchTimeout)
		defer cancel()
		start := id.n * BlockSize
		buf := make([]byte, min(BlockSize, size-start))
		if _, err := c.fetch.ReadRange(fctx, k.UUID, k.Ops, buf, start); err != nil {
			return nil, err
		}
		counter.Add(int64(len(buf)))
		if err := c.store(id, buf); err != nil {
			return nil, err
		}
		return buf, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (c *Cache) store(id blockID, data []byte) error {
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
	if el, ok := c.blocks[id]; ok {
		c.lru.MoveToFront(el)
		return nil
	}
	c.blocks[id] = c.lru.PushFront(&blockEntry{id, int64(len(data))})
	c.used += int64(len(data))
	c.evictLocked()
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
// nothing, so a small read such as head(1) costs one block.
func (c *Cache) observe(k Key, off, end, size int64) blockRange {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	ss := c.streams[k]
	var s *stream
	for _, cand := range ss {
		if off >= cand.next-streamSlack && off <= cand.next+max(cand.window, streamSlack) {
			s = cand
			break
		}
	}
	if s == nil {
		if len(c.streams) >= maxTrackedObjects {
			c.pruneStreamsLocked(now)
		}
		s = &stream{next: end, issued: (end + BlockSize - 1) / BlockSize, lastUsed: now}
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
		c.streams[k] = ss
		return blockRange{}
	}
	s.lastUsed = now
	// The window grows per block a reader moves into, not per read call: the
	// Linux kernel splits reads into 128 KiB requests, and growing on each
	// one would prefetch 16 MiB for a reader that only wanted 1 MiB.
	prevBlock := (s.next - 1) / BlockSize
	s.next = max(s.next, end)
	if (s.next-1)/BlockSize <= prevBlock {
		return blockRange{}
	}
	s.window = min(max(2*s.window, BlockSize), c.o.MaxWindow)
	lastBlock := (size + BlockSize - 1) / BlockSize
	r := blockRange{
		from: max(s.issued, (s.next+BlockSize-1)/BlockSize),
		to:   min(lastBlock, (s.next+s.window+BlockSize-1)/BlockSize),
	}
	if r.to > r.from {
		s.issued = r.to
	}
	return r
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

func (c *Cache) prefetch(k Key, size int64, r blockRange) {
	obj := objName(k)
	for b := r.from; b < r.to; b++ {
		id := blockID{obj, b}
		if c.cached(id) {
			continue
		}
		c.pending.Go(func() {
			c.sem <- struct{}{}
			defer func() { <-c.sem }()
			if !c.cached(id) {
				_, _ = c.fetchBlock(context.Background(), k, id, size, &c.fetchedPrefetch)
			}
		})
	}
}

// Wait blocks until scheduled prefetches have finished.
func (c *Cache) Wait() { c.pending.Wait() }

// Forget drops the read streams of an object, for example after it was removed.
func (c *Cache) Forget(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.streams, k)
}

// Drop removes an object's blocks and streams, for a variant whose content
// changed.
func (c *Cache) Drop(k Key) {
	obj := objName(k)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.streams, k)
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
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	used := c.used
	c.mu.Unlock()
	return Stats{
		Served: c.served.Load(), Hit: c.hit.Load(),
		FetchedDemand: c.fetchedDemand.Load(), FetchedPrefetch: c.fetchedPrefetch.Load(),
		Used: used, Max: c.o.MaxBytes, Evicted: c.evicted.Load(),
	}
}
