package runcontroller

import (
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
)

func TestMapStatus(t *testing.T) {
	cases := []struct {
		name   string
		status enumspb.WorkflowExecutionStatus
		paused bool
		want   string
	}{
		{"running", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, false, "RUNNING"},
		{"running and paused", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, true, "PAUSED"},
		{"completed", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, false, "SUCCEEDED"},
		{"failed", enumspb.WORKFLOW_EXECUTION_STATUS_FAILED, false, "FAILED"},
		{"timed out", enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, false, "FAILED"},
		{"terminated", enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, false, "FAILED"},
		{"canceled", enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, false, "CANCELLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapStatus(tc.status, tc.paused)
			if got != tc.want {
				t.Fatalf("mapStatus(%v, paused=%v) = %q, want %q", tc.status, tc.paused, got, tc.want)
			}
		})
	}
}
