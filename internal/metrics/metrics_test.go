package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordTool(t *testing.T) {
	RecordTool("apps_list", "tools", "ok", 10*time.Millisecond)
	RecordTool("apps_list", "", "ok", 10*time.Millisecond) // empty instance -> "-"
	if got := testutil.ToFloat64(toolCalls.WithLabelValues("apps_list", "tools", "ok")); got != 1 {
		t.Fatalf("toolCalls[tools] = %v, want 1", got)
	}
	if got := testutil.ToFloat64(toolCalls.WithLabelValues("apps_list", "-", "ok")); got != 1 {
		t.Fatalf("empty instance should be recorded as '-', got %v", got)
	}
}

func TestRecordAuthAndBlocked(t *testing.T) {
	RecordAuth("oidc", "allow")
	RecordWriteBlocked("tools", "global_readonly")
	if got := testutil.ToFloat64(authRequests.WithLabelValues("oidc", "allow")); got != 1 {
		t.Fatalf("authRequests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(writesBlocked.WithLabelValues("tools", "global_readonly")); got != 1 {
		t.Fatalf("writesBlocked = %v, want 1", got)
	}
}

func TestRecordOperation(t *testing.T) {
	RecordOperation("tools", "sync", "ok")
	RecordOperation("", "refresh", "error")
	if got := testutil.ToFloat64(operations.WithLabelValues("tools", "sync", "ok")); got != 1 {
		t.Fatalf("operations = %v, want 1", got)
	}
	if got := testutil.ToFloat64(operations.WithLabelValues("-", "refresh", "error")); got != 1 {
		t.Fatalf("empty instance should be recorded as '-', got %v", got)
	}
}

func TestSetInstanceUp(t *testing.T) {
	SetInstanceUp("tools", true)
	if got := testutil.ToFloat64(instanceUp.WithLabelValues("tools")); got != 1 {
		t.Fatalf("instanceUp = %v, want 1", got)
	}
	SetInstanceUp("tools", false)
	if got := testutil.ToFloat64(instanceUp.WithLabelValues("tools")); got != 0 {
		t.Fatalf("instanceUp = %v, want 0", got)
	}
}

func TestSetBuildInfo(t *testing.T) {
	SetBuildInfo("1.2.3")
	if got := testutil.CollectAndCount(buildInfo); got != 1 {
		t.Fatalf("buildInfo series = %d, want 1", got)
	}
}

func TestRegisterClientGoDoesNotPanic(t *testing.T) {
	RegisterClientGo()
}
