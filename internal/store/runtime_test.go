package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// These tests exercise the pure value logic in the runtime + telemetry
// repositories that needs no live database: the enum Valid() methods and the
// terminal-status pre-check in ApprovalRequestStore.Decide. Database-touching
// coverage lives in runtime_integration_test.go and
// telemetry_integration_test.go behind the `integration` build tag.

func TestSessionStatusValid(t *testing.T) {
	valid := []SessionStatus{SessionStatusPending, SessionStatusActive, SessionStatusTerminated, SessionStatusFailed}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("SessionStatus(%q).Valid() = false, want true", s)
		}
	}
	invalid := []SessionStatus{"", "Pending", "ACTIVE", "running", "done"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("SessionStatus(%q).Valid() = true, want false", s)
		}
	}
}

func TestApprovalStatusValid(t *testing.T) {
	valid := []ApprovalStatus{ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusDenied, ApprovalStatusExpired}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("ApprovalStatus(%q).Valid() = false, want true", s)
		}
	}
	invalid := []ApprovalStatus{"", "Approved", "DENIED", "rejected", "granted"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("ApprovalStatus(%q).Valid() = true, want false", s)
		}
	}
}

func TestTrajectoryFieldClassValid(t *testing.T) {
	valid := []TrajectoryFieldClass{TrajectoryFieldClassStructure, TrajectoryFieldClassPayload}
	for _, c := range valid {
		if !c.Valid() {
			t.Errorf("TrajectoryFieldClass(%q).Valid() = false, want true", c)
		}
	}
	invalid := []TrajectoryFieldClass{"", "Structure", "PAYLOAD", "raw", "meta"}
	for _, c := range invalid {
		if c.Valid() {
			t.Errorf("TrajectoryFieldClass(%q).Valid() = true, want false", c)
		}
	}
}

// TestApprovalDecideRejectsBadStatusBeforeDB proves Decide refuses a
// non-terminal status (pending or an unknown value) before issuing any SQL.
func TestApprovalDecideRejectsBadStatusBeforeDB(t *testing.T) {
	store := NewApprovalRequestStore(failQuerier{t: t})
	ctx := context.Background()
	id := "11111111-1111-1111-1111-111111111111"

	for _, status := range []ApprovalStatus{ApprovalStatusPending, "", "bogus"} {
		t.Run(string(status), func(t *testing.T) {
			_, err := store.Decide(ctx, id, status, nil)
			if err == nil {
				t.Fatalf("Decide(%q) error = nil, want a validation error", status)
			}
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
				t.Fatalf("Decide(%q) error = %v, want a plain validation error (not a store sentinel)", status, err)
			}
			if !strings.Contains(err.Error(), "terminal decision") {
				t.Fatalf("Decide(%q) error = %q, want it to mention a terminal decision", status, err)
			}
		})
	}
}
