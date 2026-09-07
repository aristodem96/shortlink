// Package analytics buffers redirect events and writes them to storage in
// batches, so that recording a click never slows a redirect down.
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/observability"
)

// ClickWriter persists a batch of clicks.
type ClickWriter interface {
	InsertClicks(ctx context.Context, clicks []domain.Click) error
}

// CollectorConfig tunes the collector.
type CollectorConfig struct {
	// BufferSize is the depth of the in-memory queue. When it fills up,
	// further events are dropped rather than queued: shedding analytics is
	// always preferable to slowing down redirects.
	BufferSize int
	// Workers is the number of goroutines draining the queue.
	Workers int
	// BatchSize is the number of rows written per INSERT.
	BatchSize int
	// FlushInterval bounds how long a partial batch waits before it is written.
	FlushInterval time.Duration
	// FlushTimeout caps a single write to storage.
	FlushTimeout time.Duration
}

func (c *CollectorConfig) withDefaults() {
	if c.BufferSize <= 0 {
		c.BufferSize = 10_000
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 2 * time.Second
	}
	if c.FlushTimeout <= 0 {
		c.FlushTimeout = 5 * time.Second
	}
}

// Collector is a fan-in queue of click events drained by a pool of workers.
//
// Its contract with the redirect path is that Record never blocks and never
// fails: a redirect must not get slower, or fail, because analytics is behind.
type Collector struct {
	writer ClickWriter
	cfg    CollectorConfig
	log    *slog.Logger

	queue chan domain.Click
	// done is closed to signal shutdown. The queue itself is deliberately
	// never closed: Record can be in flight during shutdown, and a send on a
	// closed channel would panic. Workers drain whatever is buffered after
	// done fires, so nothing already accepted is lost.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewCollector creates a collector. Call Start before recording.
func NewCollector(writer ClickWriter, cfg CollectorConfig, log *slog.Logger) *Collector {
	cfg.withDefaults()
	return &Collector{
		writer: writer,
		cfg:    cfg,
		log:    log,
		queue:  make(chan domain.Click, cfg.BufferSize),
		done:   make(chan struct{}),
	}
}

// Start launches the worker pool.
func (c *Collector) Start() {
	for i := 0; i < c.cfg.Workers; i++ {
		c.wg.Add(1)
		go func(worker int) {
			defer c.wg.Done()
			c.run(worker)
		}(i)
	}
}

// Record enqueues a click. It returns immediately; if the queue is full the
// event is dropped and counted in shortlink_clicks_dropped_total.
func (c *Collector) Record(click domain.Click) {
	select {
	case c.queue <- click:
		observability.ClicksEnqueued.Inc()
	default:
		observability.ClicksDropped.Inc()
	}
}

// Close stops the workers and flushes everything already queued. It returns an
// error if ctx expires before the queue is drained.
func (c *Collector) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { close(c.done) })

	drained := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("analytics: draining click queue: %w", ctx.Err())
	}
}

func (c *Collector) run(worker int) {
	batch := make([]domain.Click, 0, c.cfg.BatchSize)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	log := c.log.With(slog.Int("worker", worker))

	for {
		select {
		case click := <-c.queue:
			batch = append(batch, click)
			if len(batch) >= c.cfg.BatchSize {
				batch = c.flush(log, batch)
			}

		case <-ticker.C:
			batch = c.flush(log, batch)

		case <-c.done:
			batch = c.drain(log, batch)
			c.flush(log, batch)
			return
		}
	}
}

// drain empties whatever is still buffered without blocking, flushing whenever
// a full batch accumulates.
func (c *Collector) drain(log *slog.Logger, batch []domain.Click) []domain.Click {
	for {
		select {
		case click := <-c.queue:
			batch = append(batch, click)
			if len(batch) >= c.cfg.BatchSize {
				batch = c.flush(log, batch)
			}
		default:
			return batch
		}
	}
}

// flush writes the batch and returns an empty slice reusing the same backing
// array. A failed write is logged and dropped: retrying would risk unbounded
// memory growth for data that is, by design, best effort.
func (c *Collector) flush(log *slog.Logger, batch []domain.Click) []domain.Click {
	if len(batch) == 0 {
		return batch
	}

	// The parent context is intentionally Background: a flush triggered during
	// shutdown still needs its own budget to finish the write.
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.FlushTimeout)
	defer cancel()

	if err := c.writer.InsertClicks(ctx, batch); err != nil {
		log.Error("could not persist click batch",
			slog.Int("clicks", len(batch)), slog.Any("error", err))
	} else {
		observability.ClicksPersisted.Add(float64(len(batch)))
		observability.ClickBatchSize.Observe(float64(len(batch)))
	}

	return batch[:0]
}
