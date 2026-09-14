// parallel.go — input-level parallel processing for jqlm.
//
// gojq's compiled *Code is documented as safe for concurrent Run calls, so
// when --llm-concurrency N (N > 1) is given, input values are dispatched to a
// pool of N goroutines, each running the query independently (each llm_*
// builtin call becomes an independent HTTP request). Results are printed in
// input order, so the output is byte-identical to sequential processing —
// just faster when the query makes slow calls (llm_select et al).

package cli

import (
	"fmt"

	"sync"

	"github.com/itchyny/gojq"
)

type parallelItem struct {
	seq int
	val any
}

type parallelResult struct {
	seq  int
	vals []any
	err  error // first error from the query for this input (incl. halt)
}

func (cli *cli) processParallel(iter inputIter, code *gojq.Code) error {
	n := cli.llmConcurrency

	quit := make(chan struct{})
	items := make(chan parallelItem)
	results := make(chan parallelResult)

	// producer: read inputs sequentially, dispatch to the pool
	go func() {
		defer close(items)
		for seq := 0; ; seq++ {
			v, ok := iter.Next()
			if !ok {
				return
			}
			select {
			case items <- parallelItem{seq, v}:
			case <-quit:
				return
			}
		}
	}()

	// workers: run the compiled query on one input each
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range items {
				var vals []any
				var rerr error
				runIter := code.Run(it.val, cli.argvalues...)
				for {
					v, ok := runIter.Next()
					if !ok {
						break
					}
					if e, ok := v.(error); ok {
						rerr = e
						break
					}
					vals = append(vals, v)
				}
				select {
				case results <- parallelResult{seq: it.seq, vals: vals, err: rerr}:
				case <-quit:
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	stop := func() {
		select {
		case <-quit:
		default:
			close(quit)
		}
	}
	defer stop()

	m := cli.createMarshaler()
	next := 0
	pending := make(map[int]parallelResult)
	var procErr error
	halted := false

	for r := range results {
		pending[r.seq] = r
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++

			if p.err != nil {
				if e, ok := p.err.(*gojq.HaltError); ok {
					if v := e.Value(); v != nil {
						if str, ok := v.(string); ok {
							cli.errStream.Write([]byte(str))
						} else {
							bs, _ := gojq.Marshal(v)
							cli.errStream.Write(bs)
							cli.errStream.Write([]byte{'\n'})
						}
					}
					procErr = p.err
					halted = true
					stop()
					break
				}
				fmt.Fprintf(cli.errStream, "%s: %s\n", name, p.err)
				procErr = p.err
				continue
			}
			for _, v := range p.vals {
				if e := cli.printValue(m, v); e != nil {
					fmt.Fprintf(cli.errStream, "%s: %s\n", name, e)
					procErr = e
				}
			}
		}
		if halted {
			// keep draining results so in-flight workers can finish,
			// but print nothing further
			_ = r
		}
	}
	if procErr != nil {
		return &emptyError{procErr}
	}
	return nil
}
