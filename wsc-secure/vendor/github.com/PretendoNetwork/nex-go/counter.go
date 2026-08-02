package nex

import "sync/atomic"

// Counter represents an incremental counter
type Counter struct {
	value uint32
}

// Value returns the counters current value
func (counter *Counter) Value() uint32 {
	return atomic.LoadUint32(&counter.value)
}

// Increment increments the counter by 1 and returns the new value
func (counter *Counter) Increment() uint32 {
	return atomic.AddUint32(&counter.value, 1)
}

// NewCounter returns a new Counter, with a starting number
func NewCounter(start uint32) *Counter {
	counter := &Counter{value: start}

	return counter
}
