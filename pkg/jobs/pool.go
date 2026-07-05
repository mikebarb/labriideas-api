// Package jobs provides a bounded worker pool for processing items concurrently.
package jobs

import (
	"sync"
)

// RunParallel processes items with up to maxConcurrency workers.
// onItem is called for each item; onSuccess and onFailure are called from the worker goroutines.
// onSuccess and onFailure are called sequentially per worker but concurrently across workers,
// so they should be thread‑safe (or use a mutex). The function returns only after all items
// have been processed.
func RunParallel[T any](
	items []T,
	maxConcurrency int,
	onItem func(T) error,
	onSuccess func(T),
	onFailure func(T, error),
) {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{} // acquire – blocks if full

		go func(item T) {
			defer wg.Done()
			defer func() { <-sem }() // release

			if err := onItem(item); err != nil {
				onFailure(item, err)
			} else {
				onSuccess(item)
			}
		}(item)
	}

	wg.Wait()
}
