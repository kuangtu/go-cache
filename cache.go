package cache

// go-cache is an in-memory key:value store/cache similar to memcached that is
// suitable for applications running on a single machine. Any object can be stored,
// for a given duration or forever, and the cache can be used safely by multiple
// goroutines.
//
// == Installation
//     goinstall github.com/pmylund/go-cache
//
// == Usage
//     import "github.com/pmylund/go-cache"
//
//     // Create a cache with a default expiration time of 5 minutes, and which
//     // purges expired items every 30 seconds
//     c := cache.New(5*time.Minute, 30*time.Second)
//
//     // Set the value of the key "foo" to "bar", with the default expiration time
//     c.Set("foo", "bar", 0)
//
//     // Set the value of the key "baz" to "yes", with no expiration time
//     // (the item won't be removed until it is re-set, or removed using
//     // c.Delete("baz")
//     c.Set("baz", "yes", -1)
//
//     // Get the string associated with the key "foo" from the cache
//     foo, found := c.Get("foo")
//     if found {
//             fmt.Println(foo)
//     }
//
//     // Since Go is statically typed, and cache values can be anything, type
//     // assertion is needed when values are being passed to functions that don't
//     // take arbitrary types, (i.e. interface{}). The simplest way to do this for
//     // values which will only be used once--e.g. for passing to another
//     // function--is:
//     foo, found := c.Get("foo")
//     if found {
//             MyFunction(foo.(string))
//     }
//
//     // This gets tedious if the value is used several times in the same function.
//     // You might do either of the following instead:
//     if x, found := c.Get("foo"); found {
//             foo := x.(string)
//             ...
//     }
//     // or
//     var foo string
//     if x, found := c.Get("foo"); found {
//             foo = x.(string)
//     }
//     ...
//     // foo can then be passed around freely as a string
//
//     // Want performance? Store pointers!
//     c.Set("foo", &MyStruct, 0)
//     if x, found := c.Get("foo"); found {
//             foo := x.(*MyStruct)
//             ...
//     }
//
//     // If you store a reference type like a pointer, slice, map or channel, you
//     // do not need to run Set if you modify the underlying data. The cached
//     // reference points to the same memory, so if you modify a struct whose
//     // pointer you've stored in the cache, retrieving that pointer with Get will
//     // point you to the same data:
//     foo := &MyStruct{Num: 1}
//     c.Set("foo", foo, 0)
//     ...
//     x, _ := c.Get("foo")
//     foo := x.(*MyStruct)
//     fmt.Println(foo.Num)
//     ...
//     foo.Num++
//     ...
//     x, _ := c.Get("foo")
//     foo := x.(*MyStruct)
//     foo.Println(foo.Num)
//
//     // will print:
//     1
//     2

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"
)

type Item struct {
	Object     interface{}
	Expiration *time.Time
}

// Returns true if the item has expired.
func (i *Item) Expired() bool {
	if i.Expiration == nil {
		return false
	}
	return i.Expiration.Before(time.Now())
}

type Cache struct {
	*cache
	// If this is confusing, see the comment at the bottom of the New() function
}

type cache struct {
	DefaultExpiration time.Duration
	Items             map[string]*Item
	mu                *sync.Mutex
	janitor           *janitor
}

// Adds an item to the cache, replacing any existing item. If the duration is 0, the
// cache's default expiration time is used. If it is -1, the item never expires.
func (c *cache) Set(k string, x interface{}, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.set(k, x, d)
}

func (c *cache) set(k string, x interface{}, d time.Duration) {
	var e *time.Time
	if d == 0 {
		d = c.DefaultExpiration
	}
	if d > 0 {
		t := time.Now().Add(d)
		e = &t
	}
	c.Items[k] = &Item{
		Object:     x,
		Expiration: e,
	}
}

// Adds an item to the cache only if an item doesn't already exist for the given key,
// or if the existing item has expired. Returns an error if not.
func (c *cache) Add(k string, x interface{}, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, found := c.get(k)
	if found {
		return fmt.Errorf("Item %s already exists", k)
	}
	c.set(k, x, d)
	return nil
}

// Sets a new value for the cache item only if it already exists. Returns an error if
// it does not.
func (c *cache) Replace(k string, x interface{}, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, found := c.get(k)
	if !found {
		return fmt.Errorf("Item %s doesn't exist", k)
	}
	c.set(k, x, d)
	return nil
}

// Gets an item from the cache. Returns the item or nil, and a bool indicating whether
// the given key was found in the cache.
func (c *cache) Get(k string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.get(k)
}

func (c *cache) get(k string) (interface{}, bool) {
	item, found := c.Items[k]
	if !found {
		return nil, false
	}
	if item.Expired() {
		c.delete(k)
		return nil, false
	}
	return item.Object, true
}

// Increment an item of type int, int8, int16, int32, int64, uintptr, uint, uint8,
// uint32, uint64, float32 or float64 by n. Returns an error if the item's value is
// not an integer, if it was not found, or if it is not possible to increment it by
// n. Passing a negative number will cause the item to be decremented.
func (c *cache) IncrementFloat(k string, n float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, found := c.Items[k]
	if !found || v.Expired() {
		return fmt.Errorf("Item not found")
	}

	t := reflect.TypeOf(v.Object)
	switch t.Kind() {
	default:
		return fmt.Errorf("The value of %s is not an integer", k)
	case reflect.Uint:
		v.Object = v.Object.(uint) + uint(n)
	case reflect.Uintptr:
		v.Object = v.Object.(uintptr) + uintptr(n)
	case reflect.Uint8:
		v.Object = v.Object.(uint8) + uint8(n)
	case reflect.Uint16:
		v.Object = v.Object.(uint16) + uint16(n)
	case reflect.Uint32:
		v.Object = v.Object.(uint32) + uint32(n)
	case reflect.Uint64:
		v.Object = v.Object.(uint64) + uint64(n)
	case reflect.Int:
		v.Object = v.Object.(int) + int(n)
	case reflect.Int8:
		v.Object = v.Object.(int8) + int8(n)
	case reflect.Int16:
		v.Object = v.Object.(int16) + int16(n)
	case reflect.Int32:
		v.Object = v.Object.(int32) + int32(n)
	case reflect.Int64:
		v.Object = v.Object.(int64) + int64(n)
	case reflect.Float32:
		v.Object = v.Object.(float32) + float32(n)
	case reflect.Float64:
		v.Object = v.Object.(float64) + n
	}
	return nil
}

// Increment an item of type int, int8, int16, int32, int64, uintptr, uint, uint8,
// uint32, or uint64, float32 or float64 by n. Returns an error if the item's value
// is not an integer, if it was not found, or if it is not possible to increment it
// by n. Passing a negative number will cause the item to be decremented.
func (c *cache) Increment(k string, n int64) error {
	return c.IncrementFloat(k, float64(n))
}

// Decrement an item of type int, int8, int16, int32, int64, uintptr, uint, uint8,
// uint32, or uint64, float32 or float64 by n. Returns an error if the item's value
// is not an integer, if it was not found, or if it is not possible to decrement it
// by n.
func (c *cache) Decrement(k string, n int64) error {
	return c.Increment(k, n*-1)
}

// Deletes an item from the cache. Does nothing if the item does not exist in the cache.
func (c *cache) Delete(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.delete(k)
}

func (c *cache) delete(k string) {
	delete(c.Items, k)
}

// Deletes all expired items from the cache.
func (c *cache) DeleteExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range c.Items {
		if v.Expired() {
			c.delete(k)
		}
	}
}

// Writes the cache's items (using Gob) to an io.Writer.
func (c *cache) Save(w io.Writer) error {
	enc := gob.NewEncoder(w)

	defer func() {
		if x := recover(); x != nil {
			fmt.Printf(`The Gob library paniced while registering the cache's item types!
Information: %v

The cache will not be saved.
Please report under what conditions this happened, and particularly what special type of objects
were stored in cache, at https://github.com/pmylund/go-cache/issues/new`, x)
		}
	}()
	for _, v := range c.Items {
		gob.Register(v.Object)
	}
	err := enc.Encode(&c.Items)
	return err
}

// Saves the cache's items to the given filename, creating the file if it
// doesn't exist, and overwriting it if it does.
func (c *cache) SaveFile(fname string) error {
	fp, err := os.Create(fname)
	if err != nil {
		return err
	}
	return c.Save(fp)
}

// Adds (Gob-serialized) cache items from an io.Reader, excluding any items that
// already exist in the current cache.
func (c *cache) Load(r io.Reader) error {
	dec := gob.NewDecoder(r)
	items := map[string]*Item{}
	err := dec.Decode(&items)
	if err == nil {
		for k, v := range items {
			_, found := c.Items[k]
			if !found {
				c.Items[k] = v
			}
		}
	}
	return err
}

// Loads and adds cache items from the given filename, excluding any items that
// already exist in the current cache.
func (c *cache) LoadFile(fname string) error {
	fp, err := os.Open(fname)
	if err != nil {
		return err
	}
	return c.Load(fp)
}

// Deletes all items from the cache.
func (c *cache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Items = map[string]*Item{}
}

type janitor struct {
	Interval time.Duration
	stop     chan bool
}

func (j *janitor) Run(c *cache) {
	j.stop = make(chan bool)
	tick := time.Tick(j.Interval)
	for {
		select {
		case <-tick:
			c.DeleteExpired()
		case <-j.stop:
			return
		}
	}
}

func (j *janitor) Stop() {
	j.stop <- true
}

func stopJanitor(c *Cache) {
	c.janitor.Stop()
}

// Returns a new cache with a given default expiration duration and default cleanup
// interval. If the expiration duration is less than 1, the items in the cache never
// expire and must be deleted manually. If the cleanup interval is less than one,
// expired items are not deleted from the cache before their next lookup or before
// calling DeleteExpired.
func New(de, ci time.Duration) *Cache {
	if de == 0 {
		de = -1
	}
	c := &cache{
		DefaultExpiration: de,
		Items:             map[string]*Item{},
		mu:                &sync.Mutex{},
	}
	if ci > 0 {
		j := &janitor{
			Interval: ci,
		}
		c.janitor = j
		go j.Run(c)
	}
	// This trick ensures that the janitor goroutine (which--granted it was enabled--is
	// running DeleteExpired on c forever) does not keep the returned C object from being
	// garbage collected. When it is garbage collected, the finalizer stops the janitor
	// goroutine, after which c is collected.
	C := &Cache{c}
	if ci > 0 {
		runtime.SetFinalizer(C, stopJanitor)
	}
	return C
}