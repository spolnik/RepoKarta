package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RuntimeSnapshot contains only aggregate process state.
type RuntimeSnapshot struct {
	Repositories map[string]int64
	Database     map[string]int64
}

// RegisterRuntimeSnapshot exposes bounded catalogue and database gauges.
func (system *System) RegisterRuntimeSnapshot(
	provider func(context.Context) (RuntimeSnapshot, error),
) error {
	if system == nil || system.meterProvider == nil || provider == nil {
		return nil
	}
	meter := system.meterProvider.Meter(instrumentationName)
	repositories, err := meter.Int64ObservableGauge(
		"repokarta.catalogue.repositories",
		metric.WithDescription("Repositories by bounded catalogue lifecycle state."),
		metric.WithUnit("{repository}"),
	)
	if err != nil {
		return err
	}
	database, err := meter.Int64ObservableGauge(
		"repokarta.database.connections",
		metric.WithDescription("Database connection pool state."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		snapshot, snapshotErr := provider(ctx)
		if snapshotErr != nil {
			return snapshotErr
		}
		for state, count := range snapshot.Repositories {
			observer.ObserveInt64(repositories, count, metric.WithAttributes(
				attribute.String("repokarta.state", boundedState(state)),
			))
		}
		for state, count := range snapshot.Database {
			observer.ObserveInt64(database, count, metric.WithAttributes(
				attribute.String("repokarta.state", boundedState(state)),
			))
		}
		return nil
	}, repositories, database)
	return err
}
