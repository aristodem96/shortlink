package analytics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shortlink/internal/domain"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingWriter captures every batch it is handed.
type recordingWriter struct {
	mu      sync.Mutex
	batches [][]domain.Click

	// block, when non-nil, holds each write until it is closed.
	block chan struct{}
	// err, when set, is returned by every write.
	err error
}

func (w *recordingWriter) InsertClicks(_ context.Context, clicks []domain.Click) error {
	if w.block != nil {
		<-w.block
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return w.err
	}
	// The collector reuses its slice between flushes, so the batch must be
	// copied to be inspected later.
	w.batches = append(w.batches, append([]domain.Click(nil), clicks...))
	return nil
}

func (w *recordingWriter) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0
	for _, b := range w.batches {
		n += len(b)
	}
	return n
}

func (w *recordingWriter) batchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.batches)
}

func TestCollector_FlushesAFullBatchImmediately(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{}
	c := NewCollector(writer, CollectorConfig{
		BufferSize: 100,
		Workers:    1,
		BatchSize:  10,
		// Long enough that only a full batch can trigger the flush under test.
		FlushInterval: time.Hour,
	}, discardLogger())
	c.Start()
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	for i := 0; i < 10; i++ {
		c.Record(domain.Click{LinkID: int64(i), ClickedAt: time.Now()})
	}

	require.Eventually(t, func() bool { return writer.total() == 10 }, 2*time.Second, 10*time.Millisecond,
		"a full batch should be written without waiting for the flush interval")
	assert.Equal(t, 1, writer.batchCount())
}

func TestCollector_FlushesAPartialBatchOnTheInterval(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    100,
		Workers:       1,
		BatchSize:     1000, // Never reached by the three clicks below.
		FlushInterval: 50 * time.Millisecond,
	}, discardLogger())
	c.Start()
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	for i := 0; i < 3; i++ {
		c.Record(domain.Click{LinkID: int64(i), ClickedAt: time.Now()})
	}

	require.Eventually(t, func() bool { return writer.total() == 3 }, 2*time.Second, 10*time.Millisecond,
		"a partial batch should still be written once the interval elapses")
}

// TestCollector_CloseDrainsEverythingQueued is the guarantee that makes a
// rolling deploy safe: clicks already accepted are written before the process
// exits.
func TestCollector_CloseDrainsEverythingQueued(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    1000,
		Workers:       2,
		BatchSize:     1000,
		FlushInterval: time.Hour, // Only Close can trigger a flush here.
	}, discardLogger())
	c.Start()

	const n = 500
	for i := 0; i < n; i++ {
		c.Record(domain.Click{LinkID: int64(i), ClickedAt: time.Now()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Close(ctx))

	assert.Equal(t, n, writer.total(), "every queued click must be persisted before shutdown completes")
}

// TestCollector_DropsWhenTheBufferIsFull encodes the load-shedding contract:
// analytics is sacrificed so that redirects stay fast.
func TestCollector_DropsWhenTheBufferIsFull(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{block: make(chan struct{})}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    8,
		Workers:       1,
		BatchSize:     4,
		FlushInterval: time.Hour,
	}, discardLogger())
	c.Start()

	// Far more than the buffer holds, while the writer is wedged.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10_000; i++ {
			c.Record(domain.Click{LinkID: int64(i), ClickedAt: time.Now()})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked; the redirect path must never wait on analytics")
	}

	close(writer.block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// TestCollector_RecordDuringShutdownDoesNotPanic guards the reason the queue
// channel is never closed: a redirect in flight can call Record after Close has
// started, and a send on a closed channel would take the process down.
func TestCollector_RecordDuringShutdownDoesNotPanic(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    100,
		Workers:       2,
		BatchSize:     10,
		FlushInterval: 10 * time.Millisecond,
	}, discardLogger())
	c.Start()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c.Record(domain.Click{LinkID: 1, ClickedAt: time.Now()})
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.NotPanics(t, func() { _ = c.Close(ctx) })

	close(stop)
	wg.Wait()
}

func TestCollector_SurvivesAFailingWriter(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{err: errors.New("database is down")}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    100,
		Workers:       1,
		BatchSize:     5,
		FlushInterval: 10 * time.Millisecond,
	}, discardLogger())
	c.Start()

	for i := 0; i < 20; i++ {
		c.Record(domain.Click{LinkID: int64(i), ClickedAt: time.Now()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A failing writer must not wedge the workers or fail the shutdown; the
	// clicks are simply lost, which is the accepted trade for this data.
	assert.NoError(t, c.Close(ctx))
}

func TestCollector_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	c := NewCollector(&recordingWriter{}, CollectorConfig{}, discardLogger())
	c.Start()

	ctx := context.Background()
	require.NoError(t, c.Close(ctx))
	assert.NotPanics(t, func() { _ = c.Close(ctx) }, "closing twice must not close the done channel twice")
}

func TestCollector_CloseReportsATimeout(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{block: make(chan struct{})}
	c := NewCollector(writer, CollectorConfig{
		BufferSize:    10,
		Workers:       1,
		BatchSize:     1,
		FlushInterval: time.Millisecond,
	}, discardLogger())
	c.Start()
	c.Record(domain.Click{LinkID: 1, ClickedAt: time.Now()})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := c.Close(ctx)
	require.Error(t, err, "a stuck writer must surface as a shutdown error, not a silent hang")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	close(writer.block)
}
