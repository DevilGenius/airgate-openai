package gateway

import (
	"container/heap"
	"time"
)

const cacheCleanupBudget = 32

type cacheExpiry struct {
	key   string
	at    time.Time
	index int
}

type cacheExpiryQueue []*cacheExpiry

func (q cacheExpiryQueue) Len() int           { return len(q) }
func (q cacheExpiryQueue) Less(i, j int) bool { return q[i].at.Before(q[j].at) }
func (q cacheExpiryQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].index = i; q[j].index = j }
func (q *cacheExpiryQueue) Push(value any) {
	e := value.(*cacheExpiry)
	e.index = len(*q)
	*q = append(*q, e)
}
func (q *cacheExpiryQueue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*q = old[:n-1]
	return e
}
func (q *cacheExpiryQueue) add(key string, at time.Time) *cacheExpiry {
	e := &cacheExpiry{key: key, at: at}
	heap.Push(q, e)
	return e
}
func (q *cacheExpiryQueue) update(e *cacheExpiry, at time.Time) { e.at = at; heap.Fix(q, e.index) }
func (q *cacheExpiryQueue) remove(e *cacheExpiry)               { heap.Remove(q, e.index) }
