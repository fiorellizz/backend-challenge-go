// Package metrics defines the Prometheus instruments the service exposes
// on /metrics. They are incremented at the edges (HTTP handlers, the SQS
// consumer, the workers), so the use cases stay free of instrumentation.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every instrument. Label values are bounded (enumerations
// only) so cardinality stays flat: no ids ever become labels.
type Metrics struct {
	registry *prometheus.Registry

	// Operations: outcome by entry point, kind and status; failure_code is
	// empty unless the status is REJECTED or FAILED.
	TransactionsTotal *prometheus.CounterVec
	// Replays served from persisted outcomes (idempotentReplay = true).
	DuplicatesTotal *prometheus.CounterVec
	// Idempotency conflicts: "payload" (same key, other body), "key"
	// (same operation, other key), "version" (lost-update guard).
	ConflictsTotal *prometheus.CounterVec
	// Time to process one operation, by entry point.
	ProcessingSeconds *prometheus.HistogramVec

	// Pending-reference worker: resolutions by outcome (processed,
	// rejected, retried).
	ReferenceResolutionsTotal *prometheus.CounterVec

	// Outbox publisher.
	OutboxPublishedTotal prometheus.Counter
	OutboxFailedTotal    prometheus.Counter
	OutboxLagSeconds     prometheus.Gauge

	// SQS consumer: decisions (ack, reject, retry) and messages sent to
	// the dead-letter queue by this consumer.
	ConsumerMessagesTotal *prometheus.CounterVec
	DLQTotal              prometheus.Counter

	// Reconciliation divergences found.
	ReconciliationDivergencesTotal prometheus.Counter

	// HTTP server.
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
}

// New builds and registers every instrument on a private registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	f := promauto{reg}
	m := &Metrics{
		registry: reg,
		TransactionsTotal: f.counterVec("wager_transactions_total",
			"Operations by entry point, kind, final status and failure code.",
			[]string{"source", "kind", "status", "failure_code"}),
		DuplicatesTotal: f.counterVec("wager_duplicates_total",
			"Operations answered from a persisted outcome (idempotent replay).", []string{"source"}),
		ConflictsTotal: f.counterVec("wager_conflicts_total",
			"Idempotency and concurrency conflicts by type.", []string{"type"}),
		ProcessingSeconds: f.histogramVec("wager_processing_seconds",
			"Time to process one operation end to end.", []string{"source"},
			[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}),
		ReferenceResolutionsTotal: f.counterVec("wager_reference_resolutions_total",
			"Pending-reference worker outcomes.", []string{"outcome"}),
		OutboxPublishedTotal: f.counter("outbox_published_total", "Events published from the outbox."),
		OutboxFailedTotal:    f.counter("outbox_publish_failures_total", "Publish attempts that failed and were rescheduled."),
		OutboxLagSeconds:     f.gauge("outbox_lag_seconds", "Age of the oldest unpublished event, in seconds."),
		ConsumerMessagesTotal: f.counterVec("sqs_consumer_messages_total",
			"Messages handled by the consumer, by decision.", []string{"decision"}),
		DLQTotal: f.counter("sqs_consumer_dlq_total", "Messages the consumer moved to the dead-letter queue."),
		ReconciliationDivergencesTotal: f.counter("reconciliation_divergences_total",
			"Reconciliations whose stored balance differed from the ledger."),
		HTTPRequestsTotal: f.counterVec("http_requests_total",
			"HTTP requests by method, route pattern and status.", []string{"method", "route", "status"}),
		HTTPRequestDuration: f.histogramVec("http_request_duration_seconds",
			"HTTP request latency by method and route pattern.", []string{"method", "route"},
			[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}),
	}
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveProcessing records one operation outcome and its latency.
func (m *Metrics) ObserveProcessing(source, kind, status, failureCode string, replay bool, elapsed time.Duration) {
	m.TransactionsTotal.WithLabelValues(source, kind, status, failureCode).Inc()
	if replay {
		m.DuplicatesTotal.WithLabelValues(source).Inc()
	}
	m.ProcessingSeconds.WithLabelValues(source).Observe(elapsed.Seconds())
}

type promauto struct{ reg *prometheus.Registry }

func (p promauto) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	p.reg.MustRegister(c)
	return c
}

func (p promauto) counterVec(name, help string, labels []string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	p.reg.MustRegister(c)
	return c
}

func (p promauto) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	p.reg.MustRegister(g)
	return g
}

func (p promauto) histogramVec(name, help string, labels []string, buckets []float64) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	p.reg.MustRegister(h)
	return h
}
