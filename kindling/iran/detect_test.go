package iran

import "testing"

func TestLikelyIran(t *testing.T) {
	cases := []struct {
		name   string
		mcc    string
		tzName string
		want   bool
	}{
		{
			name:   "MCC=432 (Iranian network) alone",
			mcc:    "432",
			tzName: "",
			want:   true,
		},
		{
			name:   "MCC=432 overrides non-Tehran TZ (Iranian roaming in from US)",
			mcc:    "432",
			tzName: "America/Los_Angeles",
			want:   true,
		},
		{
			name:   "MCC=310 (US network) overrides Tehran TZ (diaspora user)",
			mcc:    "310",
			tzName: IranTZName,
			want:   false,
		},
		{
			name:   "MCC=262 (Germany) overrides Tehran TZ (Iranian student in Berlin)",
			mcc:    "262",
			tzName: IranTZName,
			want:   false,
		},
		{
			name:   "MCC=424 (UAE) overrides Tehran TZ",
			mcc:    "424",
			tzName: IranTZName,
			want:   false,
		},
		{
			name:   "no MCC, TZ=Tehran (iOS / WiFi-only in Iran)",
			mcc:    "",
			tzName: IranTZName,
			want:   true,
		},
		{
			name:   "no MCC, TZ=non-Tehran",
			mcc:    "",
			tzName: "America/Los_Angeles",
			want:   false,
		},
		{
			name:   "no MCC, TZ=UTC (containerized / default)",
			mcc:    "",
			tzName: "UTC",
			want:   false,
		},
		{
			name:   "no MCC, no TZ",
			mcc:    "",
			tzName: "",
			want:   false,
		},
		{
			name:   "empty-but-not-nil MCC treated as absent",
			mcc:    "",
			tzName: IranTZName,
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LikelyIran(tc.mcc, tc.tzName); got != tc.want {
				t.Errorf("LikelyIran(%q, %q) = %v, want %v",
					tc.mcc, tc.tzName, got, tc.want)
			}
		})
	}
}

// LocalTZName depends on the test host's timezone, so we can only
// assert the contract: it returns a string and never panics.
func TestLocalTZName_NoPanic(t *testing.T) {
	_ = LocalTZName()
}
