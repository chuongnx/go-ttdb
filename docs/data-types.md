# Data types

- [Reading columns (Scan)](#reading-columns-scan)
- [Sending parameters](#sending-parameters)
- [PL/SQL OUT parameters](#plsql-out-parameters)
- [XLA values](#xla-values)
- [Value rules](#value-rules)

## Reading columns (Scan)

| TimesTen | Go value (`Scan` into `any`) | Also scans into |
|---|---|---|
| TT_TINYINT, TT_SMALLINT, TT_INTEGER, TT_BIGINT, NUMBER(p≤18, 0) | `int64` | `int`, `int32`, `float64`, `string` |
| NUMBER(p,s), NUMBER, FLOAT, DECIMAL | `string`: exact decimal text with a leading zero (`0.25`, `-0.5`) | `float64`, `int64` (if integral), `sql.Null*`, any `sql.Scanner` decimal type |
| BINARY_FLOAT, BINARY_DOUBLE | `float64` | `float32`, `string` |
| DATE, TIMESTAMP(n), TT_DATE, TT_TIMESTAMP | `time.Time` in `go_location` | `sql.NullTime` |
| TIME | `time.Time` on 0000-01-01 | |
| CHAR, VARCHAR2, NCHAR, NVARCHAR2, TT_CHAR, TT_VARCHAR, … | `string` (UTF-8) | `[]byte`, `sql.NullString` |
| CLOB, NCLOB | `string` (UTF-8) | `[]byte` |
| BINARY, VARBINARY, BLOB | `[]byte` | |
| ROWID | `string` | |
| NULL (any type) | `nil` | `sql.Null*`, pointers, `[]byte` (nil) |

Notes:

- **NUMBER without precision and FLOAT(n).** TimesTen describes them as binary doubles. The driver
  fetches them as text, so every digit is kept.
- **Decimal types.** To get an arbitrary-precision decimal, scan into a `sql.Scanner` decimal type
  such as `shopspring/decimal`, or into `string`.
- **Character data** is exchanged as UTF-8. Use `ConnectionCharacterSet=AL32UTF8`.
- **LOBs** are read whole, in 64 KB pieces, into memory. There is no streaming API.

`rows.ColumnTypes()` gives `DatabaseTypeName()` (for example `NUMBER`, `VARCHAR2`, `TIMESTAMP`),
`Length()` for character and binary types, `DecimalSize()` and `Nullable()`.

## Sending parameters

| Go value | Bound as | Notes |
|---|---|---|
| `int`, `int8` … `int64`, `uint8` … `uint32` | BIGINT | |
| `uint64` | BIGINT | Values above `math.MaxInt64` are rejected by `database/sql`; send them as a string |
| `float32`, `float64` | DOUBLE | NaN and ±Inf are kept in BINARY_FLOAT / BINARY_DOUBLE columns |
| `bool` | 1 / 0 | |
| `string` | VARCHAR; longer than 32 KB: LONGVARCHAR (LOB) | `""` is stored as NULL |
| `[]byte` | VARBINARY; longer than 32 KB: LONGVARBINARY (LOB) | |
| `time.Time` | TIMESTAMP | converted to `go_location` first; DATE columns drop fractional seconds |
| `nil` | NULL | |
| exact decimal | `string` (`"12345.678"`) | TimesTen converts the text to NUMBER without loss |
| `driver.Valuer` | its `Value()` | e.g. `sql.NullString`, decimal types |

## PL/SQL OUT parameters

See the table in [plsql.md](plsql.md#out-parameters-of-every-type).

## XLA values

Each column of a row image is an [`xla.Value`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/xla#Value)
with a `Kind`:

| TimesTen column | `Kind` | Field |
|---|---|---|
| TT_TINYINT, TT_SMALLINT, TT_INTEGER, TT_BIGINT | `KindInt` | `Int` |
| BINARY_FLOAT, BINARY_DOUBLE | `KindFloat` | `Float` |
| NUMBER, FLOAT, TT_DECIMAL | `KindDecimal` | `Text` (exact, with a leading zero) |
| CHAR, VARCHAR2, NCHAR, NVARCHAR2 (and TT_ variants), ROWID | `KindString` | `Text` (UTF-8) |
| BINARY, VARBINARY | `KindBytes` | `Bytes` |
| DATE, TIMESTAMP, TT_DATE, TT_TIMESTAMP | `KindTime` | `Time` (in the reader's location, `xla.WithLocation`) |
| TIME | `KindTimeOfDay` | `Time` on 0000-01-01 |
| CLOB, NCLOB, BLOB (non-NULL) | `KindLOB` | none: XLA does not deliver LOB contents |
| NULL | `KindNull` | |

`Value.Any()` returns the value as `int64`, `float64`, `string`, `[]byte`, `time.Time` or `nil`.
`Value.String()` formats it for logs.

## Value rules

All of these rules are covered by `TestTypeEdgeCases` and related tests.

- **Empty string is NULL.** `""` is stored as NULL, which is Oracle semantics. Code that tells `""`
  from NULL must not rely on the database.
- **Wide NUMBER.** NUMBER(19) and wider can exceed `int64`. `Scan` into `int64` then fails with
  "value out of range"; scan into `string` for exact values.
- **CHAR padding.** CHAR values are blank-padded to their declared length. Compare with
  `strings.TrimRight`, or use VARCHAR2.
- **Time zones.** `time.Time` values are converted to `go_location` before they are stored. In a
  zone with daylight saving time, a wall-clock time that does not exist (the hour skipped in spring)
  is read back shifted by Go's normalization. Prefer `UTC`, or a zone without DST, for timestamps.
- **Precision.** TT_TIMESTAMP has microsecond resolution. TIMESTAMP(9) keeps nanoseconds.
- **Date range.** Years 1 to 9999 round-trip.
- **No INTERVAL.** TimesTen has no INTERVAL columns. `date1 - date2` is a NUMBER of days.
- **4-byte UTF-8 and control characters.** Emoji, supplementary characters, tabs, newlines and NUL
  characters round-trip in VARCHAR2. `VARCHAR2(n CHAR)` counts characters, not bytes.
- **Long text.** Text and LOBs over 64 KB with multibyte characters are read correctly over direct
  connections. Over client/server, CLOB has a driver defect; see
  [production.md](production.md#clob-over-clientserver).
