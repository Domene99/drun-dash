package httpapi

import "testing"

func TestSameOrg(t *testing.T) {
	tests := []struct {
		name   string
		caller string
		row    string
		want   bool
	}{
		{name: "match", caller: "org-1", row: "org-1", want: true},
		{name: "mismatch", caller: "org-1", row: "org-2", want: false},
		{name: "empty row does not match real org", caller: "org-1", row: "", want: false},
		{name: "both empty match", caller: "", row: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameOrg(tt.caller, tt.row); got != tt.want {
				t.Errorf("SameOrg(%q, %q) = %v, want %v", tt.caller, tt.row, got, tt.want)
			}
		})
	}
}
