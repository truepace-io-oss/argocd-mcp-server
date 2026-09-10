package argocd

import (
	"encoding/json"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Source is one entry of spec.source / spec.sources.
type Source struct {
	RepoURL        string
	Path           string
	Chart          string
	TargetRevision string
	Ref            string
}

// Location returns "path" or "chart" — whichever the source uses.
func (s Source) Location() string {
	if s.Chart != "" {
		return "chart:" + s.Chart
	}
	if s.Path != "" {
		return s.Path
	}
	return "."
}

// ResourceStatus is one entry of status.resources: a resource Argo CD manages
// for this application, with its own sync and health state.
type ResourceStatus struct {
	Group           string
	Version         string
	Kind            string
	Namespace       string
	Name            string
	Status          string // Synced | OutOfSync | Unknown | ""
	Health          string // Healthy | Progressing | Degraded | Missing | Suspended | ""
	HealthMessage   string
	Hook            bool
	RequiresPruning bool
	SyncWave        int64
}

// Drifted reports whether this resource deviates from the desired state or is
// not healthy — the filter behind app_resources' onlyDrifted.
func (r ResourceStatus) Drifted() bool {
	if r.RequiresPruning {
		return true
	}
	if r.Status != "" && r.Status != SyncSynced {
		return true
	}
	if r.Health != "" && r.Health != HealthHealthy {
		return true
	}
	return false
}

// HistoryEntry is one entry of status.history — a past successful sync, and the
// unit app_rollback targets.
type HistoryEntry struct {
	ID          int64
	Revision    string
	Revisions   []string
	DeployedAt  *time.Time
	Source      map[string]any
	Sources     []any
	InitiatedBy string
}

// Condition is one entry of status.conditions.
type Condition struct {
	Type    string
	Message string
}

// OperationState projects status.operationState.
type OperationState struct {
	Phase       string
	Message     string
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Revision    string
	InitiatedBy string
}

// Running reports whether an operation currently occupies the application.
func (o *OperationState) Running() bool {
	if o == nil {
		return false
	}
	return o.Phase == PhaseRunning || o.Phase == PhaseTerminating
}

// App is the projection of an Argo CD Application the MCP works with.
type App struct {
	Namespace string
	Name      string
	Project   string

	Sources       []Source
	DestServer    string
	DestName      string
	DestNamespace string

	AutoSync  bool
	SelfHeal  bool
	AutoPrune bool

	SyncStatus    string
	SyncRevision  string
	HealthStatus  string
	HealthMessage string
	ReconciledAt  *time.Time

	// HasPendingOp is true when the top-level `operation` field is set, i.e. an
	// operation has been requested but not yet picked up by the controller.
	HasPendingOp bool
	Operation    *OperationState

	Resources  []ResourceStatus
	History    []HistoryEntry
	Conditions []Condition

	RefreshRequested string // value of the refresh annotation, "" when absent
	Labels           map[string]string
}

// Destination renders the destination as "<cluster>/<namespace>".
func (a *App) Destination() string {
	cluster := a.DestName
	if cluster == "" {
		cluster = a.DestServer
	}
	if cluster == "" {
		cluster = "?"
	}
	return cluster + "/" + a.DestNamespace
}

// AutoSyncLabel renders the sync policy compactly, e.g. "true(selfHeal,prune)".
func (a *App) AutoSyncLabel() string {
	if !a.AutoSync {
		return "false"
	}
	var extra []string
	if a.SelfHeal {
		extra = append(extra, "selfHeal")
	}
	if a.AutoPrune {
		extra = append(extra, "prune")
	}
	if len(extra) == 0 {
		return "true"
	}
	out := "true("
	for i, e := range extra {
		if i > 0 {
			out += ","
		}
		out += e
	}
	return out + ")"
}

// AppFromUnstructured projects an Application object. It tolerates every field
// being absent (a freshly created Application has no status at all).
func AppFromUnstructured(u *unstructured.Unstructured) *App {
	a := &App{
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		Labels:    u.GetLabels(),
	}
	if ann := u.GetAnnotations(); ann != nil {
		a.RefreshRequested = ann[AnnotationRefresh]
	}
	obj := u.Object

	a.Project, _, _ = unstructured.NestedString(obj, "spec", "project")
	a.DestServer, _, _ = unstructured.NestedString(obj, "spec", "destination", "server")
	a.DestName, _, _ = unstructured.NestedString(obj, "spec", "destination", "name")
	a.DestNamespace, _, _ = unstructured.NestedString(obj, "spec", "destination", "namespace")

	if src, found, _ := unstructured.NestedMap(obj, "spec", "source"); found {
		a.Sources = append(a.Sources, sourceFromMap(src))
	}
	if srcs, found, _ := unstructured.NestedSlice(obj, "spec", "sources"); found {
		for _, s := range srcs {
			if m, ok := s.(map[string]any); ok {
				a.Sources = append(a.Sources, sourceFromMap(m))
			}
		}
	}

	if auto, found, _ := unstructured.NestedMap(obj, "spec", "syncPolicy", "automated"); found {
		a.AutoSync = true
		a.SelfHeal, _, _ = unstructured.NestedBool(auto, "selfHeal")
		a.AutoPrune, _, _ = unstructured.NestedBool(auto, "prune")
	}

	_, a.HasPendingOp, _ = unstructured.NestedMap(obj, "operation")

	a.SyncStatus, _, _ = unstructured.NestedString(obj, "status", "sync", "status")
	a.SyncRevision, _, _ = unstructured.NestedString(obj, "status", "sync", "revision")
	a.HealthStatus, _, _ = unstructured.NestedString(obj, "status", "health", "status")
	a.HealthMessage, _, _ = unstructured.NestedString(obj, "status", "health", "message")
	a.ReconciledAt = nestedTime(obj, "status", "reconciledAt")

	if os, found, _ := unstructured.NestedMap(obj, "status", "operationState"); found {
		st := &OperationState{}
		st.Phase, _, _ = unstructured.NestedString(os, "phase")
		st.Message, _, _ = unstructured.NestedString(os, "message")
		st.StartedAt = nestedTime(os, "startedAt")
		st.FinishedAt = nestedTime(os, "finishedAt")
		st.Revision, _, _ = unstructured.NestedString(os, "syncResult", "revision")
		st.InitiatedBy, _, _ = unstructured.NestedString(os, "operation", "initiatedBy", "username")
		a.Operation = st
	}

	if res, found, _ := unstructured.NestedSlice(obj, "status", "resources"); found {
		for _, r := range res {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			rs := ResourceStatus{}
			rs.Group, _, _ = unstructured.NestedString(m, "group")
			rs.Version, _, _ = unstructured.NestedString(m, "version")
			rs.Kind, _, _ = unstructured.NestedString(m, "kind")
			rs.Namespace, _, _ = unstructured.NestedString(m, "namespace")
			rs.Name, _, _ = unstructured.NestedString(m, "name")
			rs.Status, _, _ = unstructured.NestedString(m, "status")
			rs.Health, _, _ = unstructured.NestedString(m, "health", "status")
			rs.HealthMessage, _, _ = unstructured.NestedString(m, "health", "message")
			rs.Hook, _, _ = unstructured.NestedBool(m, "hook")
			rs.RequiresPruning, _, _ = unstructured.NestedBool(m, "requiresPruning")
			rs.SyncWave = nestedInt64(m, "syncWave")
			a.Resources = append(a.Resources, rs)
		}
	}

	if hist, found, _ := unstructured.NestedSlice(obj, "status", "history"); found {
		for _, h := range hist {
			m, ok := h.(map[string]any)
			if !ok {
				continue
			}
			e := HistoryEntry{}
			e.ID = nestedInt64(m, "id")
			e.Revision, _, _ = unstructured.NestedString(m, "revision")
			e.Revisions, _, _ = unstructured.NestedStringSlice(m, "revisions")
			e.DeployedAt = nestedTime(m, "deployedAt")
			e.InitiatedBy, _, _ = unstructured.NestedString(m, "initiatedBy", "username")
			if src, found, _ := unstructured.NestedMap(m, "source"); found {
				e.Source = src
			}
			if srcs, found, _ := unstructured.NestedSlice(m, "sources"); found {
				e.Sources = srcs
			}
			a.History = append(a.History, e)
		}
		// Newest last is Argo CD's own ordering; make it explicit so callers can
		// rely on it regardless of how the object was written.
		sort.SliceStable(a.History, func(i, j int) bool { return a.History[i].ID < a.History[j].ID })
	}

	if conds, found, _ := unstructured.NestedSlice(obj, "status", "conditions"); found {
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cd := Condition{}
			cd.Type, _, _ = unstructured.NestedString(m, "type")
			cd.Message, _, _ = unstructured.NestedString(m, "message")
			a.Conditions = append(a.Conditions, cd)
		}
	}

	return a
}

func sourceFromMap(m map[string]any) Source {
	s := Source{}
	s.RepoURL, _, _ = unstructured.NestedString(m, "repoURL")
	s.Path, _, _ = unstructured.NestedString(m, "path")
	s.Chart, _, _ = unstructured.NestedString(m, "chart")
	s.TargetRevision, _, _ = unstructured.NestedString(m, "targetRevision")
	s.Ref, _, _ = unstructured.NestedString(m, "ref")
	return s
}

// nestedInt64 reads an integer field tolerantly. The API server decodes JSON
// numbers into int64, but objects built from plain encoding/json (test
// fixtures) or from typed structs can carry float64 / json.Number instead.
func nestedInt64(obj map[string]any, fields ...string) int64 {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

func nestedTime(obj map[string]any, fields ...string) *time.Time {
	s, found, _ := unstructured.NestedString(obj, fields...)
	if !found || s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// Project is the projection of an AppProject.
type Project struct {
	Namespace          string
	Name               string
	Description        string
	SourceRepos        []string
	Destinations       []string // "<server|name>/<namespace>"
	ClusterResourceAll bool
}

// ProjectFromUnstructured projects an AppProject object.
func ProjectFromUnstructured(u *unstructured.Unstructured) *Project {
	p := &Project{Namespace: u.GetNamespace(), Name: u.GetName()}
	obj := u.Object
	p.Description, _, _ = unstructured.NestedString(obj, "spec", "description")
	p.SourceRepos, _, _ = unstructured.NestedStringSlice(obj, "spec", "sourceRepos")
	if dests, found, _ := unstructured.NestedSlice(obj, "spec", "destinations"); found {
		for _, d := range dests {
			m, ok := d.(map[string]any)
			if !ok {
				continue
			}
			server, _, _ := unstructured.NestedString(m, "server")
			name, _, _ := unstructured.NestedString(m, "name")
			ns, _, _ := unstructured.NestedString(m, "namespace")
			target := name
			if target == "" {
				target = server
			}
			p.Destinations = append(p.Destinations, target+"/"+ns)
		}
	}
	if wl, found, _ := unstructured.NestedSlice(obj, "spec", "clusterResourceWhitelist"); found && len(wl) > 0 {
		p.ClusterResourceAll = true
	}
	return p
}

// AppSet is the projection of an ApplicationSet.
type AppSet struct {
	Namespace  string
	Name       string
	Generators []string
	Conditions []Condition
}

// AppSetFromUnstructured projects an ApplicationSet object.
func AppSetFromUnstructured(u *unstructured.Unstructured) *AppSet {
	s := &AppSet{Namespace: u.GetNamespace(), Name: u.GetName()}
	obj := u.Object
	if gens, found, _ := unstructured.NestedSlice(obj, "spec", "generators"); found {
		for _, g := range gens {
			m, ok := g.(map[string]any)
			if !ok {
				continue
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			s.Generators = append(s.Generators, keys...)
		}
	}
	if conds, found, _ := unstructured.NestedSlice(obj, "status", "conditions"); found {
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cd := Condition{}
			cd.Type, _, _ = unstructured.NestedString(m, "type")
			cd.Message, _, _ = unstructured.NestedString(m, "message")
			s.Conditions = append(s.Conditions, cd)
		}
	}
	return s
}
