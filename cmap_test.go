// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmap_test

import (
	"math/rand"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/min1324/cmap"
)

type mapOp string

const (
	opLoad          = mapOp("Load")
	opStore         = mapOp("Store")
	opLoadOrStore   = mapOp("LoadOrStore")
	opLoadAndDelete = mapOp("LoadAndDelete")
	opDelete        = mapOp("Delete")
)

var mapOps = [...]mapOp{opLoad, opStore, opLoadOrStore, opLoadAndDelete, opDelete}

// mapCall is a quick.Generator for calls on mapInterface.
type mapCall struct {
	op   mapOp
	k, v interface{}
}

func (c mapCall) apply(m mapInterface) (interface{}, bool) {
	switch c.op {
	case opLoad:
		return m.Load(c.k)
	case opStore:
		m.Store(c.k, c.v)
		return nil, false
	case opLoadOrStore:
		return m.LoadOrStore(c.k, c.v)
	case opLoadAndDelete:
		return m.LoadAndDelete(c.k)
	case opDelete:
		m.Delete(c.k)
		return nil, false
	default:
		panic("invalid mapOp")
	}
}

type mapResult struct {
	value interface{}
	ok    bool
}

func randValue(r *rand.Rand) interface{} {
	b := make([]byte, r.Intn(4))
	for i := range b {
		b[i] = 'a' + byte(rand.Intn(26))
	}
	return string(b)
}

func (mapCall) Generate(r *rand.Rand, size int) reflect.Value {
	c := mapCall{op: mapOps[rand.Intn(len(mapOps))], k: randValue(r)}
	switch c.op {
	case opStore, opLoadOrStore:
		c.v = randValue(r)
	}
	return reflect.ValueOf(c)
}

func applyCalls(m mapInterface, calls []mapCall) (results []mapResult, final map[interface{}]interface{}) {
	for _, c := range calls {
		v, ok := c.apply(m)
		results = append(results, mapResult{v, ok})
	}

	final = make(map[interface{}]interface{})
	m.Range(func(k, v interface{}) bool {
		final[k] = v
		return true
	})

	return results, final
}

func applyMap(calls []mapCall) ([]mapResult, map[interface{}]interface{}) {
	return applyCalls(new(cmap.Map), calls)
}

func applySyncMap(calls []mapCall) ([]mapResult, map[interface{}]interface{}) {
	return applyCalls(new(sync.Map), calls)
}

func applyRWMutexMap(calls []mapCall) ([]mapResult, map[interface{}]interface{}) {
	return applyCalls(new(RWMutexMap), calls)
}

func applyDeepCopyMap(calls []mapCall) ([]mapResult, map[interface{}]interface{}) {
	return applyCalls(new(DeepCopyMap), calls)
}

func TestMapEvacute(t *testing.T) {
	var m cmap.Map
	for i := 0; i < 1<<20; i++ {
		m.Store(i, i)
	}
	v, ok := m.Load(1 << 15)
	if !ok || v != 1<<15 {
		t.Errorf("i!=15")
	}
}

func TestMapMatchesSync(t *testing.T) {
	if err := quick.CheckEqual(applyMap, applySyncMap, nil); err != nil {
		t.Error(err)
	}
}

func TestMapMatchesRWMutex(t *testing.T) {
	if err := quick.CheckEqual(applyMap, applyRWMutexMap, nil); err != nil {
		t.Error(err)
	}
}

func TestMapMatchesDeepCopy(t *testing.T) {
	if err := quick.CheckEqual(applyMap, applyDeepCopyMap, nil); err != nil {
		t.Error(err)
	}
}

func TestConcurrentRange(t *testing.T) {
	const mapSize = 1 << 10

	m := new(sync.Map)
	for n := int64(1); n <= mapSize; n++ {
		m.Store(n, int64(n))
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(done)
		wg.Wait()
	}()
	for g := int64(runtime.GOMAXPROCS(0)); g > 0; g-- {
		r := rand.New(rand.NewSource(g))
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			for i := int64(0); ; i++ {
				select {
				case <-done:
					return
				default:
				}
				for n := int64(1); n < mapSize; n++ {
					if r.Int63n(mapSize) == 0 {
						m.Store(n, n*i*g)
					} else {
						m.Load(n)
					}
				}
			}
		}(g)
	}

	iters := 1 << 10
	if testing.Short() {
		iters = 16
	}
	for n := iters; n > 0; n-- {
		seen := make(map[int64]bool, mapSize)

		m.Range(func(ki, vi interface{}) bool {
			k, v := ki.(int64), vi.(int64)
			if v%k != 0 {
				t.Fatalf("while Storing multiples of %v, Range saw value %v", k, v)
			}
			if seen[k] {
				t.Fatalf("Range visited key %v twice", k)
			}
			seen[k] = true
			return true
		})

		if len(seen) != mapSize {
			t.Fatalf("Range visited %v elements of %v-element Map", len(seen), mapSize)
		}
	}
}

func TestIssue40999(t *testing.T) {
	var m cmap.Map

	// Since the miss-counting in missLocked (via Delete)
	// compares the miss count with len(m.dirty),
	// add an initial entry to bias len(m.dirty) above the miss count.
	m.Store(nil, struct{}{})

	var finalized uint32

	// Set finalizers that count for collected keys. A non-zero count
	// indicates that keys have not been leaked.
	for atomic.LoadUint32(&finalized) == 0 {
		p := new(int)
		runtime.SetFinalizer(p, func(*int) {
			atomic.AddUint32(&finalized, 1)
		})
		m.Store(p, struct{}{})
		m.Delete(p)
		runtime.GC()
	}
}

func TestMapStoreAndLoad(t *testing.T) {
	const mapSize = 1 << 14

	var (
		m    cmap.Map
		wg   sync.WaitGroup
		seen = make(map[int64]bool, mapSize)
	)

	for n := int64(1); n <= mapSize; n++ {
		nn := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Store(nn, nn)
		}()
	}

	wg.Wait()

	m.Range(func(ki, vi interface{}) bool {
		k, v := ki.(int64), vi.(int64)
		if v%k != 0 {
			t.Fatalf("while Storing multiples of %v, Range saw value %v", k, v)
		}
		if seen[k] {
			t.Fatalf("Range visited key %v twice", k)
		}
		seen[k] = true
		return true
	})

	if len(seen) != mapSize {
		t.Fatalf("Range visited %v elements of %v-element Map", len(seen), mapSize)
	}

	for n := int64(1); n <= mapSize; n++ {
		nn := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Delete(nn)
		}()
	}

	wg.Wait()

	m.Range(func(key, value interface{}) bool {
		t.Fatalf("Map should be empty")
		return false
	})
}
