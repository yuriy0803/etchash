// Copyright 2015 The go-ethereum Authors
// Copyright 2015 Lefteris Karapetsas <lefteris@refu.co>
// Copyright 2015 Matthew Wampler-Doty <matthew.wampler.doty@gmail.com>
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package etchash

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"math/rand"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/edsrzf/mmap-go"
	"github.com/yuriy0803/core-geth1/common"
	"github.com/yuriy0803/core-geth1/common/hexutil"
	lrupkg "github.com/yuriy0803/core-geth1/common/lru"
	"github.com/yuriy0803/core-geth1/crypto"
	"github.com/yuriy0803/core-geth1/log"
)

var (
	maxUint256  = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))
	sharedLight = new(Light)

	// algorithmRevision is the data structure version used for file naming.
	algorithmRevision = 23

	// dumpMagic is a dataset dump header to sanity check a data dump.
	dumpMagic = []uint32{0xbaddcafe, 0xfee1dead}

	ErrInvalidDumpMagic = errors.New("invalid dump magic")
)

const (
	cacheSizeForTesting uint64 = 1024
	dagSizeForTesting   uint64 = 1024 * 32
	cachesInMem                = 2
	cachesOnDisk               = 3
	cachesLockMmap             = false
	datasetsInMem              = 1
	datasetsOnDisk             = 2
	datasetsLockMmap           = false
)

var DefaultDir = defaultDir()

func defaultDir() string {
	home := os.Getenv("HOME")
	if user, err := user.Current(); err == nil {
		home = user.HomeDir
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Etchash")
	}
	return filepath.Join(home, ".etchash")
}

// isLittleEndian returns whether the local system is running in little or big
// endian byte order.
func isLittleEndian() bool {
	n := uint32(0x01020304)
	return *(*byte)(unsafe.Pointer(&n)) == 0x04
}

// uint32Array2ByteArray returns the bytes represented by uint32 array c
// nolint:unused
func uint32Array2ByteArray(c []uint32) []byte {
	buf := make([]byte, len(c)*4)
	if isLittleEndian() {
		for i, v := range c {
			binary.LittleEndian.PutUint32(buf[i*4:], v)
		}
	} else {
		for i, v := range c {
			binary.BigEndian.PutUint32(buf[i*4:], v)
		}
	}
	return buf
}

// bytes2Keccak256 returns the keccak256 hash as a hex string (0x prefixed)
// for a given uint32 array (cache/dataset)
func uint32Array2Keccak256(data []uint32) string {
	// convert to bytes
	bytes := uint32Array2ByteArray(data)
	// hash with keccak256
	digest := crypto.Keccak256(bytes)
	// return hex string
	return hexutil.Encode(digest)
}

// memoryMap tries to memory map a file of uint32s for read only access.
func memoryMap(path string, lock bool) (*os.File, mmap.MMap, []uint32, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, nil, nil, err
	}

	mem, buffer, err := memoryMapFile(file, false)
	if err != nil {
		file.Close()
		return nil, nil, nil, err
	}
	for i, magic := range dumpMagic {
		if buffer[i] != magic {
			mem.Unmap()
			file.Close()
			return nil, nil, nil, ErrInvalidDumpMagic
		}
	}
	if lock {
		if err := mem.Lock(); err != nil {
			mem.Unmap()
			file.Close()
			return nil, nil, nil, err
		}
	}
	return file, mem, buffer[len(dumpMagic):], err
}

// memoryMapFile tries to memory map an already opened file descriptor.
func memoryMapFile(file *os.File, write bool) (mmap.MMap, []uint32, error) {
	// Try to memory map the file
	flag := mmap.RDONLY
	if write {
		flag = mmap.RDWR
	}
	mem, err := mmap.Map(file, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	// Yay, we managed to memory map the file, here be dragons
	header := *(*reflect.SliceHeader)(unsafe.Pointer(&mem))
	header.Len /= 4
	header.Cap /= 4

	return mem, *(*[]uint32)(unsafe.Pointer(&header)), nil
}

// memoryMapAndGenerate tries to memory map a temporary file of uint32s for write
// access, fill it with the data from a generator and then move it into the final
// path requested.
func memoryMapAndGenerate(path string, size uint64, lock bool, generator func(buffer []uint32)) (*os.File, mmap.MMap, []uint32, error) {
	// Ensure the data folder exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, nil, nil, err
	}
	// Create a huge temporary empty file to fill with data
	temp := path + "." + strconv.Itoa(rand.Int())

	dump, err := os.Create(temp)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = ensureSize(dump, int64(len(dumpMagic))*4+int64(size)); err != nil {
		dump.Close()
		os.Remove(temp)
		return nil, nil, nil, err
	}
	// Memory map the file for writing and fill it with the generator
	mem, buffer, err := memoryMapFile(dump, true)
	if err != nil {
		dump.Close()
		os.Remove(temp)
		return nil, nil, nil, err
	}
	copy(buffer, dumpMagic)

	data := buffer[len(dumpMagic):]
	generator(data)

	if err := mem.Unmap(); err != nil {
		return nil, nil, nil, err
	}
	if err := dump.Close(); err != nil {
		return nil, nil, nil, err
	}
	if err := os.Rename(temp, path); err != nil {
		return nil, nil, nil, err
	}
	return memoryMap(path, lock)
}

type cacheOrDataset interface {
	*cache | *dataset
}

// lru tracks caches or datasets by their last use time, keeping at most N of them.
type lru[T cacheOrDataset] struct {
	what string
	new  func(epoch uint64, epochLength uint64) T
	mu   sync.Mutex
	// Items are kept in a LRU cache, but there is a special case:
	// We always keep an item for (highest seen epoch) + 1 as the 'future item'.
	cache      lrupkg.BasicLRU[uint64, T]
	future     uint64
	futureItem T
}

// newlru create a new least-recently-used cache for either the verification caches
// or the mining datasets.
func newlru[T cacheOrDataset](maxItems int, new func(epoch uint64, epochLength uint64) T) *lru[T] {
	var what string
	switch any(T(nil)).(type) {
	case *cache:
		what = "cache"
	case *dataset:
		what = "dataset"
	default:
		panic("unknown type")
	}
	return &lru[T]{
		what:  what,
		new:   new,
		cache: lrupkg.NewBasicLRU[uint64, T](maxItems),
	}
}

// get retrieves or creates an item for the given epoch. The first return value is always
// non-nil. The second return value is non-nil if lru thinks that an item will be useful in
// the near future.
func (lru *lru[T]) get(epoch uint64, epochLength uint64, ecip1099FBlock *uint64) (item, future T) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	// Use the sum of epoch and epochLength as the cache key.
	// This is not perfectly safe, but it's good enough (at least for the first 30000 epochs, or the first 427 years).
	cacheKey := epochLength + epoch

	// Get or create the item for the requested epoch.
	item, ok := lru.cache.Get(cacheKey)
	if !ok {
		if lru.future > 0 && lru.future == epoch {
			item = lru.futureItem
		} else {
			log.Trace("Requiring new etchash "+lru.what, "epoch", epoch)
			item = lru.new(epoch, epochLength)
		}
		lru.cache.Add(cacheKey, item)
	}

	// Ensure pre-generation handles ecip-1099 changeover correctly
	var nextEpoch = epoch + 1
	var nextEpochLength = epochLength
	if ecip1099FBlock != nil {
		nextEpochBlock := nextEpoch * epochLength
		// Note that == demands that the ECIP1099 activation block is situated
		// at the beginning of an epoch.
		// https://github.com/ethereumclassic/ECIPs/blob/master/_specs/ecip-1099.md#implementation
		if nextEpochBlock == *ecip1099FBlock && epochLength == epochLengthDefault {
			nextEpoch = nextEpoch / 2
			nextEpochLength = epochLengthECIP1099
		}
	}

	// Update the 'future item' if epoch is larger than previously seen.
	// Last conditional clause ('lru.future > nextEpoch') handles the ECIP1099 case where
	// the next epoch is expected to be LESSER THAN that of the previous state's future epoch number.
	if epoch < maxEpoch-1 && lru.future != nextEpoch {
		log.Trace("Requiring new future etchash "+lru.what, "epoch", nextEpoch)
		future = lru.new(nextEpoch, nextEpochLength)
		lru.future = nextEpoch
		lru.futureItem = future
	}
	return item, future
}

// cache wraps an etchash cache with some metadata to allow easier concurrent use.
type cache struct {
	epoch       uint64    // Epoch for which this cache is relevant
	epochLength uint64    // Epoch length (ECIP-1099)
	uip1Epoch   *uint64   // Epoch for UIP-1 activation
	dump        *os.File  // File descriptor of the memory mapped cache
	mmap        mmap.MMap // Memory map itself to unmap before releasing
	cache       []uint32  // The actual cache data content (may be memory mapped)
	once        sync.Once // Ensures the cache is generated only once
	used        time.Time
}

// newCache creates a new ethash verification cache.
func newCache(epoch uint64, epochLength uint64, uip1Epoch *uint64) *cache {
	return &cache{epoch: epoch, epochLength: epochLength, uip1Epoch: uip1Epoch}
}

// generate ensures that the cache content is generated before use.
func (c *cache) generate(dir string, limit int, lock bool, test bool) {
	c.once.Do(func() {
		size := cacheSize(c.epoch)
		seed := seedHash(c.epoch*c.epochLength + 1)
		if test {
			size = 1024
		}
		// If we don't store anything on disk, generate and return.
		if dir == "" {
			c.cache = make([]uint32, size/4)
			generateCache(c.cache, c.epoch, c.epochLength, c.uip1Epoch, seed)
			return
		}
		// Disk storage is needed, this will get fancy
		var endian string
		if !isLittleEndian() {
			endian = ".be"
		}

		// The file path naming scheme was changed to include epoch values in the filename,
		// which enables a filepath glob with scan to identify out-of-bounds caches and remove them.
		// The legacy path declaration is provided below as a comment for reference.
		//
		// path := filepath.Join(dir, fmt.Sprintf("cache-R%d-%x%s", algorithmRevision, seed[:8], endian))                 // LEGACY
		path := filepath.Join(dir, fmt.Sprintf("cache-R%d-%d-%x%s", algorithmRevision, c.epoch, seed[:8], endian)) // CURRENT
		logger := log.New("epoch", c.epoch, "epochLength", c.epochLength)

		// We're about to mmap the file, ensure that the mapping is cleaned up when the
		// cache becomes unused.
		runtime.SetFinalizer(c, (*cache).finalizer)

		// Try to load the file from disk and memory map it
		var err error
		c.dump, c.mmap, c.cache, err = memoryMap(path, lock)
		if err == nil {
			logger.Debug("Loaded old etchash cache from disk")
			return
		}
		logger.Debug("Failed to load old etchash cache", "err", err)

		// No usable previous cache available, create a new cache file to fill
		c.dump, c.mmap, c.cache, err = memoryMapAndGenerate(path, size, lock, func(buffer []uint32) { generateCache(buffer, c.epoch, c.epochLength, c.uip1Epoch, seed) })
		if err != nil {
			logger.Error("Failed to generate mapped etchash cache", "err", err)

			c.cache = make([]uint32, size/4)
			generateCache(c.cache, c.epoch, c.epochLength, c.uip1Epoch, seed)
		}
		// Iterate over all cache file instances, deleting any out of bounds (where epoch is below lower limit, or above upper limit).
		matches, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("cache-R%d*", algorithmRevision)))
		for _, file := range matches {
			var ar int   // algorithm revision
			var e uint64 // epoch
			var s string // seed
			if _, err := fmt.Sscanf(filepath.Base(file), "cache-R%d-%d-%s"+endian, &ar, &e, &s); err != nil {
				// There is an unrecognized file in this directory.
				// See if the name matches the expected pattern of the legacy naming scheme.
				if _, err := fmt.Sscanf(filepath.Base(file), "cache-R%d-%s"+endian, &ar, &s); err == nil {
					// This file matches the previous generation naming pattern (sans epoch).
					if err := os.Remove(file); err != nil {
						logger.Error("Failed to remove legacy ethash cache file", "file", file, "err", err)
					} else {
						logger.Warn("Deleted legacy ethash cache file", "path", file)
					}
				}
				// Else the file is unrecognized (unknown name format), leave it alone.
				continue
			}
			if e <= c.epoch-uint64(limit) || e > c.epoch+1 {
				if err := os.Remove(file); err == nil {
					logger.Debug("Deleted ethash cache file", "target.epoch", e, "file", file)
				} else {
					logger.Error("Failed to delete ethash cache file", "target.epoch", e, "file", file, "err", err)
				}
			}
		}
	})
}

// finalizer unmaps the memory and closes the file.
func (c *cache) finalizer() {
	if c.mmap != nil {
		c.mmap.Unmap()
		c.dump.Close()
		c.mmap, c.dump = nil, nil
	}
}

func (c *cache) compute(dagSize uint64, hash common.Hash, nonce uint64) (common.Hash, common.Hash) {
	// ret := C.etchash_light_compute_internal(cache.ptr, C.uint64_t(dagSize), hashToH256(hash), C.uint64_t(nonce))
	digest, result := hashimotoLight(dagSize, c.cache, hash.Bytes(), nonce)
	// Caches are unmapped in a finalizer. Ensure that the cache stays alive
	// until after the call to hashimotoLight so it's not unmapped while being used.
	runtime.KeepAlive(c)
	return common.BytesToHash(digest), common.BytesToHash(result)
}

// Light implements the Verify half of the proof of work. It uses a few small
// in-memory caches to verify the nonces found by Full.
type Light struct {
	test bool // If set, use a smaller cache size

	mu     sync.Mutex        // Protects the per-epoch map of verification caches
	caches map[uint64]*cache // Currently maintained verification caches
	future *cache            // Pre-generated cache for the estimated future DAG

	NumCaches      int // Maximum number of caches to keep before eviction (only init, don't modify)
	ecip1099FBlock *uint64
	uip1Epoch      *uint64
}

// Verify checks whether the block's nonce is valid.
func (l *Light) Verify(block Block) (bool, int64) {
	// TODO: do etchash_quick_verify before getCache in order
	// to prevent DOS attacks.
	blockNum := block.NumberU64()
	if blockNum >= epochLengthDefault*2048 {
		log.Debug(fmt.Sprintf("block number %d too high, limit is %d", blockNum, epochLengthDefault*2048))
		return false, 0
	}

	difficulty := block.Difficulty()
	/* Cannot happen if block header diff is validated prior to PoW, but can
		 happen if PoW is checked first due to parallel PoW checking.
		 We could check the minimum valid difficulty but for SoC we avoid (duplicating)
	   Ethereum protocol consensus rules here which are not in scope of Etchash
	*/
	if difficulty.Cmp(common.Big0) == 0 {
		log.Debug("invalid block difficulty")
		return false, 0
	}

	epochLength := calcEpochLength(blockNum, l.ecip1099FBlock)
	epoch := calcEpoch(blockNum, epochLength)

	cache := l.getCache(blockNum)
	dagSize := datasetSize(epoch)
	if l.test {
		dagSize = dagSizeForTesting
	}
	// Recompute the hash using the cache.
	mixDigest, result := cache.compute(uint64(dagSize), block.HashNoNonce(), block.Nonce())

	// avoid mixdigest malleability as it's not included in a block's "hashNononce"
	if block.MixDigest() != mixDigest {
		return false, 0
	}

	// The actual check.
	target := new(big.Int).Div(maxUint256, difficulty)
	actual := new(big.Int).Div(maxUint256, result.Big())
	return result.Big().Cmp(target) <= 0, actual.Int64()
}

// compute() to get mixhash and result
func (l *Light) Compute(blockNum uint64, hashNoNonce common.Hash, nonce uint64) (mixDigest common.Hash, result common.Hash) {
	epochLength := calcEpochLength(blockNum, l.ecip1099FBlock)
	epoch := calcEpoch(blockNum, epochLength)

	cache := l.getCache(blockNum)
	dagSize := datasetSize(epoch)
	return cache.compute(uint64(dagSize), hashNoNonce, nonce)
}

func (l *Light) getCache(blockNum uint64) *cache {
	var c *cache
	epochLength := calcEpochLength(blockNum, l.ecip1099FBlock)
	epoch := calcEpoch(blockNum, epochLength)

	// If we have a PoW for that epoch, use that
	l.mu.Lock()
	if l.caches == nil {
		l.caches = make(map[uint64]*cache)
	}
	if l.NumCaches == 0 {
		l.NumCaches = 3
	}
	c = l.caches[epoch]
	if c == nil {
		// No cached DAG, evict the oldest if the cache limit was reached
		if len(l.caches) >= l.NumCaches {
			var evict *cache
			for _, cache := range l.caches {
				if evict == nil || evict.used.After(cache.used) {
					evict = cache
				}
			}
			log.Debug(fmt.Sprintf("Evicting DAG for epoch %d in favour of epoch %d", evict.epoch, epoch))
			delete(l.caches, evict.epoch)
		}
		// If we have the new DAG pre-generated, use that, otherwise create a new one
		if l.future != nil && l.future.epoch == epoch {
			log.Debug(fmt.Sprintf("Using pre-generated DAG for epoch %d", epoch))
			c, l.future = l.future, nil
		} else {
			log.Debug(fmt.Sprintf("No pre-generated DAG available, creating new for epoch %d", epoch))
			c = &cache{epoch: epoch, epochLength: epochLength}
		}
		l.caches[epoch] = c

		var nextEpoch = epoch + 1
		var nextEpochLength = epochLength
		var nextEpochBlock = nextEpoch * epochLength

		if l.ecip1099FBlock != nil {
			if nextEpochBlock == *l.ecip1099FBlock && epochLength == epochLengthDefault {
				nextEpoch = nextEpoch / 2
				nextEpochLength = epochLengthECIP1099
			}
		}

		// If we just used up the future cache, or need a refresh, regenerate
		if l.future == nil || l.future.epoch <= epoch {
			log.Debug(fmt.Sprintf("Pre-generating DAG for epoch %d", nextEpoch))
			l.future = &cache{epoch: nextEpoch, epochLength: nextEpochLength}
			go l.future.generate(defaultDir(), cachesOnDisk, cachesLockMmap, l.test)
		}
	}
	c.used = time.Now()
	l.mu.Unlock()

	// Wait for generation finish and return the cache
	c.generate(defaultDir(), cachesOnDisk, cachesLockMmap, l.test)
	return c
}

// / dataset wraps an etchash dataset with some metadata to allow easier concurrent use.
type dataset struct {
	epoch       uint64      // Epoch for which this cache is relevant
	epochLength uint64      // Epoch length (ECIP-1099)
	uip1Epoch   *uint64     // Epoch for UIP-1 activation
	dump        *os.File    // File descriptor of the memory mapped cache
	mmap        mmap.MMap   // Memory map itself to unmap before releasing
	dataset     []uint32    // The actual cache data content
	once        sync.Once   // Ensures the cache is generated only once
	done        atomic.Bool // Atomic flag to determine generation status
	used        time.Time
}

// newDataset creates a new etchash mining dataset and returns it as a plain Go
// interface to be usable in an LRU cache.
func newDataset(epoch uint64, epochLength uint64, uip1Epoch *uint64) *dataset {
	return &dataset{epoch: epoch, epochLength: epochLength, uip1Epoch: uip1Epoch}
}

// generate ensures that the dataset content is generated before use.
func (d *dataset) generate(dir string, limit int, lock bool, test bool) {
	d.once.Do(func() {
		// Mark the dataset generated after we're done. This is needed for remote
		defer d.done.Store(true)

		csize := cacheSize(d.epoch)
		dsize := datasetSize(d.epoch)
		seed := seedHash(d.epoch*d.epochLength + 1)
		if test {
			csize = 1024
			dsize = 32 * 1024
		}
		// If we don't store anything on disk, generate and return
		if dir == "" {
			cache := make([]uint32, csize/4)
			generateCache(cache, d.epoch, d.epochLength, d.uip1Epoch, seed)

			d.dataset = make([]uint32, dsize/4)
			generateDataset(d.dataset, d.epoch, d.epochLength, cache)

			return
		}
		// Disk storage is needed, this will get fancy
		var endian string
		if !isLittleEndian() {
			endian = ".be"
		}
		path := filepath.Join(dir, fmt.Sprintf("full-R%d-%d-%x%s", algorithmRevision, d.epoch, seed[:8], endian))
		logger := log.New("epoch", d.epoch)

		// We're about to mmap the file, ensure that the mapping is cleaned up when the
		// cache becomes unused.
		runtime.SetFinalizer(d, (*dataset).finalizer)

		// Try to load the file from disk and memory map it
		var err error
		d.dump, d.mmap, d.dataset, err = memoryMap(path, lock)
		if err == nil {
			logger.Debug("Loaded old etchash dataset from disk", "path", path)
			return
		}
		logger.Debug("Failed to load old etchash dataset", "err", err)

		// No usable previous dataset available, create a new dataset file to fill
		cache := make([]uint32, csize/4)
		generateCache(cache, d.epoch, d.epochLength, d.uip1Epoch, seed)

		d.dump, d.mmap, d.dataset, err = memoryMapAndGenerate(path, dsize, lock, func(buffer []uint32) { generateDataset(buffer, d.epoch, d.epochLength, cache) })
		if err != nil {
			logger.Error("Failed to generate mapped etchash dataset", "err", err)

			d.dataset = make([]uint32, dsize/4)
			generateDataset(d.dataset, d.epoch, d.epochLength, cache)
		}
		// Iterate over all full file instances, deleting any out of bounds (where epoch is below lower limit, or above upper limit).
		matches, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("full-R%d*", algorithmRevision)))
		for _, file := range matches {
			var ar int   // algorithm revision
			var e uint64 // epoch
			var s string // seed
			if _, err := fmt.Sscanf(filepath.Base(file), "full-R%d-%d-%s"+endian, &ar, &e, &s); err != nil {
				// There is an unrecognized file in this directory.
				// See if the name matches the expected pattern of the legacy naming scheme.
				if _, err := fmt.Sscanf(filepath.Base(file), "full-R%d-%s"+endian, &ar, &s); err == nil {
					// This file matches the previous generation naming pattern (sans epoch).
					if err := os.Remove(file); err != nil {
						logger.Error("Failed to remove legacy ethash full file", "file", file, "err", err)
					} else {
						logger.Warn("Deleted legacy ethash full file", "path", file)
					}
				}
				// Else the file is unrecognized (unknown name format), leave it alone.
				continue
			}
			if e <= d.epoch-uint64(limit) || e > d.epoch+1 {
				if err := os.Remove(file); err == nil {
					logger.Debug("Deleted ethash full file", "target.epoch", e, "file", file)
				} else {
					logger.Error("Failed to delete ethash full file", "target.epoch", e, "file", file, "err", err)
				}
			}
		}
	})
}

// generated returns whether this particular dataset finished generating already
// or not (it may not have been started at all). This is useful for remote miners
// to default to verification caches instead of blocking on DAG generations.
func (d *dataset) generated() bool {
	return d.done.Load()
}

// finalizer closes any file handlers and memory maps open.
func (d *dataset) finalizer() {
	if d.mmap != nil {
		d.mmap.Unmap()
		d.dump.Close()
		d.mmap, d.dump = nil, nil
	}
}

// MakeDAG generates a new etchash dataset and optionally stores it to disk.
func MakeDAG(block uint64, epochLength uint64, dir string) {
	epoch := calcEpoch(block, epochLength)
	d := dataset{epoch: epoch, epochLength: epochLength}
	d.generate(dir, math.MaxInt32, false, false)
}

// Full implements the Search half of the proof of work.
type Full struct {
	Dir string // use this to specify a non-default DAG directory

	test     bool // if set use a smaller DAG size
	turbo    bool
	hashRate int32

	mu             sync.Mutex // protects dag
	current        *dataset   // current full DAG
	ecip1099FBlock *uint64
	uip1Epoch      *uint64
}

func (pow *Full) getDAG(blockNum uint64) (d *dataset) {
	epochLength := calcEpochLength(blockNum, pow.ecip1099FBlock)
	epoch := calcEpoch(blockNum, epochLength)
	pow.mu.Lock()
	if pow.current != nil && pow.current.epoch == epoch {
		d = pow.current
	} else {
		d = &dataset{epoch: epoch, epochLength: uint64(epochLength)}
		pow.current = d
	}
	pow.mu.Unlock()
	// wait for it to finish generating.
	d.generate(defaultDir(), datasetsOnDisk, datasetsLockMmap, pow.test)
	return d
}

func (pow *Full) Search(block Block, stop <-chan struct{}, index int) (nonce uint64, mixDigest []byte) {
	dag := pow.getDAG(block.NumberU64())

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	diff := block.Difficulty()

	i := int64(0)
	starti := i
	start := time.Now().UnixNano()
	previousHashrate := int32(0)

	nonce = uint64(r.Int63())
	hash := block.HashNoNonce()
	target := new(big.Int).Div(maxUint256, diff)
	for {
		select {
		case <-stop:
			atomic.AddInt32(&pow.hashRate, -previousHashrate)
			return 0, nil
		default:
			i++

			// we don't have to update hash rate on every nonce, so update after
			// first nonce check and then after 2^X nonces
			if i == 2 || ((i % (1 << 16)) == 0) {
				elapsed := time.Now().UnixNano() - start
				hashes := (float64(1e9) / float64(elapsed)) * float64(i-starti)
				hashrateDiff := int32(hashes) - previousHashrate
				previousHashrate = int32(hashes)
				atomic.AddInt32(&pow.hashRate, hashrateDiff)
			}

			digest, result := hashimotoFull(dag.dataset, hash.Bytes(), nonce)
			// result := h256ToHash(ret.result).Big()
			bigres := common.BytesToHash(result).Big()
			// TODO: disagrees with the spec https://github.com/ethereum/wiki/wiki/Etchash#mining
			if digest != nil && bigres.Cmp(target) <= 0 {
				mixDigest = digest
				atomic.AddInt32(&pow.hashRate, -previousHashrate)
				return nonce, mixDigest
			}
			nonce += 1
		}

		if !pow.turbo {
			time.Sleep(20 * time.Microsecond)
		}
	}
}

func (pow *Full) GetHashrate() int64 {
	return int64(atomic.LoadInt32(&pow.hashRate))
}

func (pow *Full) Turbo(on bool) {
	// TODO: this needs to use an atomic operation.
	pow.turbo = on
}

// Etchash combines block verification with Light and
// nonce searching with Full into a single proof of work.
type Etchash struct {
	*Light
	*Full
}

// New creates an instance of the proof of work.
func New(ecip1099FBlock *uint64, uip1FEpoch *uint64) *Etchash {
	var light = new(Light)
	light.ecip1099FBlock = ecip1099FBlock
	light.uip1Epoch = uip1FEpoch
	return &Etchash{light, &Full{turbo: true, ecip1099FBlock: ecip1099FBlock, uip1Epoch: uip1FEpoch}}
}

// NewShared creates an instance of the proof of work., where a single instance
// of the Light cache is shared across all instances created with NewShared.
func NewShared(ecip1099FBlock *uint64, uip1FEpoch *uint64) *Etchash {
	return &Etchash{sharedLight, &Full{turbo: true, ecip1099FBlock: ecip1099FBlock, uip1Epoch: uip1FEpoch}}
}

// NewForTesting creates a proof of work for use in unit tests.
// It uses a smaller DAG and cache size to keep test times low.
// DAG files are stored in a temporary directory.
//
// Nonces found by a testing instance are not verifiable with a
// regular-size cache.
func NewForTesting(ecip1099FBlock *uint64, uip1FEpoch *uint64) (*Etchash, error) {
	dir, err := ioutil.TempDir("", "etchash-test")
	if err != nil {
		return nil, err
	}
	return &Etchash{&Light{test: true}, &Full{Dir: dir, test: true}}, nil
}
