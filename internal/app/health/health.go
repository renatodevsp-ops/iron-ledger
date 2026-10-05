// Package health reports process liveness and dependency readiness.
//
// Liveness answers one question: is this process still able to do work? It must
// never depend on an external system, because a database outage should not get
// the process restarted into the same outage. Readiness answers the other
// question: can this instance serve traffic right now? That does depend on
// PostgreSQL and SQS, so an instance without them leaves the load balancer
// instead of failing requests.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ironledger/iron-ledger/internal/platform/httpx"
	"github.com/ironledger/iron-ledger/internal/platform/logging"
	"github.com/ironledger/iron-ledger/internal/platform/metrics"
)

// Probe is a named dependency check.
type Probe struct {
	Name  string
	Check func(ctx context.Context) error
}

// Report is the readiness body.
type Report struct {
	Status   string            `json:"status"`
	Instance string            `json:"instance"`
	Uptime   string            `json:"uptime"`
	Checks   map[string]string `json:"checks"`
}

// Service renders the health endpoints.
type Service struct {
	instance  string
	startedAt time.Time
	probes    []Probe
	logger    *slog.Logger
	metrics   *metrics.Metrics

	mu sync.RWMutex
}

// New builds the health service.
func New(instance string, logger *slog.Logger, m *metrics.Metrics, probes ...Probe) *Service {
	return &Service{
		instance:  instance,
		startedAt: time.Now().UTC(),
		probes:    probes,
		logger:    logger,
		metrics:   m,
	}
}

// Live reports that the process is running.
//
// It answers from memory alone. A failing dependency must not make the process
// look dead: restarting it would not fix the dependency and would only add
// churn.
func (s *Service) Live(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, Report{
		Status:   "ok",
		Instance: s.instance,
		Uptime:   time.Since(s.startedAt).Round(time.Second).String(),
		Checks:   map[string]string{"process": "ok"},
	})
}

// Ready reports whether this instance can serve traffic right now.
func (s *Service) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	report := Report{
		Status:   "ok",
		Instance: s.instance,
		Uptime:   time.Since(s.startedAt).Round(time.Second).String(),
		Checks:   make(map[string]string, len(s.probes)),
	}

	ready := true
	for _, probe := range s.probes {
		if err := probe.Check(ctx); err != nil {
			report.Checks[probe.Name] = "unavailable: " + err.Error()
			ready = false
			if s.metrics != nil {
				s.metrics.DependencyUp.WithLabelValues(probe.Name).Set(0)
			}
			if s.logger != nil {
				s.logger.Log(ctx, slog.LevelWarn, "dependency unavailable",
					logging.FieldComponent, "health",
					"dependency", probe.Name,
					"error", err.Error(),
				)
			}
			continue
		}
		report.Checks[probe.Name] = "ok"
		if s.metrics != nil {
			s.metrics.DependencyUp.WithLabelValues(probe.Name).Set(1)
		}
	}

	if !ready {
		report.Status = "unavailable"
		httpx.WriteJSON(w, http.StatusServiceUnavailable, report)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, report)
}

// Handler mounts both endpoints.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.Live)
	mux.HandleFunc("/health/ready", s.Ready)
	return mux
}
