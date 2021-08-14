package cmap

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	mInitBit  = 4
	mInitSize = 1 << mInitBit
)

// Map is a "thread" safe map of type AnyComparableType:Any.
// To avoid lock bottlenecks this map is dived to several map shards.
type Map struct {
	mu    sync.Mutex
	count int64
	node  unsafe.Pointer
}

type node struct {
	B       uint8          // log_2 of # of buckets (can hold up to loadFactor * 2^B items)
	mask    uintptr        // 1<<B - 1
	resize  uint32         // 重新计算进程，0表示完成，1表示正在进行
	oldNode unsafe.Pointer // *node
	buckets []bucket
}

type bucket struct {
	mu     sync.RWMutex
	init   int64                       // 是否完成初始化
	frozen bool                        // true表示当前bucket已经冻结，进行resize
	m      map[interface{}]interface{} //
}

// use in range
type entry struct {
	key, value interface{}
}

func New() *Map {
	m := &Map{}
	n := m.getNode()
	n.initBuckets()
	return m
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	hash := chash(key)
	_, b := m.getNodeAndBucket(hash)
	value, ok = b.tryLoad(key)
	return
}

// Store sets the value for a key.
func (m *Map) Store(key, value interface{}) {
	hash := chash(key)
	for {
		n, b := m.getNodeAndBucket(hash)
		if b.tryStore(m, n, false, key, value) {
			return
		}
	}
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	hash := chash(key)
	for {
		n, b := m.getNodeAndBucket(hash)
		actual, loaded = b.tryLoad(key)
		if loaded {
			return
		}
		if b.tryStore(m, n, true, key, value) {
			return value, false
		}
	}
}

// Delete deletes the value for a key.
func (m *Map) Delete(key interface{}) {
	m.LoadAndDelete(key)
}

// Delete deletes the value for a key.
func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	hash := chash(key)
	for {
		n, b := m.getNodeAndBucket(hash)
		value, loaded = b.tryLoad(key)
		if !loaded {
			return
		}
		if b.tryDelete(m, n, key) {
			return
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value interface{}) bool) {
	n := m.getNode()
	for i := range n.buckets {
		b := n.getBucket(uintptr(i))
		for _, e := range b.clone() {
			if !f(e.key, e.value) {
				return
			}
		}
	}
}

// Len returns the number of elements within the map.
func (m *Map) Len() int {
	return int(atomic.LoadInt64(&m.count))
}

func (m *Map) getNodeAndBucket(hash uintptr) (n *node, b *bucket) {
	n = m.getNode()
	b = n.getBucket(hash)
	return n, b
}

func (m *Map) getNode() *node {
	n := (*node)(atomic.LoadPointer(&m.node))
	if n == nil {
		m.mu.Lock()
		n = (*node)(atomic.LoadPointer(&m.node))
		if n == nil {
			n = &node{
				mask:    uintptr(mInitSize - 1),
				B:       mInitBit,
				buckets: make([]bucket, mInitSize),
			}
			atomic.StorePointer(&m.node, unsafe.Pointer(n))
		}
		m.mu.Unlock()
	}
	return n
}

// give a hash key and return it's store bucket
func (n *node) getBucket(h uintptr) *bucket {
	return n.initBucket(h)
}

func (n *node) initBuckets() {
	for i := range n.buckets {
		n.initBucket(uintptr(i))
	}
	atomic.StorePointer(&n.oldNode, nil)
	atomic.StoreUint32(&n.resize, 0)
}

func (n *node) initBucket(i uintptr) *bucket {
	i = i & n.mask
	nb := &(n.buckets[i])
	if nb.inited() {
		return nb
	}
	nb.mu.Lock()
	defer nb.mu.Unlock()
	nb = &(n.buckets[i])
	if nb.inited() {
		return nb
	}
	nb.m = make(map[interface{}]interface{})

	p := (*node)(atomic.LoadPointer(&n.oldNode))
	if p != nil {
		if n.mask > p.mask {
			// grow
			pb := p.getBucket(i)
			for k, v := range pb.freeze() {
				h := chash(k)
				if h&n.mask == i {
					nb.m[k] = v
				}
			}
		} else {
			// shrink
			pb0 := p.getBucket(i)
			for k, v := range pb0.freeze() {
				nb.m[k] = v
			}
			pb1 := *p.getBucket(i + bucketShift(n.B))
			for k, v := range pb1.freeze() {
				nb.m[k] = v
			}
		}
	}

	// finish initialize
	atomic.StoreInt64(&nb.init, 1)
	return nb
}

func (b *bucket) inited() bool {
	return atomic.LoadInt64(&b.init) == 1
}

func (b *bucket) freeze() map[interface{}]interface{} {
	b.mu.Lock()
	b.frozen = true
	m := b.m
	b.mu.Unlock()
	return m
}

func (b *bucket) clone() []entry {
	b.mu.RLock()
	entries := make([]entry, 0, len(b.m))
	for k, v := range b.m {
		entries = append(entries, entry{key: k, value: v})
	}
	b.mu.RUnlock()
	return entries
}

func (b *bucket) tryLoad(key interface{}) (value interface{}, ok bool) {
	b.mu.RLock()
	value, ok = b.m[key]
	b.mu.RUnlock()
	return
}

func (b *bucket) tryStore(m *Map, n *node, check bool, key, value interface{}) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frozen {
		return false
	}
	if check {
		if _, ok := b.m[key]; ok {
			return false
		}
	}

	l0 := len(b.m) // Using length check existence is faster than accessing.
	b.m[key] = value
	l1 := len(b.m)
	if l0 == l1 {
		return true
	}
	// atomic.AddInt64(&m.count, 1)
	count := atomic.AddInt64(&m.count, 1)
	// TODO grow
	if overLoadFactor(count, n.B) || overflowGrow(int64(l1), n.B) {
		growWork(m, n, n.B+1)
	}
	return true
}

func (b *bucket) tryDelete(m *Map, n *node, key interface{}) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frozen {
		return false
	}

	if _, ok := b.m[key]; !ok {
		return true
	}

	delete(b.m, key)
	count := atomic.AddInt64(&m.count, -1)

	// TODO shrink
	if belowShrink(count, n.B) {
		growWork(m, n, n.B-1)
	}
	return true
}

func growWork(m *Map, n *node, B uint8) {
	if !n.growing() && atomic.CompareAndSwapUint32(&n.resize, 0, 1) {
		nn := &node{
			mask:    bucketMask(B),
			B:       B,
			resize:  1,
			oldNode: unsafe.Pointer(n),
			buckets: make([]bucket, bucketShift(B)),
		}
		ok := atomic.CompareAndSwapPointer(&m.node, unsafe.Pointer(n), unsafe.Pointer(nn))
		if !ok {
			panic("BUG: failed swapping head")
		}
		go nn.initBuckets()
	}
}

func (n *node) growing() bool {
	return atomic.LoadPointer(&n.oldNode) != nil
}

func overLoadFactor(count int64, B uint8) bool {
	if B > 15 {
		return false
	}
	return count >= int64(1<<(2*B))
}

func overflowGrow(count int64, B uint8) bool {
	if B > 15 {
		return false
	}
	return count > int64(1<<(B+1))
}

func belowShrink(count int64, B uint8) bool {
	if B-1 <= mInitBit {
		return false
	}
	return count < int64(1<<(B-1))
}

// bucketShift returns 1<<b, optimized for code generation.
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.
	return uintptr(1) << (b)
}

// bucketMask returns 1<<b - 1, optimized for code generation.
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}
