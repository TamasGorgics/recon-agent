package main

import "testing"

// TestTestDBCleanDSN pins D-3: `testdb clean` must target TEST_DATABASE_URL so
// it can never drop orphan test databases on the wrong server. DATABASE_URL is
// only a fallback when TEST_DATABASE_URL is unset.
func TestTestDBCleanDSN(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")

	cases := []struct {
		name        string
		testURL     string
		databaseURL string
		want        string
		wantErr     bool
	}{
		{
			name:        "prefers TEST_DATABASE_URL",
			testURL:     "postgres://test@test-server:5432/recon",
			databaseURL: "postgres://prod@prod-server:5432/recon",
			want:        "postgres://test@test-server:5432/recon",
		},
		{
			name:        "falls back to DATABASE_URL",
			databaseURL: "postgres://prod@prod-server:5432/recon",
			want:        "postgres://prod@prod-server:5432/recon",
		},
		{
			name:    "requires a DSN",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_DATABASE_URL", tc.testURL)
			t.Setenv("DATABASE_URL", tc.databaseURL)

			got, err := testdbCleanDSN()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("testdbCleanDSN() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("testdbCleanDSN(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("testdbCleanDSN() = %q, want %q", got, tc.want)
			}
		})
	}
}
