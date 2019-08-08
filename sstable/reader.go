// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"unsafe"

	"github.com/golang/snappy"
	"github.com/petermattis/pebble/cache"
	"github.com/petermattis/pebble/internal/base"
	"github.com/petermattis/pebble/internal/crc"
	"github.com/petermattis/pebble/internal/rangedel"
	"github.com/petermattis/pebble/vfs"
)

// BlockHandle is the file offset and length of a block.
type BlockHandle struct {
	Offset, Length uint64
}

// decodeBlockHandle returns the block handle encoded at the start of src, as
// well as the number of bytes it occupies. It returns zero if given invalid
// input.
func decodeBlockHandle(src []byte) (BlockHandle, int) {
	offset, n := binary.Uvarint(src)
	length, m := binary.Uvarint(src[n:])
	if n == 0 || m == 0 {
		return BlockHandle{}, 0
	}
	return BlockHandle{offset, length}, n + m
}

func encodeBlockHandle(dst []byte, b BlockHandle) int {
	n := binary.PutUvarint(dst, b.Offset)
	m := binary.PutUvarint(dst[n:], b.Length)
	return n + m
}

// block is a []byte that holds a sequence of key/value pairs plus an index
// over those pairs.
type block []byte

// Iterator iterates over an entire table of data. It is a two-level iterator:
// to seek for a given key, it first looks in the index for the block that
// contains that key, and then looks inside that block.
type Iterator struct {
	cmp Compare
	// Global lower/upper bound for the iterator.
	lower []byte
	upper []byte
	// Per-block lower/upper bound. Nil if the bound does not apply to the block
	// because we determined the block lies completely within the bound.
	blockLower []byte
	blockUpper []byte
	reader     *Reader
	index      blockIter
	data       blockIter
	dataBH     BlockHandle
	err        error
	closeHook  func(i *Iterator) error
}

var iterPool = sync.Pool{
	New: func() interface{} {
		return &Iterator{}
	},
}

// Init initializes an iterator for reading from the table. It is synonmous
// with Reader.NewIter, but allows for reusing of the Iterator between
// different Readers.
func (i *Iterator) Init(r *Reader, lower, upper []byte) error {
	*i = Iterator{
		lower:  lower,
		upper:  upper,
		reader: r,
		err:    r.err,
	}
	if i.err == nil {
		var index block
		index, i.err = r.readIndex()
		if i.err != nil {
			return i.err
		}
		i.cmp = r.compare
		i.err = i.index.init(i.cmp, index, r.Properties.GlobalSeqNum)
	}
	return i.err
}

func (i *Iterator) initBounds() {
	if i.lower == nil && i.upper == nil {
		return
	}

	// Trim the iteration bounds for the current block. We don't have to check
	// the bounds on each iteration if the block is entirely contained within the
	// iteration bounds.
	i.blockLower = i.lower
	if i.blockLower != nil {
		key, _ := i.data.First()
		if key != nil && i.cmp(i.blockLower, key.UserKey) < 0 {
			// The lower-bound is less than the first key in the block. No need
			// to check the lower-bound again for this block.
			i.blockLower = nil
		}
	}
	i.blockUpper = i.upper
	if i.blockUpper != nil && i.cmp(i.blockUpper, i.index.Key().UserKey) > 0 {
		// The upper-bound is greater than the index key which itself is greater
		// than or equal to every key in the block. No need to check the
		// upper-bound again for this block.
		i.blockUpper = nil
	}
}

// loadBlock loads the block at the current index position and leaves i.data
// unpositioned. If unsuccessful, it sets i.err to any error encountered, which
// may be nil if we have simply exhausted the entire table.
func (i *Iterator) loadBlock() bool {
	if !i.index.Valid() {
		i.err = i.index.err
		// TODO(peter): Need to test that seeking to a key outside of the sstable
		// invalidates the iterator.
		i.data.offset = 0
		i.data.restarts = 0
		return false
	}
	// Load the next block.
	v := i.index.Value()
	var n int
	i.dataBH, n = decodeBlockHandle(v)
	if n == 0 || n != len(v) {
		i.err = errors.New("pebble/table: corrupt index entry")
		return false
	}
	block, err := i.reader.readBlock(i.dataBH, nil /* transform */)
	if err != nil {
		i.err = err
		return false
	}
	i.data.setCacheHandle(block)
	i.err = i.data.init(i.cmp, block.Get(), i.reader.Properties.GlobalSeqNum)
	if i.err != nil {
		return false
	}
	i.initBounds()
	return true
}

// seekBlock loads the block at the current index position and positions i.data
// at the first key in that block which is >= the given key. If unsuccessful,
// it sets i.err to any error encountered, which may be nil if we have simply
// exhausted the entire table.
func (i *Iterator) seekBlock(key []byte) bool {
	if !i.index.Valid() {
		i.err = i.index.err
		return false
	}
	// Load the next block.
	v := i.index.Value()
	h, n := decodeBlockHandle(v)
	if n == 0 || n != len(v) {
		i.err = errors.New("pebble/table: corrupt index entry")
		return false
	}
	block, err := i.reader.readBlock(h, nil /* transform */)
	if err != nil {
		i.err = err
		return false
	}
	i.data.setCacheHandle(block)
	i.err = i.data.init(i.cmp, block.Get(), i.reader.Properties.GlobalSeqNum)
	if i.err != nil {
		return false
	}
	// Look for the key inside that block.
	i.initBounds()
	i.data.SeekGE(key)
	return true
}

// SeekGE implements internalIterator.SeekGE, as documented in the pebble
// package. Note that SeekGE only checks the upper bound. It is up to the
// caller to ensure that key is greater than or equal to the lower bound.
func (i *Iterator) SeekGE(key []byte) (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}

	if ikey, _ := i.index.SeekGE(key); ikey == nil {
		return nil, nil
	}
	if !i.loadBlock() {
		return nil, nil
	}
	ikey, val := i.data.SeekGE(key)
	if ikey == nil {
		return nil, nil
	}
	if i.blockUpper != nil && i.cmp(ikey.UserKey, i.blockUpper) >= 0 {
		i.data.invalidateUpper() // force i.data.Valid() to return false
		return nil, nil
	}
	return ikey, val
}

// SeekPrefixGE implements internalIterator.SeekPrefixGE, as documented in the
// pebble package. Note that SeekPrefixGE only checks the upper bound. It is up
// to the caller to ensure that key is greater than or equal to the lower bound.
func (i *Iterator) SeekPrefixGE(prefix, key []byte) (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}

	// Check prefix bloom filter.
	if i.reader.tableFilter != nil {
		data, err := i.reader.readFilter()
		if err != nil {
			return nil, nil
		}
		if !i.reader.tableFilter.mayContain(data, prefix) {
			i.data.invalidateUpper() // force i.data.Valid() to return false
			return nil, nil
		}
	}

	if ikey, _ := i.index.SeekGE(key); ikey == nil {
		return nil, nil
	}
	if !i.loadBlock() {
		return nil, nil
	}
	ikey, val := i.data.SeekGE(key)
	if ikey == nil {
		return nil, nil
	}
	if i.blockUpper != nil && i.cmp(ikey.UserKey, i.blockUpper) >= 0 {
		i.data.invalidateUpper() // force i.data.Valid() to return false
		return nil, nil
	}
	return ikey, val
}

// SeekLT implements internalIterator.SeekLT, as documented in the pebble
// package. Note that SeekLT only checks the lower bound. It is up to the
// caller to ensure that key is less than the upper bound.
func (i *Iterator) SeekLT(key []byte) (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}

	if ikey, _ := i.index.SeekGE(key); ikey == nil {
		i.index.Last()
	}
	if !i.loadBlock() {
		return nil, nil
	}
	ikey, val := i.data.SeekLT(key)
	if ikey == nil {
		// The index contains separator keys which may lie between
		// user-keys. Consider the user-keys:
		//
		//   complete
		// ---- new block ---
		//   complexion
		//
		// If these two keys end one block and start the next, the index key may
		// be chosen as "compleu". The SeekGE in the index block will then point
		// us to the block containing "complexion". If this happens, we want the
		// last key from the previous data block.
		if ikey, _ = i.index.Prev(); ikey == nil {
			return nil, nil
		}
		if !i.loadBlock() {
			return nil, nil
		}
		if ikey, val = i.data.Last(); ikey == nil {
			return nil, nil
		}
	}
	if i.blockLower != nil && i.cmp(ikey.UserKey, i.blockLower) < 0 {
		i.data.invalidateLower() // force i.data.Valid() to return false
		return nil, nil
	}
	return ikey, val
}

// First implements internalIterator.First, as documented in the pebble
// package. Note that First only checks the upper bound. It is up to the caller
// to ensure that key is greater than or equal to the lower bound (e.g. via a
// call to SeekGE(lower)).
func (i *Iterator) First() (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}

	if ikey, _ := i.index.First(); ikey == nil {
		return nil, nil
	}
	if !i.loadBlock() {
		return nil, nil
	}
	ikey, val := i.data.First()
	if ikey == nil {
		return nil, nil
	}
	if i.blockUpper != nil && i.cmp(ikey.UserKey, i.blockUpper) >= 0 {
		i.data.invalidateUpper() // force i.data.Valid() to return false
		return nil, nil
	}
	return ikey, val
}

// Last implements internalIterator.Last, as documented in the pebble
// package. Note that Last only checks the lower bound. It is up to the caller
// to ensure that key is less than the upper bound (e.g. via a call to
// SeekLT(upper))
func (i *Iterator) Last() (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}

	if ikey, _ := i.index.Last(); ikey == nil {
		return nil, nil
	}
	if !i.loadBlock() {
		return nil, nil
	}
	if ikey, _ := i.data.Last(); ikey == nil {
		return nil, nil
	}
	if i.blockLower != nil && i.cmp(i.data.ikey.UserKey, i.blockLower) < 0 {
		i.data.invalidateLower()
		return nil, nil
	}
	return &i.data.ikey, i.data.val
}

// Next implements internalIterator.Next, as documented in the pebble
// package.
// Note: compactionIterator.Next mirrors the implementation of Iterator.Next
// due to performance. Keep the two in sync.
func (i *Iterator) Next() (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}
	if key, val := i.data.Next(); key != nil {
		if i.blockUpper != nil && i.cmp(key.UserKey, i.blockUpper) >= 0 {
			i.data.invalidateUpper()
			return nil, nil
		}
		return key, val
	}
	for {
		if i.data.err != nil {
			i.err = i.data.err
			break
		}
		if key, _ := i.index.Next(); key == nil {
			break
		}
		if i.loadBlock() {
			key, val := i.data.First()
			if key == nil {
				return nil, nil
			}
			if i.blockUpper != nil && i.cmp(key.UserKey, i.blockUpper) >= 0 {
				i.data.invalidateUpper()
				return nil, nil
			}
			return key, val
		}
	}
	return nil, nil
}

// Prev implements internalIterator.Prev, as documented in the pebble
// package.
func (i *Iterator) Prev() (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}
	if key, val := i.data.Prev(); key != nil {
		if i.blockLower != nil && i.cmp(key.UserKey, i.blockLower) < 0 {
			i.data.invalidateLower()
			return nil, nil
		}
		return key, val
	}
	for {
		if i.data.err != nil {
			i.err = i.data.err
			break
		}
		if key, _ := i.index.Prev(); key == nil {
			break
		}
		if i.loadBlock() {
			key, val := i.data.Last()
			if key == nil {
				return nil, nil
			}
			if i.blockLower != nil && i.cmp(key.UserKey, i.blockLower) < 0 {
				i.data.invalidateLower()
				return nil, nil
			}
			return key, val
		}
	}
	return nil, nil
}

// Key implements internalIterator.Key, as documented in the pebble package.
func (i *Iterator) Key() *InternalKey {
	return i.data.Key()
}

// Value implements internalIterator.Value, as documented in the pebble
// package.
func (i *Iterator) Value() []byte {
	return i.data.Value()
}

// Valid implements internalIterator.Valid, as documented in the pebble
// package.
func (i *Iterator) Valid() bool {
	return i.data.Valid()
}

// Error implements internalIterator.Error, as documented in the pebble
// package.
func (i *Iterator) Error() error {
	if err := i.data.Error(); err != nil {
		return err
	}
	return i.err
}

// SetCloseHook sets a function that will be called when the iterator is
// closed.
func (i *Iterator) SetCloseHook(fn func(i *Iterator) error) {
	i.closeHook = fn
}

// Close implements internalIterator.Close, as documented in the pebble
// package.
func (i *Iterator) Close() error {
	if i.closeHook != nil {
		if err := i.closeHook(i); err != nil {
			return err
		}
	}
	if err := i.data.Close(); err != nil {
		return err
	}
	err := i.err
	*i = Iterator{}
	iterPool.Put(i)
	return err
}

// SetBounds implements internalIterator.SetBounds, as documented in the pebble
// package.
func (i *Iterator) SetBounds(lower, upper []byte) {
	i.lower = lower
	i.upper = upper
}

// compactionIterator is similar to Iterator but it increments the number of
// bytes that have been iterated through.
type compactionIterator struct {
	*Iterator
	bytesIterated *uint64
	prevOffset    uint64
}

func (i *compactionIterator) SeekGE(key []byte) (*InternalKey, []byte) {
	panic("pebble: SeekGE unimplemented")
}

func (i *compactionIterator) SeekPrefixGE(prefix, key []byte) (*InternalKey, []byte) {
	panic("pebble: SeekPrefixGE unimplemented")
}

func (i *compactionIterator) SeekLT(key []byte) (*InternalKey, []byte) {
	panic("pebble: SeekLT unimplemented")
}

func (i *compactionIterator) First() (*InternalKey, []byte) {
	key, val := i.Iterator.First()
	if key == nil {
		// An empty sstable will still encode the block trailer and restart points, so bytes
		// iterated must be incremented.

		// We must use i.dataBH.length instead of (4*(i.data.numRestarts+1)) to calculate the
		// number of bytes for the restart points, since i.dataBH.length accounts for
		// compression. When uncompressed, i.dataBH.length == (4*(i.data.numRestarts+1))
		*i.bytesIterated += blockTrailerLen + i.dataBH.Length
		return nil, nil
	}
	// If the sstable only has 1 entry, we are at the last entry in the block and we must
	// increment bytes iterated by the size of the block trailer and restart points.
	if i.data.nextOffset+(4*(i.data.numRestarts+1)) == int32(len(i.data.data)) {
		i.prevOffset = blockTrailerLen + i.dataBH.Length
	} else {
		// i.dataBH.length/len(i.data.data) is the compression ratio. If uncompressed, this is 1.
		// i.data.nextOffset is the uncompressed size of the first record.
		i.prevOffset = (uint64(i.data.nextOffset) * i.dataBH.Length) / uint64(len(i.data.data))
	}
	*i.bytesIterated += i.prevOffset
	return key, val
}

func (i *compactionIterator) Last() (*InternalKey, []byte) {
	panic("pebble: Last unimplemented")
}

// Note: compactionIterator.Next mirrors the implementation of Iterator.Next
// due to performance. Keep the two in sync.
func (i *compactionIterator) Next() (*InternalKey, []byte) {
	if i.err != nil {
		return nil, nil
	}
	key, val := i.data.Next()
	if key == nil {
		for {
			if i.data.err != nil {
				i.err = i.data.err
				return nil, nil
			}
			if key, _ := i.index.Next(); key == nil {
				return nil, nil
			}
			if i.loadBlock() {
				key, val = i.data.First()
				if key == nil {
					return nil, nil
				}
				break
			}
		}
	}

	// i.dataBH.length/len(i.data.data) is the compression ratio. If uncompressed, this is 1.
	// i.data.nextOffset is the uncompressed position of the current record in the block.
	// i.dataBH.offset is the offset of the block in the sstable before decompression.
	recordOffset := (uint64(i.data.nextOffset) * i.dataBH.Length) / uint64(len(i.data.data))
	curOffset := i.dataBH.Offset + recordOffset
	// Last entry in the block must increment bytes iterated by the size of the block trailer
	// and restart points.
	if i.data.nextOffset+(4*(i.data.numRestarts+1)) == int32(len(i.data.data)) {
		curOffset = i.dataBH.Offset + i.dataBH.Length + blockTrailerLen
	}
	*i.bytesIterated += uint64(curOffset - i.prevOffset)
	i.prevOffset = curOffset
	return key, val
}

func (i *compactionIterator) Prev() (*InternalKey, []byte) {
	panic("pebble: Prev unimplemented")
}

type weakCachedBlock struct {
	bh     BlockHandle
	mu     sync.RWMutex
	handle cache.WeakHandle
}

type blockTransform func([]byte) ([]byte, error)

// Reader is a table reader.
type Reader struct {
	file              vfs.File
	dbNum             uint64
	fileNum           uint64
	err               error
	index             weakCachedBlock
	filter            weakCachedBlock
	rangeDel          weakCachedBlock
	rangeDelTransform blockTransform
	propertiesBH      BlockHandle
	metaIndexBH       BlockHandle
	footerBH          BlockHandle
	opts              *Options
	cache             *cache.Cache
	compare           Compare
	split             Split
	tableFilter       *tableFilterReader
	Properties        Properties
}

// Close implements DB.Close, as documented in the pebble package.
func (r *Reader) Close() error {
	if r.err != nil {
		if r.file != nil {
			r.file.Close()
			r.file = nil
		}
		return r.err
	}
	if r.file != nil {
		r.err = r.file.Close()
		r.file = nil
		if r.err != nil {
			return r.err
		}
	}
	// Make any future calls to Get, NewIter or Close return an error.
	r.err = errors.New("pebble/table: reader is closed")
	return nil
}

// get is a testing helper that simulates a read and helps verify bloom filters
// until they are available through iterators.
func (r *Reader) get(key []byte) (value []byte, err error) {
	if r.err != nil {
		return nil, r.err
	}

	if r.tableFilter != nil {
		data, err := r.readFilter()
		if err != nil {
			return nil, err
		}
		var lookupKey []byte
		if r.split != nil {
			lookupKey = key[:r.split(key)]
		} else {
			lookupKey = key
		}
		if !r.tableFilter.mayContain(data, lookupKey) {
			return nil, base.ErrNotFound
		}
	}

	i := iterPool.Get().(*Iterator)
	if err := i.Init(r, nil, nil); err == nil {
		i.index.SeekGE(key)
		i.seekBlock(key)
	}

	if !i.Valid() || r.compare(key, i.Key().UserKey) != 0 {
		err := i.Close()
		if err == nil {
			err = base.ErrNotFound
		}
		return nil, err
	}
	return i.Value(), i.Close()
}

// NewIter returns an internal iterator for the contents of the table.
func (r *Reader) NewIter(lower, upper []byte) *Iterator {
	// NB: pebble.tableCache wraps the returned iterator with one which performs
	// reference counting on the Reader, preventing the Reader from being closed
	// until the final iterator closes.
	i := iterPool.Get().(*Iterator)
	_ = i.Init(r, lower, upper)
	return i
}

// NewCompactionIter returns an internal iterator similar to NewIter but it also increments
// the number of bytes iterated.
func (r *Reader) NewCompactionIter(bytesIterated *uint64) *compactionIterator {
	i := iterPool.Get().(*Iterator)
	_ = i.Init(r, nil /* lower */, nil /* upper */)
	return &compactionIterator{
		Iterator:      i,
		bytesIterated: bytesIterated,
	}
}

// NewRangeDelIter returns an internal iterator for the contents of the
// range-del block for the table. Returns nil if the table does not contain any
// range deletions.
func (r *Reader) NewRangeDelIter() *blockIter {
	if r.rangeDel.bh.Length == 0 {
		return nil
	}
	b, err := r.readRangeDel()
	if err != nil {
		// TODO(peter): propagate the error
		panic(err)
	}
	i := &blockIter{}
	if err := i.init(r.compare, b, r.Properties.GlobalSeqNum); err != nil {
		// TODO(peter): propagate the error
		panic(err)
	}
	return i
}

func (r *Reader) readIndex() (block, error) {
	return r.readWeakCachedBlock(&r.index, nil /* transform */)
}

func (r *Reader) readFilter() (block, error) {
	return r.readWeakCachedBlock(&r.filter, nil /* transform */)
}

func (r *Reader) readRangeDel() (block, error) {
	return r.readWeakCachedBlock(&r.rangeDel, r.rangeDelTransform)
}

func (r *Reader) readWeakCachedBlock(
	w *weakCachedBlock, transform blockTransform,
) (block, error) {
	// Fast-path for retrieving the block from a weak cache handle.
	w.mu.RLock()
	var b []byte
	if w.handle != nil {
		b = w.handle.Get()
	}
	w.mu.RUnlock()
	if b != nil {
		return b, nil
	}

	// Slow-path: read the index block from disk. This checks the cache again,
	// but that is ok because somebody else might have inserted it for us.
	h, err := r.readBlock(w.bh, transform)
	if err != nil {
		return nil, err
	}
	b = h.Get()
	if wh := h.Weak(); wh != nil {
		w.mu.Lock()
		w.handle = wh
		w.mu.Unlock()
	}
	return b, err
}

// readBlock reads and decompresses a block from disk into memory.
func (r *Reader) readBlock(
	bh BlockHandle, transform blockTransform,
) (cache.Handle, error) {
	if h := r.cache.Get(r.dbNum, r.fileNum, bh.Offset); h.Get() != nil {
		return h, nil
	}

	b := r.cache.Alloc(int(bh.Length + blockTrailerLen))
	if _, err := r.file.ReadAt(b, int64(bh.Offset)); err != nil {
		return cache.Handle{}, err
	}

	checksum0 := binary.LittleEndian.Uint32(b[bh.Length+1:])
	checksum1 := crc.New(b[:bh.Length+1]).Value()
	if checksum0 != checksum1 {
		return cache.Handle{}, errors.New("pebble/table: invalid table (checksum mismatch)")
	}

	typ := b[bh.Length]
	b = b[:bh.Length]

	switch typ {
	case noCompressionBlockType:
		break
	case snappyCompressionBlockType:
		decodedLen, err := snappy.DecodedLen(b)
		if err != nil {
			return cache.Handle{}, err
		}
		decoded := r.cache.Alloc(decodedLen)
		decoded, err = snappy.Decode(decoded, b)
		if err != nil {
			return cache.Handle{}, err
		}
		r.cache.Free(b)
		b = decoded
	default:
		return cache.Handle{}, fmt.Errorf("pebble/table: unknown block compression: %d", typ)
	}

	if transform != nil {
		// Transforming blocks is rare, so we don't bother to use cache.Alloc.
		var err error
		b, err = transform(b)
		if err != nil {
			return cache.Handle{}, err
		}
	}

	h := r.cache.Set(r.dbNum, r.fileNum, bh.Offset, b)
	return h, nil
}

func (r *Reader) transformRangeDelV1(b []byte) ([]byte, error) {
	// Convert v1 (RocksDB format) range-del blocks to v2 blocks on the fly. The
	// v1 format range-del blocks have unfragmented and unsorted range
	// tombstones. We need properly fragmented and sorted range tombstones in
	// order to serve from them directly.
	iter := &blockIter{}
	if err := iter.init(r.compare, b, r.Properties.GlobalSeqNum); err != nil {
		return nil, err
	}
	var tombstones []rangedel.Tombstone
	for key, value := iter.First(); key != nil; key, value = iter.Next() {
		t := rangedel.Tombstone{
			Start: *key,
			End:   value,
		}
		tombstones = append(tombstones, t)
	}
	rangedel.Sort(r.compare, tombstones)

	// Fragment the tombstones, outputting them directly to a block writer.
	rangeDelBlock := blockWriter{
		restartInterval: 1,
	}
	frag := rangedel.Fragmenter{
		Cmp: r.compare,
		Emit: func(fragmented []rangedel.Tombstone) {
			for i := range fragmented {
				t := &fragmented[i]
				rangeDelBlock.add(t.Start, t.End)
			}
		},
	}
	for i := range tombstones {
		t := &tombstones[i]
		frag.Add(t.Start, t.End)
	}
	frag.Finish()

	// Return the contents of the constructed v2 format range-del block.
	return rangeDelBlock.finish(), nil
}

func (r *Reader) readMetaindex(metaindexBH BlockHandle, o *Options) error {
	b, err := r.readBlock(metaindexBH, nil /* transform */)
	if err != nil {
		return err
	}
	i, err := newRawBlockIter(bytes.Compare, b.Get())
	b.Release()
	if err != nil {
		return err
	}

	meta := map[string]BlockHandle{}
	for valid := i.First(); valid; valid = i.Next() {
		bh, n := decodeBlockHandle(i.Value())
		if n == 0 {
			return errors.New("pebble/table: invalid table (bad filter block handle)")
		}
		meta[string(i.Key().UserKey)] = bh
	}
	if err := i.Close(); err != nil {
		return err
	}

	if bh, ok := meta[metaPropertiesName]; ok {
		b, err = r.readBlock(bh, nil /* transform */)
		if err != nil {
			return err
		}
		data := b.Get()
		r.propertiesBH = bh
		err := r.Properties.load(data, bh.Offset)
		b.Release()
		if err != nil {
			return err
		}
	}

	if bh, ok := meta[metaRangeDelV2Name]; ok {
		r.rangeDel.bh = bh
	} else if bh, ok := meta[metaRangeDelName]; ok {
		r.rangeDel.bh = bh
		r.rangeDelTransform = r.transformRangeDelV1
	}

	for name, fp := range r.opts.Filters {
		types := []struct {
			ftype  FilterType
			prefix string
		}{
			{TableFilter, "fullfilter."},
		}
		var done bool
		for _, t := range types {
			if bh, ok := meta[t.prefix+name]; ok {
				r.filter.bh = bh

				switch t.ftype {
				case TableFilter:
					r.tableFilter = newTableFilterReader(fp)
				default:
					return fmt.Errorf("unknown filter type: %v", t.ftype)
				}

				done = true
				break
			}
		}
		if done {
			break
		}
	}
	return nil
}

// Layout returns the layout (block organization) for an sstable.
func (r *Reader) Layout() (*Layout, error) {
	if r.err != nil {
		return nil, r.err
	}

	l := &Layout{
		Data:       make([]BlockHandle, 0, r.Properties.NumDataBlocks),
		Filter:     r.filter.bh,
		RangeDel:   r.rangeDel.bh,
		Properties: r.propertiesBH,
		MetaIndex:  r.metaIndexBH,
		Footer:     r.footerBH,
	}

	index, err := r.readIndex()
	if err != nil {
		return nil, err
	}

	if r.Properties.IndexPartitions == 0 {
		l.Index = append(l.Index, r.index.bh)
		iter, _ := newBlockIter(r.compare, index)
		for key, value := iter.First(); key != nil; key, value = iter.Next() {
			dataBH, n := decodeBlockHandle(value)
			if n == 0 || n != len(value) {
				return nil, errors.New("pebble/table: corrupt index entry")
			}
			l.Data = append(l.Data, dataBH)
		}
	} else {
		l.TopIndex = r.index.bh
		topIter, _ := newBlockIter(r.compare, index)
		for key, value := topIter.First(); key != nil; key, value = topIter.Next() {
			indexBH, n := decodeBlockHandle(value)
			if n == 0 || n != len(value) {
				return nil, errors.New("pebble/table: corrupt index entry")
			}
			l.Index = append(l.Index, indexBH)

			subIndex, err := r.readBlock(indexBH, nil /* transform */)
			if err != nil {
				return nil, err
			}
			iter, _ := newBlockIter(r.compare, subIndex.Get())
			for key, value := iter.First(); key != nil; key, value = iter.Next() {
				dataBH, n := decodeBlockHandle(value)
				if n == 0 || n != len(value) {
					return nil, errors.New("pebble/table: corrupt index entry")
				}
				l.Data = append(l.Data, dataBH)
			}
			subIndex.Release()
		}
	}

	return l, nil
}

// NewReader returns a new table reader for the file. Closing the reader will
// close the file.
func NewReader(f vfs.File, dbNum, fileNum uint64, o *Options) (*Reader, error) {
	o = o.EnsureDefaults()

	r := &Reader{
		file:    f,
		dbNum:   dbNum,
		fileNum: fileNum,
		opts:    o,
		cache:   o.Cache,
		compare: o.Comparer.Compare,
		split:   o.Comparer.Split,
	}
	if f == nil {
		r.err = errors.New("pebble/table: nil file")
		return r, r.err
	}
	footer, err := readFooter(f)
	if err != nil {
		r.err = err
		return r, r.err
	}
	// Read the metaindex.
	if err := r.readMetaindex(footer.metaindexBH, o); err != nil {
		r.err = err
		return r, r.err
	}
	r.index.bh = footer.indexBH
	r.metaIndexBH = footer.metaindexBH
	r.footerBH = footer.footerBH

	if r.Properties.ComparerName == "" {
		r.compare = o.Comparer.Compare
		r.split = o.Comparer.Split
	} else if comparer, ok := o.Comparers[r.Properties.ComparerName]; ok {
		r.compare = comparer.Compare
		r.split = comparer.Split
	} else {
		r.err = fmt.Errorf("pebble/table: %d: unknown comparer %s",
			fileNum, r.Properties.ComparerName)
	}

	if name := r.Properties.MergerName; name != "" && name != "nullptr" {
		if _, ok := o.Mergers[r.Properties.MergerName]; !ok {
			r.err = fmt.Errorf("pebble/table: %d: unknown merger %s",
				fileNum, r.Properties.MergerName)
		}
	}
	return r, r.err
}

// Layout describes the block organization of an sstable.
type Layout struct {
	Data       []BlockHandle
	Index      []BlockHandle
	TopIndex   BlockHandle
	Filter     BlockHandle
	RangeDel   BlockHandle
	Properties BlockHandle
	MetaIndex  BlockHandle
	Footer     BlockHandle
}

// Describe returns a description of the layout. If the verbose parameter is
// true, details of the structure of each block are returned as well.
func (l *Layout) Describe(w io.Writer, verbose bool, r *Reader) {
	type block struct {
		BlockHandle
		name string
	}
	var blocks []block

	for i := range l.Data {
		blocks = append(blocks, block{l.Data[i], "data"})
	}
	for i := range l.Index {
		blocks = append(blocks, block{l.Index[i], "index"})
	}
	if l.TopIndex.Length != 0 {
		blocks = append(blocks, block{l.TopIndex, "top-index"})
	}
	if l.Filter.Length != 0 {
		blocks = append(blocks, block{l.Filter, "filter"})
	}
	if l.RangeDel.Length != 0 {
		blocks = append(blocks, block{l.RangeDel, "range-del"})
	}
	if l.Properties.Length != 0 {
		blocks = append(blocks, block{l.Properties, "properties"})
	}
	if l.MetaIndex.Length != 0 {
		blocks = append(blocks, block{l.MetaIndex, "meta-index"})
	}
	if l.Footer.Length != 0 {
		if l.Footer.Length == levelDBFooterLen {
			blocks = append(blocks, block{l.Footer, "leveldb-footer"})
		} else {
			blocks = append(blocks, block{l.Footer, "footer"})
		}
	}

	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Offset < blocks[j].Offset
	})

	for i := range blocks {
		b := &blocks[i]
		fmt.Fprintf(w, "%10d  %s (%d)\n", b.Offset, b.name, b.Length)

		if !verbose {
			continue
		}
		if b.name == "footer" || b.name == "leveldb-footer" || b.name == "filter" {
			continue
		}

		h, err := r.readBlock(b.BlockHandle, nil /* transform */)
		if err != nil {
			fmt.Fprintf(w, "  [err: %s]\n", err)
			continue
		}

		getRestart := func(data []byte, restarts, i int32) int32 {
			return int32(binary.LittleEndian.Uint32(data[restarts+4*i:]))
		}

		formatIsRestart := func(data []byte, restarts, numRestarts, offset int32) {
			i := sort.Search(int(numRestarts), func(i int) bool {
				return getRestart(data, restarts, int32(i)) >= offset
			})
			if i < int(numRestarts) && getRestart(data, restarts, int32(i)) == offset {
				fmt.Fprintf(w, " [restart]\n")
			} else {
				fmt.Fprintf(w, "\n")
			}
		}

		formatRestarts := func(data []byte, restarts, numRestarts int32) {
			for i := int32(0); i < numRestarts; i++ {
				offset := getRestart(data, restarts, i)
				fmt.Fprintf(w, "%10d    [restart %d]\n",
					b.Offset+uint64(restarts+4*i), b.Offset+uint64(offset))
			}
		}

		switch b.name {
		case "data":
			iter, _ := newBlockIter(r.compare, h.Get())
			for key, _ := iter.First(); key != nil; key, _ = iter.Next() {
				ptr := unsafe.Pointer(uintptr(iter.ptr) + uintptr(iter.offset))
				shared, ptr := decodeVarint(ptr)
				unshared, ptr := decodeVarint(ptr)
				value, _ := decodeVarint(ptr)

				fmt.Fprintf(w, "%10d    record (%d+%d+%d/%d)",
					b.Offset+uint64(iter.offset), shared, unshared, value, iter.nextOffset-iter.offset)
				formatIsRestart(iter.data, iter.restarts, iter.numRestarts, iter.offset)
			}
			formatRestarts(iter.data, iter.restarts, iter.numRestarts)
		case "index", "top-index":
			iter, _ := newBlockIter(r.compare, h.Get())
			for key, value := iter.First(); key != nil; key, value = iter.Next() {
				bh, n := decodeBlockHandle(value)
				if n == 0 || n != len(value) {
					fmt.Fprintf(w, "%10d    [err: %s]\n", b.Offset+uint64(iter.offset), err)
					continue
				}
				fmt.Fprintf(w, "%10d    block:%d/%d",
					b.Offset+uint64(iter.offset), bh.Offset, bh.Length)
				formatIsRestart(iter.data, iter.restarts, iter.numRestarts, iter.offset)
			}
			formatRestarts(iter.data, iter.restarts, iter.numRestarts)
		case "properties":
			iter, _ := newRawBlockIter(r.compare, h.Get())
			for valid := iter.First(); valid; valid = iter.Next() {
				fmt.Fprintf(w, "%10d    %s (%d)",
					b.Offset+uint64(iter.offset), iter.Key().UserKey, iter.nextOffset-iter.offset)
				formatIsRestart(iter.data, iter.restarts, iter.numRestarts, iter.offset)
			}
			formatRestarts(iter.data, iter.restarts, iter.numRestarts)
		case "meta-index":
			iter, _ := newRawBlockIter(r.compare, h.Get())
			for valid := iter.First(); valid; valid = iter.Next() {
				value := iter.Value()
				bh, n := decodeBlockHandle(value)
				if n == 0 || n != len(value) {
					fmt.Fprintf(w, "%10d    [err: %s]\n", b.Offset+uint64(iter.offset), err)
					continue
				}

				fmt.Fprintf(w, "%10d    %s block:%d/%d",
					b.Offset+uint64(iter.offset), iter.Key().UserKey,
					bh.Offset, bh.Length)
				formatIsRestart(iter.data, iter.restarts, iter.numRestarts, iter.offset)
			}
			formatRestarts(iter.data, iter.restarts, iter.numRestarts)
		}

		h.Release()
	}
}
