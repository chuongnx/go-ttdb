package tt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorPromotesFirstDiag(t *testing.T) {
	e := New("SQLExecute", []Diag{
		{SQLState: "40001", Code: ErrDeadlock, Message: "Deadlock"},
		{SQLState: "HY000", Code: 6003, Message: "Lock timeout"},
	})
	if e.Code != ErrDeadlock || e.SQLState != "40001" {
		t.Fatalf("primary diag not promoted: %+v", e)
	}
	s := e.Error()
	for _, want := range []string{"SQLExecute", "[6002]", "(40001)", "Deadlock", "[6003] Lock timeout"} {
		if !strings.Contains(s, want) {
			t.Errorf("Error() = %q, missing %q", s, want)
		}
	}
	if !e.IsTransient() || e.IsConnectionLost() {
		t.Errorf("classification wrong: transient=%v lost=%v", e.IsTransient(), e.IsConnectionLost())
	}
}

func TestConnectionLost(t *testing.T) {
	cases := []struct {
		d    Diag
		lost bool
	}{
		{Diag{SQLState: "08S01", Code: 1}, true},
		{Diag{SQLState: "HY000", Code: ErrConnTerminated}, true},
		{Diag{SQLState: "HY000", Code: ErrClientFailover}, true},
		{Diag{SQLState: "23000", Code: ErrKeyExists}, false},
	}
	for _, c := range cases {
		e := New("x", []Diag{c.d})
		if e.IsConnectionLost() != c.lost {
			t.Errorf("%+v: lost=%v, want %v", c.d, e.IsConnectionLost(), c.lost)
		}
	}
}

func TestHelpersUnwrap(t *testing.T) {
	err := fmt.Errorf("apply batch: %w", New("SQLExecute", []Diag{{Code: ErrLockTimeout}}))
	if Code(err) != ErrLockTimeout || !IsTransient(err) || IsConnectionLost(err) {
		t.Fatalf("helpers did not unwrap: code=%d", Code(err))
	}
	if Code(errors.New("plain")) != 0 || IsTransient(nil) {
		t.Fatal("non-TimesTen errors must not classify")
	}
}

func TestEmptyDiags(t *testing.T) {
	if e := New("op", nil); !strings.Contains(e.Error(), "no diagnostic") {
		t.Fatalf("got %q", e.Error())
	}
}
