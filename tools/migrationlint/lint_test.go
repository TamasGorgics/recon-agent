package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// covers: FIN-01
func TestCheckFile(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []Finding
	}{
		{
			name: "clean numeric and bigint",
			src:  "CREATE TABLE t (amount BIGINT NOT NULL, rate NUMERIC(24,12));\n",
		},
		{
			name: "real",
			src:  "ALTER TABLE t ADD COLUMN amount REAL;\n",
			want: []Finding{{Path: "fixture.sql", Line: 1, Column: 33, Rule: "REAL"}},
		},
		{
			name: "double precision",
			src:  "CREATE TABLE t (amount DOUBLE PRECISION);\n",
			want: []Finding{{Path: "fixture.sql", Line: 1, Column: 24, Rule: "DOUBLE PRECISION"}},
		},
		{
			name: "float with precision",
			src:  "CREATE TABLE t (amount FLOAT(53));\n",
			want: []Finding{{Path: "fixture.sql", Line: 1, Column: 24, Rule: "FLOAT"}},
		},
		{
			name: "case insensitive",
			src:  "CREATE TABLE t (a ReAl, b DoUbLe PrEcIsIoN, c FlOaT);\n",
			want: []Finding{
				{Path: "fixture.sql", Line: 1, Column: 19, Rule: "REAL"},
				{Path: "fixture.sql", Line: 1, Column: 27, Rule: "DOUBLE PRECISION"},
				{Path: "fixture.sql", Line: 1, Column: 47, Rule: "FLOAT"},
			},
		},
		{
			name: "line comments ignored",
			src:  "-- REAL\n-- DOUBLE PRECISION\n-- FLOAT\nCREATE TABLE t (amount BIGINT);\n",
		},
		{
			name: "block comments ignored, including nesting",
			src:  "/* REAL /* DOUBLE PRECISION */ FLOAT */\nCREATE TABLE t (amount BIGINT);\n",
		},
		{
			name: "string literals ignored",
			src:  "INSERT INTO t VALUES ('REAL', 'DOUBLE PRECISION', 'FLOAT');\n",
		},
		{
			name: "dollar quoted strings ignored",
			src:  "SELECT $$REAL FLOAT DOUBLE PRECISION$$, $tag$REAL$tag$;\n",
		},
		{
			name: "quoted identifiers ignored",
			src:  "CREATE TABLE t (\"REAL\" BIGINT, \"DOUBLE PRECISION\" BIGINT);\n",
		},
		{
			name: "words inside identifiers ignored",
			src:  "CREATE TABLE t (real_amount BIGINT, double_precision_col BIGINT, floaty BIGINT);\n",
		},
		{
			name: "newline between double and precision",
			src:  "CREATE TABLE t (amount DOUBLE\nPRECISION);\n",
			want: []Finding{{Path: "fixture.sql", Line: 1, Column: 24, Rule: "DOUBLE PRECISION"}},
		},
		{
			name: "comment between double and precision",
			src:  "CREATE TABLE t (amount DOUBLE /* exact */ PRECISION);\n",
			want: []Finding{{Path: "fixture.sql", Line: 1, Column: 24, Rule: "DOUBLE PRECISION"}},
		},
		{
			name: "double without precision ignored",
			src:  "CREATE TABLE t (amount DOUBLE);\n",
		},
		{
			name: "multiple findings sorted by position",
			src:  "CREATE TABLE t (a REAL, b FLOAT);\n",
			want: []Finding{
				{Path: "fixture.sql", Line: 1, Column: 19, Rule: "REAL"},
				{Path: "fixture.sql", Line: 1, Column: 27, Rule: "FLOAT"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckFile("fixture.sql", []byte(tt.src))
			if len(got) != len(tt.want) {
				t.Fatalf("CheckFile() returned %d findings, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("finding %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// covers: FIN-01
func TestCheckDir(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		got, err := CheckDir(filepath.Join("testdata", "clean"))
		if err != nil {
			t.Fatalf("CheckDir: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("clean directory produced findings: %v", got)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		got, err := CheckDir(filepath.Join("testdata", "dirty"))
		if err != nil {
			t.Fatalf("CheckDir: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d findings, want 3: %v", len(got), got)
		}
		for i, rule := range []string{"REAL", "DOUBLE PRECISION", "FLOAT"} {
			if got[i].Rule != rule {
				t.Errorf("finding %d rule = %q, want %q", i, got[i].Rule, rule)
			}
			if got[i].Path != filepath.Join("testdata", "dirty", "001_dirty.sql") {
				t.Errorf("finding %d path = %q", i, got[i].Path)
			}
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := CheckDir(filepath.Join("testdata", "does-not-exist"))
		if err == nil {
			t.Fatal("CheckDir on a missing directory returned nil error")
		}
		var scanErr *ScanError
		if !errors.As(err, &scanErr) {
			t.Fatalf("error %v is not a *ScanError", err)
		}
		if scanErr.Path == "" {
			t.Error("ScanError.Path is empty")
		}
	})
}

// covers: FIN-01
func TestFindingString(t *testing.T) {
	f := Finding{Path: "migrations/001.sql", Line: 7, Column: 12, Rule: "REAL"}
	got := f.String()
	for _, want := range []string{
		"migrations/001.sql:7:12",
		"REAL",
		"FIN-01",
		"BIGINT",
		"NUMERIC",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Finding.String() = %q, missing %q", got, want)
		}
	}
}
