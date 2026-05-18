// Package metrics provides Prometheus counters for config and secret reload observability.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ConfigReloadTotal counts config reload attempts by file basename and result.
	ConfigReloadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "config_reload_total",
			Help: "Total number of config reload attempts.",
		},
		[]string{"file", "result"},
	)

	// SecretReloadTotal counts secret reload attempts by scheme and result.
	SecretReloadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "secret_reload_total",
			Help: "Total number of secret reload attempts.",
		},
		[]string{"ref", "result"},
	)
)
