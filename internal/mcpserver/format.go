package mcpserver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
)

// maxMessageLen caps free-form Argo CD messages so one broken application cannot
// flood the model's context.
const maxMessageLen = 500

// textResult wraps a plain string into an MCP tool result.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// errorResult wraps an error as an MCP *tool* error (IsError), so the model sees
// the message (including Kubernetes 403/404 text) instead of a transport failure.
func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// truncate shortens long messages deterministically.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + " …(truncated)"
}

// dash returns "-" for empty strings so columns stay aligned.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// appsTable renders a list of applications with a leading count and a
// sync/health histogram — the shape an operator scans first.
func appsTable(instance, scope string, apps []*argocd.App) string {
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Namespace != apps[j].Namespace {
			return apps[i].Namespace < apps[j].Namespace
		}
		return apps[i].Name < apps[j].Name
	})

	sync := map[string]int{}
	health := map[string]int{}
	for _, a := range apps {
		sync[dash(a.SyncStatus)]++
		health[dash(a.HealthStatus)]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d application(s) in %s @ %s (%s | %s):\n",
		len(apps), scope, instance, histogram(sync), histogram(health))
	for _, a := range apps {
		fmt.Fprintf(&b, "- %s/%s  sync=%s health=%s project=%s rev=%s auto=%s dest=%s",
			a.Namespace, a.Name, dash(a.SyncStatus), dash(a.HealthStatus),
			dash(a.Project), shortRev(a.SyncRevision), a.AutoSyncLabel(), a.Destination())
		if a.Operation.Running() {
			fmt.Fprintf(&b, " op=%s", a.Operation.Phase)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// histogram renders a bounded count map deterministically, e.g. "Synced 10, OutOfSync 2".
func histogram(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// shortRev trims a git SHA to 7 characters, leaving branch/tag names intact.
func shortRev(rev string) string {
	if len(rev) == 40 && isHex(rev) {
		return rev[:7]
	}
	return dash(rev)
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// appDetail renders one application in full.
func appDetail(instance string, a *argocd.App, historyLimit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Application %s/%s @ %s\n", a.Namespace, a.Name, instance)
	fmt.Fprintf(&b, "  project:     %s\n", dash(a.Project))
	fmt.Fprintf(&b, "  destination: %s\n", a.Destination())
	for i, s := range a.Sources {
		fmt.Fprintf(&b, "  source[%d]:   %s @ %s (%s)\n", i, dash(s.RepoURL), dash(s.TargetRevision), s.Location())
	}
	fmt.Fprintf(&b, "  syncPolicy:  automated=%s\n", a.AutoSyncLabel())
	fmt.Fprintf(&b, "  sync:        %s (revision %s)\n", dash(a.SyncStatus), shortRev(a.SyncRevision))
	fmt.Fprintf(&b, "  health:      %s", dash(a.HealthStatus))
	if a.HealthMessage != "" {
		fmt.Fprintf(&b, " — %s", truncate(a.HealthMessage, maxMessageLen))
	}
	b.WriteByte('\n')
	if a.ReconciledAt != nil {
		fmt.Fprintf(&b, "  reconciled:  %s\n", a.ReconciledAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if a.RefreshRequested != "" {
		fmt.Fprintf(&b, "  refresh:     %s requested (not yet reconciled)\n", a.RefreshRequested)
	}
	if a.HasPendingOp {
		b.WriteString("  operation:   queued (not started yet)\n")
	}
	if a.Operation != nil {
		fmt.Fprintf(&b, "  operation:   phase=%s", dash(a.Operation.Phase))
		if a.Operation.InitiatedBy != "" {
			fmt.Fprintf(&b, " by=%s", a.Operation.InitiatedBy)
		}
		if a.Operation.Message != "" {
			fmt.Fprintf(&b, " — %s", truncate(a.Operation.Message, maxMessageLen))
		}
		b.WriteByte('\n')
	}

	drifted := 0
	for _, r := range a.Resources {
		if r.Drifted() {
			drifted++
		}
	}
	fmt.Fprintf(&b, "  resources:   %d managed, %d drifted (use app_resources for detail)\n", len(a.Resources), drifted)

	if len(a.Conditions) > 0 {
		b.WriteString("  conditions:\n")
		for _, c := range a.Conditions {
			fmt.Fprintf(&b, "    - %s: %s\n", c.Type, truncate(c.Message, maxMessageLen))
		}
	}
	if len(a.History) > 0 {
		b.WriteString("  history (newest last, use the id with app_rollback):\n")
		b.WriteString(indent(historyTable(a, historyLimit), "    "))
	}
	return b.String()
}

// resourcesTable renders status.resources, optionally only the drifted ones.
func resourcesTable(a *argocd.App, onlyDrifted bool) string {
	var b strings.Builder
	shown := 0
	var lines []string
	for _, r := range a.Resources {
		if onlyDrifted && !r.Drifted() {
			continue
		}
		shown++
		kind := r.Kind
		if r.Group != "" {
			kind = r.Group + "/" + r.Kind
		}
		loc := r.Name
		if r.Namespace != "" {
			loc = r.Namespace + "/" + r.Name
		}
		line := fmt.Sprintf("- %s %s  sync=%s health=%s", kind, loc, dash(r.Status), dash(r.Health))
		if r.RequiresPruning {
			line += " requiresPruning=true"
		}
		if r.Hook {
			line += " hook=true"
		}
		if r.HealthMessage != "" {
			line += " — " + truncate(r.HealthMessage, maxMessageLen)
		}
		lines = append(lines, line)
	}
	scope := "resource(s)"
	if onlyDrifted {
		scope = "drifted resource(s)"
	}
	fmt.Fprintf(&b, "%d %s of %d managed by %s/%s:\n", shown, scope, len(a.Resources), a.Namespace, a.Name)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if shown == 0 && onlyDrifted {
		b.WriteString("(everything is Synced and Healthy; pass onlyDrifted=false to list all)\n")
	}
	return b.String()
}

// historyTable renders the last `limit` history entries, newest last.
func historyTable(a *argocd.App, limit int) string {
	hist := a.History
	if limit > 0 && len(hist) > limit {
		hist = hist[len(hist)-limit:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d history entr(ies) for %s/%s:\n", len(hist), a.Namespace, a.Name)
	for _, h := range hist {
		deployed := "-"
		if h.DeployedAt != nil {
			deployed = h.DeployedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(&b, "- id=%d revision=%s deployedAt=%s", h.ID, shortRev(h.Revision), deployed)
		if h.InitiatedBy != "" {
			fmt.Fprintf(&b, " by=%s", h.InitiatedBy)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// projectsTable renders AppProjects.
func projectsTable(instance string, projects []*argocd.Project) string {
	sort.SliceStable(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "%d AppProject(s) @ %s:\n", len(projects), instance)
	for _, p := range projects {
		fmt.Fprintf(&b, "- %s/%s", p.Namespace, p.Name)
		if p.Description != "" {
			fmt.Fprintf(&b, " — %s", truncate(p.Description, 120))
		}
		fmt.Fprintf(&b, "\n    sourceRepos: %s\n    destinations: %s\n",
			joinOr(p.SourceRepos, "none"), joinOr(p.Destinations, "none"))
		if p.ClusterResourceAll {
			b.WriteString("    clusterResourceWhitelist: set\n")
		}
	}
	return b.String()
}

// appSetsTable renders ApplicationSets and how many Applications each owns.
func appSetsTable(instance string, sets []*argocd.AppSet, owned map[string]int) string {
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "%d ApplicationSet(s) @ %s:\n", len(sets), instance)
	for _, s := range sets {
		fmt.Fprintf(&b, "- %s/%s generators=%s applications=%d\n",
			s.Namespace, s.Name, joinOr(s.Generators, "none"), owned[s.Namespace+"/"+s.Name])
		for _, c := range s.Conditions {
			fmt.Fprintf(&b, "    condition %s: %s\n", c.Type, truncate(c.Message, maxMessageLen))
		}
	}
	return b.String()
}

func joinOr(in []string, empty string) string {
	if len(in) == 0 {
		return empty
	}
	return strings.Join(in, ", ")
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
