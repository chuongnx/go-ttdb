//go:build !tt_client && !darwin

package cli

/*
#include "cli.h"
#include <tt_xla.h>

// Accessors that keep pointer arithmetic on XLA buffers in C.
static ttXlaUpdateDesc_t *gtt_xla_rec(ttXlaUpdateDesc_t **arr, int i) { return arr[i]; }
static void *gtt_xla_tup1(ttXlaUpdateDesc_t *r) { return (char *)r + sizeof(ttXlaUpdateDesc_t); }
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/chuongnx/go-ttdb/tt"
)

// XLA open options (ttXlaPersistOpen).
const (
	XlaCreate = int(C.XLACREAT)
	XlaReuse  = int(C.XLAREUSE)
	XlaRepl   = int(C.XLAREPL)
	XlaNone   = int(C.XLANONE)
)

// XLA record types (ttXlaUpdateDesc_t.type).
const (
	XlaCreateTable = int(C.CREATAB)
	XlaDropTable   = int(C.DROPTAB)
	XlaCreateIndex = int(C.CREAIND)
	XlaDropIndex   = int(C.DROPIND)
	XlaAddColumns  = int(C.ADDCOLS)
	XlaDropColumns = int(C.DRPCOLS)
	XlaInsert      = int(C.INSERTTUP)
	XlaUpdate      = int(C.UPDATETUP)
	XlaDelete      = int(C.DELETETUP)
	XlaCommitOnly  = int(C.COMMITONLY)
	XlaCreateView  = int(C.CREATVIEW)
	XlaDropView    = int(C.DROPVIEW)
	XlaCreateSeq   = int(C.CREATSEQ)
	XlaDropSeq     = int(C.DROPSEQ)
	XlaTruncate    = int(C.TRUNCATE)
	XlaCreateSyn   = int(C.CREATSYN)
	XlaDropSyn     = int(C.DROPSYN)
)

// XLA update flags.
const (
	XlaFlagCommit  = int(C.TT_UPDCOMMIT)
	XlaFlagFirst   = int(C.TT_UPDFIRST)
	XlaFlagRepl    = int(C.TT_UPDREPL)
	XlaFlagCols    = int(C.TT_UPDCOLS)
	XlaFlagDefault = int(C.TT_UPDDEFAULT)
	XlaFlagCascDel = int(C.TT_CASCDEL)
	XlaFlagAging   = int(C.TT_AGING)
)

// XLA column data types (ttXlaColDesc_t.dataType).
const (
	TTXLA_CHAR_TT      = int(C.TTXLA_CHAR_TT)
	TTXLA_NCHAR_TT     = int(C.TTXLA_NCHAR_TT)
	TTXLA_VARCHAR_TT   = int(C.TTXLA_VARCHAR_TT)
	TTXLA_NVARCHAR_TT  = int(C.TTXLA_NVARCHAR_TT)
	TTXLA_TINYINT      = int(C.TTXLA_TINYINT)
	TTXLA_SMALLINT     = int(C.TTXLA_SMALLINT)
	TTXLA_INTEGER      = int(C.TTXLA_INTEGER)
	TTXLA_BIGINT       = int(C.TTXLA_BIGINT)
	TTXLA_BINARY_FLOAT = int(C.TTXLA_BINARY_FLOAT)
	TTXLA_BINARY_DBL   = int(C.TTXLA_BINARY_DOUBLE)
	TTXLA_DECIMAL_TT   = int(C.TTXLA_DECIMAL_TT)
	TTXLA_TIME         = int(C.TTXLA_TIME)
	TTXLA_DATE_TT      = int(C.TTXLA_DATE_TT)
	TTXLA_TIMESTAMP_TT = int(C.TTXLA_TIMESTAMP_TT)
	TTXLA_BINARY       = int(C.TTXLA_BINARY)
	TTXLA_VARBINARY    = int(C.TTXLA_VARBINARY)
	TTXLA_ROWID        = int(C.TTXLA_ROWID)
	TTXLA_CLOB         = int(C.TTXLA_CLOB)
	TTXLA_NCLOB        = int(C.TTXLA_NCLOB)
	TTXLA_BLOB         = int(C.TTXLA_BLOB)
	TTXLA_NUMBER       = int(C.TTXLA_NUMBER)
	TTXLA_DATE         = int(C.TTXLA_DATE)
	TTXLA_TIMESTAMP    = int(C.TTXLA_TIMESTAMP)
	TTXLA_CHAR         = int(C.TTXLA_CHAR)
	TTXLA_NCHAR        = int(C.TTXLA_NCHAR)
	TTXLA_VARCHAR      = int(C.TTXLA_VARCHAR)
	TTXLA_NVARCHAR     = int(C.TTXLA_NVARCHAR)
	TTXLA_FLOAT        = int(C.TTXLA_FLOAT)
)

// XColumn is a column of a table as seen by XLA.
type XColumn struct {
	Name       string
	SysColNum  int
	DataType   int
	Size       int
	Precision  int
	Scale      int
	Nullable   bool
	PrimaryKey bool
	Unique     bool
	OutOfLine  bool

	desc C.ttXlaColDesc_t
}

// XTable is a table definition at one table version.
type XTable struct {
	Owner, Name string
	SysTableID  uint64
	Version     int64
	Columns     []XColumn
	PrimaryKey  []int // 0-based column indexes, declaration order

	ver C.ttXlaTblVerDesc_t
}

// XValue is one decoded column value. Kind uses the cli kinds plus KindLOB for LOB columns, whose
// contents XLA does not deliver.
type XValue struct {
	Kind  Kind
	Int   int64
	Float float64
	Str   string // KindString, KindDecimal (decimal text), ROWID
	Bytes []byte
	Time  time.Time
}

// KindLOB marks a non-NULL LOB value: XLA reports LOB columns but not their contents.
const KindLOB Kind = 100

// XRecord is one decoded transaction log record.
type XRecord struct {
	Type        int
	Flags       int
	TranID      uint64
	ConnID      uint64
	LSNFile     uint64
	LSNOffset   uint64
	SysTableID  uint64
	Owner, Name string
	Table       *XTable  // nil for records without row data
	Old, New    []XValue // aligned with Table.Columns
	UpdatedCols []int    // 0-based indexes of columns changed by an UPDATE
}

// XlaLSN is an opaque bookmark position (ttXlaGetLSN / ttXlaSetLSN).
type XlaLSN struct{ raw C.tt_XlaLsn_t }

// Xla is an open XLA handle on its own dedicated connection.
type Xla struct {
	conn    *Conn
	h       C.ttXlaHandle_h
	tables  map[uint64]*XTable
	loc     *time.Location
	charBuf unsafe.Pointer
	charCap int
}

func (x *Xla) err(op string) *tt.Error {
	var diags []tt.Diag
	var code C.SQLINTEGER
	var retLen C.SQLINTEGER
	msg := (*C.char)(cmalloc(4096))
	defer C.free(unsafe.Pointer(msg))
	for i := 0; i < 32; i++ {
		rc := C.ttXlaError(x.h, &code, msg, 4096, &retLen)
		if rc != C.SQL_SUCCESS && rc != C.SQL_SUCCESS_WITH_INFO {
			break
		}
		diags = append(diags, tt.Diag{Code: int(code), Message: strings.TrimSpace(C.GoString(msg))})
	}
	return tt.New(op, diags)
}

// XlaOpen opens an XLA handle on c (which must be a direct connection and is dedicated to XLA from
// now on). bookmark may be empty with XlaNone.
func (c *Conn) XlaOpen(bookmark string, options int, loc *time.Location) (*Xla, error) {
	var tag *C.SQLCHAR
	if bookmark != "" {
		cs := C.CString(bookmark)
		defer C.free(unsafe.Pointer(cs))
		tag = (*C.SQLCHAR)(unsafe.Pointer(cs))
	}
	var h C.ttXlaHandle_h
	rc := C.ttXlaPersistOpen(c.h, tag, C.SQLUINTEGER(options), &h)
	if !ok(rc) {
		var err *tt.Error
		if h != nil {
			x := &Xla{h: h}
			err = x.err("ttXlaPersistOpen")
			C.ttXlaClose(h)
		} else {
			err = c.err("ttXlaPersistOpen").(*tt.Error)
		}
		return nil, err
	}
	return &Xla{conn: c, h: h, tables: map[uint64]*XTable{}, loc: loc}, nil
}

// TableByName returns the system and user table ids.
func (x *Xla) TableByName(owner, name string) (uint64, uint64, error) {
	co, cn := C.CString(owner), C.CString(name)
	defer C.free(unsafe.Pointer(co))
	defer C.free(unsafe.Pointer(cn))
	var sys, user C.SQLUBIGINT
	if rc := C.ttXlaTableByName(x.h, co, cn, &sys, &user); !ok(rc) {
		return 0, 0, x.err("ttXlaTableByName(" + owner + "." + name + ")")
	}
	return uint64(sys), uint64(user), nil
}

// TableStatus enables (1) or disables (0) change tracking for this bookmark and returns the old status.
// newStatus < 0 only queries.
func (x *Xla) TableStatus(sys, user uint64, newStatus int) (int, error) {
	var old C.SQLINTEGER
	var nsp *C.SQLINTEGER
	ns := C.SQLINTEGER(newStatus)
	if newStatus >= 0 {
		nsp = &ns
	}
	if rc := C.ttXlaTableStatus(x.h, C.SQLUBIGINT(sys), C.SQLUBIGINT(user), &old, nsp); !ok(rc) {
		return 0, x.err("ttXlaTableStatus")
	}
	return int(old), nil
}

// Next fetches up to max committed records, waiting up to waitSec seconds when none are available
// (waitSec 0 = no wait). Records are decoded into Go memory before returning.
func (x *Xla) Next(max, waitSec int) ([]XRecord, error) {
	var arr **C.ttXlaUpdateDesc_t
	var n C.SQLINTEGER
	var rc C.SQLRETURN
	if waitSec > 0 {
		rc = C.ttXlaNextUpdateWait(x.h, &arr, C.SQLINTEGER(max), &n, C.SQLINTEGER(waitSec))
	} else {
		rc = C.ttXlaNextUpdate(x.h, &arr, C.SQLINTEGER(max), &n)
	}
	if !ok(rc) {
		return nil, x.err("ttXlaNextUpdate")
	}
	out := make([]XRecord, 0, int(n))
	for i := 0; i < int(n); i++ {
		r := C.gtt_xla_rec(arr, C.int(i))
		rec, err := x.decode(r)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// Ack acknowledges every record returned so far (persists the bookmark).
func (x *Xla) Ack() error {
	if rc := C.ttXlaAcknowledge(x.h); !ok(rc) {
		return x.err("ttXlaAcknowledge")
	}
	return nil
}

// GetLSN returns the current read position of the bookmark.
func (x *Xla) GetLSN() (XlaLSN, error) {
	var l XlaLSN
	if rc := C.ttXlaGetLSN(x.h, &l.raw); !ok(rc) {
		return l, x.err("ttXlaGetLSN")
	}
	return l, nil
}

// SetLSN rewinds the read position to a value from GetLSN (not before the last Ack).
func (x *Xla) SetLSN(l XlaLSN) error {
	if rc := C.ttXlaSetLSN(x.h, &l.raw); !ok(rc) {
		return x.err("ttXlaSetLSN")
	}
	return nil
}

// File and Offset of an XlaLSN (ordering only; values are not file positions).
func (l XlaLSN) File() uint64   { return uint64(l.raw.logFile) }
func (l XlaLSN) Offset() uint64 { return uint64(l.raw.logOffset) }

// DeleteBookmark deletes the bookmark of this handle (releases its log hold).
func (x *Xla) DeleteBookmark() error {
	if rc := C.ttXlaDeleteBookmark(x.h); !ok(rc) {
		return x.err("ttXlaDeleteBookmark")
	}
	return nil
}

// Close closes the handle (not the connection).
func (x *Xla) Close() error {
	if x.h == nil {
		return nil
	}
	rc := C.ttXlaClose(x.h)
	var err error
	if !ok(rc) {
		err = fmt.Errorf("timesten: ttXlaClose failed (rc %d)", int(rc))
	}
	x.h = nil
	if x.charBuf != nil {
		C.free(x.charBuf)
		x.charBuf = nil
	}
	return err
}

// ---------------------------------------------------------------------------------------------
// decoding

func cName(b []C.char) string {
	s := C.GoStringN(&b[0], C.int(len(b)))
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// tableFor returns the table definition matching the record's table version.
func (x *Xla) tableFor(r *C.ttXlaUpdateDesc_t) (*XTable, error) {
	sys := uint64(r.sysTableID)
	if t := x.tables[sys]; t != nil {
		var compat C.SQLINTEGER
		if rc := C.ttXlaTableVersionVerify(x.h, &t.ver, r, &compat); ok(rc) && compat != 0 {
			return t, nil
		}
	}
	t := &XTable{SysTableID: sys}
	if rc := C.ttXlaVersionTableInfo(x.h, r, &t.ver); !ok(rc) {
		return nil, x.err("ttXlaVersionTableInfo")
	}
	td := &t.ver.tblDesc
	t.Owner, t.Name, t.Version = cName(td.tblOwner[:]), cName(td.tblName[:]), int64(t.ver.tblVer)
	ncols := int(td.columns)
	descs := make([]C.ttXlaColDesc_t, max(ncols, 1))
	var got C.SQLINTEGER
	if rc := C.ttXlaVersionColumnInfo(x.h, r, &descs[0], C.SQLINTEGER(len(descs)), &got); !ok(rc) {
		return nil, x.err("ttXlaVersionColumnInfo")
	}
	t.Columns = make([]XColumn, int(got))
	bySys := map[int]int{}
	for i := range t.Columns {
		d := descs[i]
		fl := int(d.flags)
		t.Columns[i] = XColumn{
			Name: cName(d.colName[:]), SysColNum: int(d.sysColNum), DataType: int(d.dataType),
			Size: int(d.size), Precision: int(d.precision), Scale: int(d.scale),
			Nullable: fl&int(C.TT_COLNULLABLE) != 0 || d.nullOffset != 0, PrimaryKey: fl&int(C.TT_COLPRIMKEY) != 0,
			Unique: fl&int(C.TT_COLUNIQUE) != 0, OutOfLine: fl&int(C.TT_COLOUTOFLINE) != 0, desc: d,
		}
		bySys[int(d.sysColNum)] = i
	}
	for i := 0; i < int(td.nPrimCols) && i < len(td.primColsSys); i++ {
		if idx, found := bySys[int(td.primColsSys[i])]; found {
			t.PrimaryKey = append(t.PrimaryKey, idx)
		}
	}
	x.tables[sys] = t
	return t, nil
}

func (x *Xla) decode(r *C.ttXlaUpdateDesc_t) (XRecord, error) {
	rec := XRecord{
		Type: int(r._type), Flags: int(r.flags), TranID: uint64(r.tranID), ConnID: uint64(r.connID),
		LSNFile: uint64(r.LSN.logFile), LSNOffset: uint64(r.LSN.logOffset), SysTableID: uint64(r.sysTableID),
	}
	tup1 := C.gtt_xla_tup1(r)
	switch rec.Type {
	case XlaInsert, XlaUpdate, XlaDelete:
		t, err := x.tableFor(r)
		if err != nil {
			return rec, err
		}
		rec.Table, rec.Owner, rec.Name = t, t.Owner, t.Name
		first, err := x.decodeRow(t, tup1)
		if err != nil {
			return rec, err
		}
		switch rec.Type {
		case XlaInsert:
			rec.New = first
		case XlaDelete:
			rec.Old = first
		case XlaUpdate:
			rec.Old = first
			if rec.New, err = x.decodeRow(t, unsafe.Add(tup1, int(r.tuple1))); err != nil {
				return rec, err
			}
		}
		if rec.Flags&XlaFlagCols != 0 {
			cols := unsafe.Add(tup1, int(r.tuple1)+int(r.tuple2))
			cnt := int(*(*C.SQLUSMALLINT)(cols))
			nums := unsafe.Slice((*C.SQLUSMALLINT)(unsafe.Add(cols, 2)), cnt)
			for _, sysCol := range nums {
				for i, c := range t.Columns {
					if c.SysColNum == int(sysCol) {
						rec.UpdatedCols = append(rec.UpdatedCols, i)
						break
					}
				}
			}
		}
	case XlaTruncate:
		tp := (*C.ttXlaTruncateTableTup_t)(tup1)
		rec.Owner, rec.Name = cName(tp.tblOwner[:]), cName(tp.tblName[:])
		delete(x.tables, rec.SysTableID)
	case XlaDropTable:
		tp := (*C.ttXlaDropTableTup_t)(tup1)
		rec.Owner, rec.Name = cName(tp.tblOwner[:]), cName(tp.tblName[:])
		delete(x.tables, rec.SysTableID)
	case XlaCreateTable:
		td := (*C.ttXlaTblDesc_t)(tup1)
		rec.Owner, rec.Name = cName(td.tblOwner[:]), cName(td.tblName[:])
	case XlaAddColumns, XlaDropColumns, XlaCreateIndex, XlaDropIndex:
		if rec.Type == XlaCreateIndex {
			tp := (*C.ttXlaCreateIndexTup_t)(tup1)
			rec.Owner, rec.Name = cName(tp.tblOwner[:]), cName(tp.tblName[:])
		} else if rec.Type == XlaDropIndex {
			tp := (*C.ttXlaDropIndexTup_t)(tup1)
			rec.Owner, rec.Name = cName(tp.tblOwner[:]), cName(tp.tblName[:])
		} else {
			// The record carries no table name: take it from the table already seen, else from
			// the record's version, else from the current definition.
			if t := x.tables[rec.SysTableID]; t != nil {
				rec.Owner, rec.Name = t.Owner, t.Name
			} else if ver := (C.ttXlaTblVerDesc_t{}); ok(C.ttXlaVersionTableInfo(x.h, r, &ver)) {
				rec.Owner, rec.Name = cName(ver.tblDesc.tblOwner[:]), cName(ver.tblDesc.tblName[:])
			} else if td := (C.ttXlaTblDesc_t{}); ok(C.ttXlaGetTableInfo(x.h, C.SQLUBIGINT(rec.SysTableID), 0, &td)) {
				rec.Owner, rec.Name = cName(td.tblOwner[:]), cName(td.tblName[:])
			}
		}
		delete(x.tables, rec.SysTableID)
	}
	return rec, nil
}

func (x *Xla) decodeRow(t *XTable, tup unsafe.Pointer) ([]XValue, error) {
	vals := make([]XValue, len(t.Columns))
	for i := range t.Columns {
		c := &t.Columns[i]
		v, err := x.decodeValue(c, tup)
		if err != nil {
			return nil, fmt.Errorf("timesten: decode %s.%s.%s: %w", t.Owner, t.Name, c.Name, err)
		}
		vals[i] = v
	}
	return vals, nil
}

var ne = binary.NativeEndian

func bytesAt(p unsafe.Pointer, n int) []byte { return unsafe.Slice((*byte)(p), n) }

func utf16String(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = ne.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// varData returns the bytes of a variable-length column (VARCHAR2, NVARCHAR2, VARBINARY): the column
// slot holds an offset to an 8-byte length followed by the data (out of line), or the length itself.
func varData(c *XColumn, col unsafe.Pointer) []byte {
	lenPtr := col
	if c.OutOfLine {
		lenPtr = unsafe.Add(col, int(int32(ne.Uint32(bytesAt(col, 4)))))
	}
	n := int(ne.Uint64(bytesAt(lenPtr, 8)))
	return bytesAt(unsafe.Add(lenPtr, 8), n)
}

func (x *Xla) charConvert(c *XColumn, tup unsafe.Pointer, maxBytes int) (string, error) {
	need := maxBytes*4 + 16
	if need > x.charCap {
		if x.charBuf != nil {
			C.free(x.charBuf)
		}
		x.charBuf, x.charCap = cmalloc(need), need
	}
	if rc := C.ttXlaConvertCharType(x.h, &c.desc, tup, x.charBuf, C.size_t(x.charCap)); !ok(rc) {
		return "", x.err("ttXlaConvertCharType")
	}
	return C.GoString((*C.char)(x.charBuf)), nil
}

func (x *Xla) decodeValue(c *XColumn, tup unsafe.Pointer) (XValue, error) {
	d := &c.desc
	if d.nullOffset != 0 && *(*byte)(unsafe.Add(tup, int(d.nullOffset))) == 1 {
		return XValue{Kind: KindNull}, nil
	}
	col := unsafe.Add(tup, int(d.offset))
	var buf [128]C.char
	switch c.DataType {
	case TTXLA_TINYINT:
		return XValue{Kind: KindInt, Int: int64(*(*uint8)(col))}, nil
	case TTXLA_SMALLINT:
		return XValue{Kind: KindInt, Int: int64(int16(ne.Uint16(bytesAt(col, 2))))}, nil
	case TTXLA_INTEGER:
		return XValue{Kind: KindInt, Int: int64(int32(ne.Uint32(bytesAt(col, 4))))}, nil
	case TTXLA_BIGINT:
		return XValue{Kind: KindInt, Int: int64(ne.Uint64(bytesAt(col, 8)))}, nil
	case TTXLA_BINARY_FLOAT:
		return XValue{Kind: KindFloat, Float: float64(math.Float32frombits(ne.Uint32(bytesAt(col, 4))))}, nil
	case TTXLA_BINARY_DBL:
		return XValue{Kind: KindFloat, Float: math.Float64frombits(ne.Uint64(bytesAt(col, 8)))}, nil
	case TTXLA_NUMBER, TTXLA_FLOAT:
		if rc := C.ttXlaNumberToCString(x.h, col, &buf[0], C.int(len(buf))); !ok(rc) {
			return XValue{}, x.err("ttXlaNumberToCString")
		}
		return XValue{Kind: KindDecimal, Str: NormalizeDecimal(C.GoString(&buf[0]))}, nil
	case TTXLA_DECIMAL_TT:
		if rc := C.ttXlaDecimalToCString(col, &buf[0], C.SQLSMALLINT(c.Precision), C.SQLSMALLINT(c.Scale)); !ok(rc) {
			return XValue{}, x.err("ttXlaDecimalToCString")
		}
		return XValue{Kind: KindDecimal, Str: NormalizeDecimal(C.GoString(&buf[0]))}, nil
	case TTXLA_DATE, TTXLA_TIMESTAMP, TTXLA_TIMESTAMP_TT:
		var ts C.TIMESTAMP_STRUCT
		var rc C.SQLRETURN
		switch c.DataType {
		case TTXLA_DATE:
			rc = C.ttXlaOraDateToODBCTimeStamp(col, &ts)
		case TTXLA_TIMESTAMP:
			rc = C.ttXlaOraTimeStampToODBCTimeStamp(col, &ts)
		default:
			rc = C.ttXlaTimeStampToODBCCType(col, &ts)
		}
		if !ok(rc) {
			return XValue{}, x.err("ttXla*ToODBCTimeStamp")
		}
		if c.DataType == TTXLA_TIMESTAMP_TT {
			// TT_TIMESTAMP has microsecond resolution and the conversion reports the fraction in
			// microseconds (observed on 22.1), not nanoseconds as in ODBC.
			ts.fraction *= 1000
		}
		return XValue{Kind: KindTime, Time: getTimestamp(unsafe.Pointer(&ts), x.loc)}, nil
	case TTXLA_DATE_TT:
		var ds C.DATE_STRUCT
		if rc := C.ttXlaDateToODBCCType(col, &ds); !ok(rc) {
			return XValue{}, x.err("ttXlaDateToODBCCType")
		}
		return XValue{Kind: KindTime, Time: time.Date(int(ds.year), time.Month(ds.month), int(ds.day), 0, 0, 0, 0, x.loc)}, nil
	case TTXLA_TIME:
		var tm C.TIME_STRUCT
		if rc := C.ttXlaTimeToODBCCType(col, &tm); !ok(rc) {
			return XValue{}, x.err("ttXlaTimeToODBCCType")
		}
		return XValue{Kind: KindTimeOfDay, Time: time.Date(0, 1, 1, int(tm.hour), int(tm.minute), int(tm.second), 0, x.loc)}, nil
	case TTXLA_ROWID:
		if rc := C.ttXlaRowidToCString(col, &buf[0], C.int(len(buf))); !ok(rc) {
			return XValue{}, x.err("ttXlaRowidToCString")
		}
		return XValue{Kind: KindString, Str: C.GoString(&buf[0])}, nil
	case TTXLA_BINARY:
		return XValue{Kind: KindBytes, Bytes: append([]byte(nil), bytesAt(col, c.Size)...)}, nil
	case TTXLA_VARBINARY:
		return XValue{Kind: KindBytes, Bytes: append([]byte{}, varData(c, col)...)}, nil
	case TTXLA_NCHAR_TT, TTXLA_NCHAR: // UTF-16, native byte order (ttXlaConvertCharType rejects N types)
		return XValue{Kind: KindString, Str: utf16String(bytesAt(col, c.Size))}, nil // Size is in bytes
	case TTXLA_NVARCHAR_TT, TTXLA_NVARCHAR:
		return XValue{Kind: KindString, Str: utf16String(varData(c, col))}, nil
	case TTXLA_CHAR_TT, TTXLA_VARCHAR_TT, TTXLA_CHAR, TTXLA_VARCHAR:
		maxBytes := c.Size
		if c.DataType == TTXLA_VARCHAR || c.DataType == TTXLA_VARCHAR_TT {
			maxBytes = max(maxBytes, len(varData(c, col)))
		}
		s, err := x.charConvert(c, tup, maxBytes)
		if err != nil {
			return XValue{}, err
		}
		return XValue{Kind: KindString, Str: s}, nil
	case TTXLA_CLOB, TTXLA_NCLOB, TTXLA_BLOB:
		return XValue{Kind: KindLOB}, nil
	}
	return XValue{Kind: KindBytes, Bytes: append([]byte(nil), bytesAt(col, max(c.Size, 0))...)},
		fmt.Errorf("unsupported XLA data type %d", c.DataType)
}
