package ipc

import "testing"

func TestPeerCanAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		usr  usr
		want bool
	}{
		{
			name: "admin allowed",
			usr:  usr{isAdmin: true},
			want: true,
		},
		{
			name: "control group allowed",
			usr:  usr{inControlGroup: true},
			want: true,
		},
		{
			name: "neither denied",
			usr:  usr{},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := peerCanAccess(tc.usr)
			if got != tc.want {
				t.Fatalf("peerCanAccess() = %v, want %v", got, tc.want)
			}
		})
	}
}
