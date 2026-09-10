// Package metrics defines the Prometheus metrics for the MCP server and the
// adapters that feed client-go's request metrics into the default registry.
// Metrics are served on a separate, unauthenticated port (see main); labels are
// deliberately low-cardinality (never namespace/application/user).
package metrics

import (
	"context"
	"net/url"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

var (
	// Label is mcp_instance (not instance) to avoid colliding with Prometheus'
	// own target label, which would otherwise overwrite it.
	toolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "amcp_tool_calls_total",
		Help: "MCP tool calls by tool, target Argo CD instance and result (ok|error|forbidden|blocked|conflict).",
	}, []string{"tool", "mcp_instance", "result"})

	toolDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "amcp_tool_call_duration_seconds",
		Help:    "MCP tool call latency by tool and Argo CD instance.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool", "mcp_instance"})

	authRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "amcp_auth_requests_total",
		Help: "Agent authentication attempts by method (static|oidc|none) and result (allow|deny).",
	}, []string{"method", "result"})

	instanceUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "amcp_instance_up",
		Help: "Argo CD instance reachability (1 = Application API reachable, 0 = not), by instance.",
	}, []string{"mcp_instance"})

	writesBlocked = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "amcp_writes_blocked_total",
		Help: "Mutating tool calls blocked by the read-only guard, by instance and reason.",
	}, []string{"mcp_instance", "reason"})

	operations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "amcp_operations_total",
		Help: "Argo CD operations requested through the MCP by instance, operation (sync|refresh|rollback|terminate) and result.",
	}, []string{"mcp_instance", "op", "result"})

	writeGuardActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "amcp_write_guard_active",
		Help: "1 = this instance's credentials cannot rewrite an Application's source (blocked by admission policy or RBAC); 0 = a spec rewrite was accepted in dry-run, i.e. the write guard is MISSING. Absent = not probed.",
	}, []string{"mcp_instance"})

	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "amcp_build_info",
		Help: "Build information; constant 1.",
	}, []string{"version", "goversion"})

	restRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rest_client_requests_total",
		Help: "Kubernetes API requests by host, status code and method (client-go).",
	}, []string{"host", "code", "method"})

	restLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_request_duration_seconds",
		Help:    "Kubernetes API request latency by host and verb (client-go).",
		Buckets: prometheus.DefBuckets,
	}, []string{"host", "verb"})
)

// RecordTool records a tool call outcome and latency.
func RecordTool(tool, instance, result string, d time.Duration) {
	if instance == "" {
		instance = "-"
	}
	toolCalls.WithLabelValues(tool, instance, result).Inc()
	toolDuration.WithLabelValues(tool, instance).Observe(d.Seconds())
}

// RecordAuth records an agent-authentication outcome.
func RecordAuth(method, result string) { authRequests.WithLabelValues(method, result).Inc() }

// RecordWriteBlocked records a mutation blocked by the read-only guard.
func RecordWriteBlocked(instance, reason string) {
	writesBlocked.WithLabelValues(instance, reason).Inc()
}

// RecordOperation records an Argo CD operation requested through the MCP.
func RecordOperation(instance, op, result string) {
	if instance == "" {
		instance = "-"
	}
	operations.WithLabelValues(instance, op, result).Inc()
}

// SetInstanceUp sets the reachability gauge for an instance.
func SetInstanceUp(instance string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	instanceUp.WithLabelValues(instance).Set(v)
}

// SetWriteGuardActive publishes whether an instance's write guard is in force.
func SetWriteGuardActive(instance string, active bool) {
	v := 0.0
	if active {
		v = 1
	}
	writeGuardActive.WithLabelValues(instance).Set(v)
}

// ClearWriteGuard removes the gauge for an instance whose guard could not be
// probed, so an unknown state is never reported as safe.
func ClearWriteGuard(instance string) {
	writeGuardActive.DeleteLabelValues(instance)
}

// SetBuildInfo publishes the build-info gauge.
func SetBuildInfo(version string) {
	buildInfo.WithLabelValues(version, runtime.Version()).Set(1)
}

// --- client-go request metrics adapters ---

type resultAdapter struct{}

func (resultAdapter) Increment(_ context.Context, code, method, host string) {
	restRequests.WithLabelValues(host, code, method).Inc()
}

type latencyAdapter struct{}

func (latencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	restLatency.WithLabelValues(u.Host, verb).Observe(latency.Seconds())
}

// RegisterClientGo plugs the adapters into client-go. Call once at startup.
func RegisterClientGo() {
	clientmetrics.Register(clientmetrics.RegisterOpts{
		RequestResult:  resultAdapter{},
		RequestLatency: latencyAdapter{},
	})
}
