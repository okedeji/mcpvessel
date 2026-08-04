package main

import "testing"

func TestApprovalFailedEverywhere(t *testing.T) {
	tests := []struct {
		name     string
		allow    bool
		targeted int
		released int
		want     bool
	}{
		{
			// The case this exists for: the operator approved a host, every live
			// run refused it, and writing it to config would allow the host on
			// the next run off the back of a command that failed.
			name: "allow that no live run took", allow: true, targeted: 2, released: 0, want: true,
		},
		{
			name: "allow that some live run took", allow: true, targeted: 2, released: 1, want: false,
		},
		{
			// Pre-approving a host before any run exists is supported, and there
			// was nothing to fail.
			name: "allow with no live run at all", allow: true, targeted: 0, released: 0, want: false,
		},
		{
			// Forgetting a grant only tightens the cage, so a deny is recorded
			// even when it reached no live run.
			name: "deny that no live run took", allow: false, targeted: 2, released: 0, want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := approvalFailedEverywhere(tt.allow, tt.targeted, tt.released); got != tt.want {
				t.Fatalf("approvalFailedEverywhere(%v, %d, %d) = %v, want %v",
					tt.allow, tt.targeted, tt.released, got, tt.want)
			}
		})
	}
}
