package analyzer

import "testing"

func TestNormalizeJobCategory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "exact", in: "Programmazione", want: "Programmazione"},
		{name: "case insensitive", in: "marketing", want: "Marketing"},
		{name: "alias devops", in: "devops/sysadmin", want: "Devops & Sysadmin"},
		{name: "english fallback alias", in: "other roles", want: "Altri ruoli"},
		{name: "unknown defaults", in: "data science", want: "Altri ruoli"},
		{name: "empty defaults", in: "", want: "Altri ruoli"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeJobCategory(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeJobCategory(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
