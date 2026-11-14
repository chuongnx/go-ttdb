package ttsql

import (
	"reflect"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	cfg, err := ParseDSN("DSN=db1;UID=app;PWD={a;b};go_location=UTC;go_stmt_cache=8;go_fetch_rows=10;go_batch_rows=50")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnString != "DSN=db1;UID=app;PWD={a;b}" {
		t.Errorf("ConnString = %q", cfg.ConnString)
	}
	if cfg.Location != time.UTC || cfg.StmtCacheSize != 8 || cfg.FetchRows != 10 || cfg.BatchRows != 50 {
		t.Errorf("options not applied: %+v", cfg)
	}
}

func TestParseDSNErrors(t *testing.T) {
	for _, dsn := range []string{"", "go_fetch_rows=1", "DSN=x;go_unknown=1", "DSN=x;go_stmt_cache=-1", "DSN=x;go_location=Nowhere/Zone"} {
		if _, err := ParseDSN(dsn); err == nil {
			t.Errorf("ParseDSN(%q) succeeded", dsn)
		}
	}
}

func TestSplitConnString(t *testing.T) {
	got := splitConnString("A=1;B={x;y;{z}};C=3")
	want := []string{"A=1", "B={x;y;{z}}", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAssignConversions(t *testing.T) {
	var s string
	var i int
	var f float64
	var b []byte
	var tm time.Time
	if err := assign(&s, int64(42)); err != nil || s != "42" {
		t.Errorf("string: %q %v", s, err)
	}
	if err := assign(&i, "17"); err != nil || i != 17 {
		t.Errorf("int: %d %v", i, err)
	}
	if err := assign(&f, "1.5"); err != nil || f != 1.5 {
		t.Errorf("float: %v %v", f, err)
	}
	if err := assign(&b, nil); err != nil || b != nil {
		t.Errorf("bytes nil: %v %v", b, err)
	}
	now := time.Now()
	if err := assign(&tm, now); err != nil || !tm.Equal(now) {
		t.Errorf("time: %v %v", tm, err)
	}
	if err := assign(&i, "abc"); err == nil {
		t.Error("expected conversion error")
	}
	if err := assign(&s, nil); err != nil || s != "" {
		t.Errorf("nil string: %q %v", s, err)
	}
}
