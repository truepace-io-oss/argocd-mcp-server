# Metrics

Metrics are served on a **separate, unauthenticated port** (`metricsAddr`,
default `:9091`, path `/metrics`). Never route it through the public ingress or
the MCP auth middleware. Set `metricsAddr: "off"` to disable it.

Labels are deliberately low-cardinality — never an application name, namespace
or user.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `amcp_tool_calls_total` | counter | `tool`, `mcp_instance`, `result` | tool calls; result is `ok`, `error`, `forbidden`, `blocked` (read-only guard) or `conflict` (an operation was already running) |
| `amcp_tool_call_duration_seconds` | histogram | `tool`, `mcp_instance` | tool latency |
| `amcp_auth_requests_total` | counter | `method` (`static`/`oidc`/`none`), `result` (`allow`/`deny`) | agent authentication attempts |
| `amcp_instance_up` | gauge | `mcp_instance` | 1 when the Application API of that instance is reachable; refreshed every 30 s independently of tool traffic |
| `amcp_writes_blocked_total` | counter | `mcp_instance`, `reason` (`global_readonly`/`instance_readonly`) | mutations stopped by the kill-switch |
| `amcp_operations_total` | counter | `mcp_instance`, `op` (`sync`/`refresh`/`rollback`/`terminate`), `result` | Argo CD operations requested through the MCP |
| `amcp_write_guard_active` | gauge | `mcp_instance` | 1 = these credentials cannot rewrite an Application's source (blocked by the admission policy or by RBAC); **0 = the write guard is MISSING**; absent = could not be probed |
| `amcp_build_info` | gauge | `version`, `goversion` | constant 1 |
| `rest_client_requests_total` | counter | `host`, `code`, `method` | Kubernetes API calls (client-go adapter) |
| `rest_client_request_duration_seconds` | histogram | `host`, `verb` | Kubernetes API latency (client-go adapter) |

> The instance label is `mcp_instance`, not `instance`: a bare `instance` label
> collides with Prometheus' own target label and would be overwritten.

## Useful queries

```promql
# Instances currently unreachable
amcp_instance_up == 0

# Tool error ratio over 5m
sum(rate(amcp_tool_calls_total{result!="ok"}[5m])) by (tool)
  / sum(rate(amcp_tool_calls_total[5m])) by (tool)

# Syncs requested per instance today
sum(increase(amcp_operations_total{op="sync",result="ok"}[24h])) by (mcp_instance)

# ALERT: an instance whose credentials can rewrite an Application's source.
# This is a privilege-escalation path into that cluster — see docs/rbac.md.
amcp_write_guard_active == 0

# Attempts blocked by the read-only guard (a misconfiguration signal)
sum(increase(amcp_writes_blocked_total[1h])) by (mcp_instance, reason)

# Rejected authentications (probing / stale tokens)
sum(rate(amcp_auth_requests_total{result="deny"}[15m]))

# p95 tool latency
histogram_quantile(0.95,
  sum(rate(amcp_tool_call_duration_seconds_bucket[5m])) by (le, tool))

# Kubernetes API errors seen by the server
sum(rate(rest_client_requests_total{code=~"4..|5.."}[5m])) by (host, code)
```

## Scraping and dashboard

The chart ships a `ServiceMonitor` (`serviceMonitor.enabled=true`; the
victoria-metrics operator converts it to a `VMServiceScrape`) and a Grafana
dashboard as a sidecar-discovered ConfigMap:

```yaml
serviceMonitor:
  enabled: true
grafanaDashboard:
  enabled: true
  folder: "MCP Servers"
  folderAnnotation: grafana-folder   # victoria-metrics-k8s-stack uses a dash
```
