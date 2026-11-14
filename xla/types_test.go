//go:build !tt_client && !darwin

package xla

import (
	"testing"
	"time"
)

func TestOpAndTypeNames(t *testing.T) {
	if OpInsert.String() != "INSERT" || OpTruncate.String() != "TRUNCATE" || Op(999).String() != "OP(999)" {
		t.Error("op names")
	}
	if !OpUpdate.IsDML() || OpTruncate.IsDML() {
		t.Error("IsDML")
	}
	if TypeNumber.String() != "NUMBER" || DataType(-999).String() != "TYPE(-999)" {
		t.Error("type names")
	}
}

func TestValueRendering(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)
	cases := []struct {
		v    Value
		want string
		any  any
	}{
		{Value{Kind: KindNull}, "NULL", nil},
		{Value{Kind: KindInt, Int: -7}, "-7", int64(-7)},
		{Value{Kind: KindDecimal, Text: "1.50"}, "1.50", "1.50"},
		{Value{Kind: KindBytes, Bytes: []byte{0xAB, 1}}, "0xAB01", []byte{0xAB, 1}},
		{Value{Kind: KindTime, Time: ts}, "2026-01-02 03:04:05.6", ts},
		{Value{Kind: KindLOB}, "<LOB>", nil},
	}
	for _, c := range cases {
		if c.v.String() != c.want {
			t.Errorf("String() = %q, want %q", c.v.String(), c.want)
		}
		if _, isBytes := c.any.([]byte); !isBytes && c.v.Any() != c.any {
			t.Errorf("Any() = %v, want %v", c.v.Any(), c.any)
		}
	}
	if !(Value{}).IsNull() {
		t.Error("zero value must be NULL")
	}
}

func TestRecordHelpers(t *testing.T) {
	tbl := &Table{Owner: "APP", Name: "T", Columns: []Column{{Name: "ID"}, {Name: "V"}},
		PrimaryKey: []int{0}, index: map[string]int{"ID": 0, "V": 1}}
	del := Record{Op: OpDelete, Owner: "APP", Name: "T", Table: tbl,
		Old: []Value{{Kind: KindInt, Int: 5}, {Kind: KindString, Text: "x"}}, Flags: FlagFirst | FlagCommit}
	if k := del.Key(); len(k) != 1 || k[0].Int != 5 {
		t.Errorf("key %v", k)
	}
	if v, ok := del.OldValue("V"); !ok || v.Text != "x" {
		t.Errorf("old value %v %v", v, ok)
	}
	if _, ok := del.NewValue("V"); ok {
		t.Error("delete has no new image")
	}
	if _, ok := del.OldValue("NOPE"); ok {
		t.Error("unknown column found")
	}
	if !del.FirstInTxn() || !del.EndsTxn() || del.QualifiedName() != "APP.T" || tbl.QualifiedName() != "APP.T" {
		t.Error("helpers")
	}
	if (&Record{Name: "X"}).QualifiedName() != "X" {
		t.Error("qualified name without owner")
	}
}

func TestSplitName(t *testing.T) {
	if o, n := splitName("", "app.orders"); o != "APP" || n != "ORDERS" {
		t.Errorf("%s %s", o, n)
	}
	if o, n := splitName("app", "orders"); o != "APP" || n != "ORDERS" {
		t.Errorf("%s %s", o, n)
	}
}
