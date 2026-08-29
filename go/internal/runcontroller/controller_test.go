package runcontroller

import (
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
)

func TestMapStatus(t *testing.T) {
	cases := []struct {
		name            string
		status          enumspb.WorkflowExecutionStatus
		paused          bool
		pendingApproval bool
		want            string
	}{
		{"running", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, false, false, "RUNNING"},
		{"running and paused", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, true, false, "PAUSED"},
		{"running with pending approval", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, false, true, "PAUSED_FOR_APPROVAL"},
		{"pending approval wins over plain pause", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, true, true, "PAUSED_FOR_APPROVAL"},
		{"completed", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, false, false, "SUCCEEDED"},
		{"failed", enumspb.WORKFLOW_EXECUTION_STATUS_FAILED, false, false, "FAILED"},
		{"timed out", enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, false, false, "FAILED"},
		{"terminated", enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, false, false, "FAILED"},
		{"canceled", enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, false, false, "CANCELLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapStatus(tc.status, tc.paused, tc.pendingApproval)
			if got != tc.want {
				t.Fatalf("mapStatus(%v, paused=%v, pendingApproval=%v) = %q, want %q", tc.status, tc.paused, tc.pendingApproval, got, tc.want)
			}
		})
	}
}
