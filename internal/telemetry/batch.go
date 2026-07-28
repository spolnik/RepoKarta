package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type flushRequest struct {
	ctx  context.Context
	done chan error
}

type traceBatchProcessor struct {
	exporter sdktrace.SpanExporter
	state    *deliveryState
	queue    chan sdktrace.ReadOnlySpan
	flush    chan flushRequest
	stop     chan flushRequest
	stopped  atomic.Bool
	batch    int
	delay    time.Duration
}

func newTraceBatchProcessor(exporter sdktrace.SpanExporter, state *deliveryState) *traceBatchProcessor {
	processor := &traceBatchProcessor{
		exporter: exporter,
		state:    state,
		queue:    make(chan sdktrace.ReadOnlySpan, state.queueCapacity()),
		flush:    make(chan flushRequest),
		stop:     make(chan flushRequest),
		batch:    envPositiveInt("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", 512),
		delay:    envMilliseconds("OTEL_BSP_SCHEDULE_DELAY", 5*time.Second),
	}
	go processor.run()
	return processor
}

func (processor *traceBatchProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (processor *traceBatchProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if processor.stopped.Load() || !span.SpanContext().IsSampled() {
		return
	}
	processor.state.queued(1)
	select {
	case processor.queue <- span:
	default:
		processor.state.queued(-1)
		processor.state.dropped(1)
	}
}

func (processor *traceBatchProcessor) ForceFlush(ctx context.Context) error {
	if processor.stopped.Load() {
		return nil
	}
	return requestFlush(ctx, processor.flush)
}

func (processor *traceBatchProcessor) Shutdown(ctx context.Context) error {
	if !processor.stopped.CompareAndSwap(false, true) {
		return nil
	}
	return requestFlush(ctx, processor.stop)
}

func (processor *traceBatchProcessor) run() {
	timer := time.NewTimer(processor.delay)
	defer timer.Stop()
	batch := make([]sdktrace.ReadOnlySpan, 0, processor.batch)
	export := func(ctx context.Context) error {
		if len(batch) == 0 {
			return nil
		}
		current := append([]sdktrace.ReadOnlySpan(nil), batch...)
		batch = batch[:0]
		return processor.exporter.ExportSpans(ctx, current)
	}
	drain := func() {
		for len(batch) < processor.batch {
			select {
			case span := <-processor.queue:
				processor.state.queued(-1)
				batch = append(batch, span)
			default:
				return
			}
		}
	}
	for {
		select {
		case span := <-processor.queue:
			processor.state.queued(-1)
			batch = append(batch, span)
			if len(batch) >= processor.batch {
				_ = export(context.Background())
			}
		case <-timer.C:
			drain()
			_ = export(context.Background())
			timer.Reset(processor.delay)
		case request := <-processor.flush:
			drain()
			request.done <- export(request.ctx)
		case request := <-processor.stop:
			drain()
			err := export(request.ctx)
			if shutdownErr := processor.exporter.Shutdown(request.ctx); err == nil {
				err = shutdownErr
			}
			request.done <- err
			return
		}
	}
}

type logBatchProcessor struct {
	exporter sdklog.Exporter
	state    *deliveryState
	queue    chan sdklog.Record
	flush    chan flushRequest
	stop     chan flushRequest
	stopped  atomic.Bool
	batch    int
	delay    time.Duration
}

func newLogBatchProcessor(exporter sdklog.Exporter, state *deliveryState) *logBatchProcessor {
	processor := &logBatchProcessor{
		exporter: exporter,
		state:    state,
		queue:    make(chan sdklog.Record, state.queueCapacity()),
		flush:    make(chan flushRequest),
		stop:     make(chan flushRequest),
		batch:    envPositiveInt("OTEL_BLRP_MAX_EXPORT_BATCH_SIZE", 512),
		delay:    envMilliseconds("OTEL_BLRP_SCHEDULE_DELAY", time.Second),
	}
	go processor.run()
	return processor
}

func (*logBatchProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (processor *logBatchProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	if processor.stopped.Load() || record == nil {
		return nil
	}
	processor.state.queued(1)
	select {
	case processor.queue <- record.Clone():
	default:
		processor.state.queued(-1)
		processor.state.dropped(1)
	}
	return nil
}

func (processor *logBatchProcessor) ForceFlush(ctx context.Context) error {
	if processor.stopped.Load() {
		return nil
	}
	return requestFlush(ctx, processor.flush)
}

func (processor *logBatchProcessor) Shutdown(ctx context.Context) error {
	if !processor.stopped.CompareAndSwap(false, true) {
		return nil
	}
	return requestFlush(ctx, processor.stop)
}

func (processor *logBatchProcessor) run() {
	timer := time.NewTimer(processor.delay)
	defer timer.Stop()
	batch := make([]sdklog.Record, 0, processor.batch)
	export := func(ctx context.Context) error {
		if len(batch) == 0 {
			return nil
		}
		current := append([]sdklog.Record(nil), batch...)
		batch = batch[:0]
		return processor.exporter.Export(ctx, current)
	}
	drain := func() {
		for len(batch) < processor.batch {
			select {
			case record := <-processor.queue:
				processor.state.queued(-1)
				batch = append(batch, record)
			default:
				return
			}
		}
	}
	for {
		select {
		case record := <-processor.queue:
			processor.state.queued(-1)
			batch = append(batch, record)
			if len(batch) >= processor.batch {
				_ = export(context.Background())
			}
		case <-timer.C:
			drain()
			_ = export(context.Background())
			timer.Reset(processor.delay)
		case request := <-processor.flush:
			drain()
			request.done <- export(request.ctx)
		case request := <-processor.stop:
			drain()
			err := export(request.ctx)
			if shutdownErr := processor.exporter.Shutdown(request.ctx); err == nil {
				err = shutdownErr
			}
			request.done <- err
			return
		}
	}
}

func requestFlush(ctx context.Context, target chan<- flushRequest) error {
	request := flushRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case target <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func envMilliseconds(name string, fallback time.Duration) time.Duration {
	value := envPositiveInt(name, 0)
	if value == 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}
