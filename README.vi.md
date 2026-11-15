# go-ttdb

[![Go Reference](https://pkg.go.dev/badge/github.com/chuongnx/go-ttdb.svg)](https://pkg.go.dev/github.com/chuongnx/go-ttdb)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Thư viện Go cho Oracle TimesTen In-Memory Database**, gồm hai phần: driver `database/sql`, và bộ
đọc Transaction Log API (XLA) để bắt thay đổi dữ liệu (change data capture). Cả hai gọi thẳng API C
ODBC và XLA của TimesTen qua cgo, không cần JVM.

[English](README.md)

| Package | Chức năng |
|---|---|
| [`ttsql`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/ttsql) | Driver `database/sql`: pool kết nối, transaction, prepared statement, mọi kiểu dữ liệu TimesTen, gọi PL/SQL có tham số OUT / IN OUT và REF CURSOR, thực thi batch bằng mảng tham số, durable commit, huỷ câu lệnh qua context |
| [`xla`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/xla) | Đọc thay đổi từ transaction log: INSERT / UPDATE / DELETE / TRUNCATE / DDL đã commit, theo đúng thứ tự commit, có ảnh trước và sau của dòng, bookmark lưu bền, xác nhận (ack) và đọc lại (rewind) |
| [`xla/admin`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/xla/admin) | Quản lý bookmark, subscription và log hold qua các thủ tục built-in của TimesTen |
| [`tt`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/tt) | Kiểu lỗi `*tt.Error` (mã lỗi gốc, SQLSTATE, mọi bản ghi chẩn đoán) và hàm phân loại (`IsTransient`, `IsConnectionLost`) |
| [`ttxtest`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/ttxtest) | Hàm tiện ích để viết integration test với database thật |

## Tính năng

- Kết nối direct và client/server. Trên macOS chỉ cần thư viện client.
- Hỗ trợ mọi kiểu cột của TimesTen, kể cả LOB. NUMBER được trả về dạng chuỗi thập phân chính xác,
  không mất chữ số nào.
- PL/SQL: tham số OUT và IN OUT của mọi kiểu vô hướng qua `sql.Out`, giá trị trả về của hàm, và nhiều
  REF CURSOR trong một lần gọi.
- `ExecBatch` dùng mảng tham số ODBC: mỗi 1024 dòng chỉ một lượt gửi, và báo chính xác dòng nào lỗi.
- Các hành vi cần cho production đều có test kiểm chứng:
  - Statement bị DDL làm mất hiệu lực sẽ được prepare lại tự động.
  - Khi mất mạng, kết nối chết trong pool được thay thế mà ứng dụng không phải nhận lỗi lặp lại.
  - Deadlock và lock timeout được phân loại để ứng dụng biết mà retry.
  - Huỷ câu lệnh qua `context`.
  - Không rò rỉ bộ nhớ khi chạy lâu.
- XLA:
  - Giá trị được giải mã sang kiểu Go.
  - Mỗi UPDATE có danh sách cột bị thay đổi.
  - Có bản ghi TRUNCATE và DDL.
  - Khi daemon khởi động lại, đọc tiếp được mà không mất bản ghi chưa ack và không lặp lại bản ghi đã ack.

## Yêu cầu

- **Oracle TimesTen 22.1** (bản client hoặc server) trên máy build và máy chạy. Thư viện này không
  chứa và không phân phối lại bất kỳ phần nào của TimesTen.
- Go 1.22 trở lên, bật cgo, có trình biên dịch C (gcc hoặc clang).
- Muốn dùng XLA: kết nối **direct**, tức chương trình phải chạy trên máy chứa database, và user phải
  có quyền `XLA`.

| Nền tảng | `ttsql` direct | `ttsql` client/server | `xla` | Trạng thái |
|---|---|---|---|---|
| Linux x86-64 | ✓ | ✓ | ✓ | đã test |
| Linux arm64 | ✓ | ✓ | ✓ | đã test |
| macOS (arm64 / x86-64) | – | ✓ | – | đã test; Oracle chỉ phát hành bản client cho macOS |
| AIX 7.2+ (POWER) | ✓ | ✓ | ✓ | build được với gcc ≥ 10, chưa kiểm chứng; xem [hướng dẫn test trên AIX](docs/testing.md#testing-on-aix) |

## Cài đặt

```bash
go get github.com/chuongnx/go-ttdb@latest
```

Chỉ cho cgo đường dẫn tới bản cài TimesTen. Chi tiết cho từng nền tảng có trong
[docs/installation.md](docs/installation.md).

```bash
export CGO_ENABLED=1
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib"
export LD_LIBRARY_PATH="$TIMESTEN_HOME/install/lib"   # macOS: thêm -Wl,-rpath vào CGO_LDFLAGS; AIX: LIBPATH
```

## Bắt đầu nhanh

### Truy vấn bằng database/sql

```go
import (
	"database/sql"

	"github.com/chuongnx/go-ttdb/ttsql"
)

// Direct:        "DSN=sampledb;UID=app;PWD=secret"
// Client/server: "TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret"
db, err := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret;go_location=Asia/Ho_Chi_Minh")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

rows, err := db.QueryContext(ctx, "SELECT id, customer, amount FROM orders WHERE amount > ?", 100)
if err != nil {
	log.Fatal(err)
}
defer rows.Close()
for rows.Next() {
	var id int64
	var customer, amount string // NUMBER(p,s) dạng chuỗi thập phân chính xác
	rows.Scan(&id, &customer, &amount)
}
```

### Gọi PL/SQL: giá trị trả về của hàm và REF CURSOR

```go
conn, _ := db.Conn(ctx) // cursor phải được đọc trên chính kết nối đã mở nó
defer conn.Close()

var count int64
conn.ExecContext(ctx, "BEGIN ? := order_pkg.order_count; END;", sql.Out{Dest: &count})

var cur ttsql.Cursor
conn.ExecContext(ctx, "BEGIN ? := order_pkg.orders_of(?); END;", sql.Out{Dest: &cur}, "alice")
defer cur.Close()
for cur.Next() {
	var id int64
	var amount float64
	cur.Scan(&id, &amount)
}
```

### Bắt thay đổi dữ liệu với XLA

```go
r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc") // tạo mới hoặc dùng lại bookmark
if err != nil {
	log.Fatal(err)
}
defer r.Close()
r.Subscribe(ctx, "APP", "ORDERS")
for {
	recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Op.IsDML() {
			amount, _ := rec.NewValue("AMOUNT")
			fmt.Println(rec.Op, rec.QualifiedName(), rec.Key(), amount)
		}
	}
	// ghi recs xuống nơi lưu trữ bền trước, sau đó mới:
	r.Ack()
}
```

Chương trình chạy được ngay có trong [examples/sql](examples/sql) và [examples/xla-tail](examples/xla-tail).
`examples/xla-tail` in thay đổi ra dạng JSON, mỗi bản ghi một dòng.

## Tài liệu

Tài liệu chi tiết viết bằng tiếng Anh:

| Hướng dẫn | Nội dung |
|---|---|
| [Cài đặt](docs/installation.md) | Linux, macOS, AIX; biến cgo, đường dẫn thư viện, build tag |
| [Kết nối](docs/connecting.md) | Chuỗi kết nối, tuỳ chọn driver `go_*`, direct và client/server, cấu hình pool, `Connector` |
| [SQL](docs/sql-guide.md) | CRUD, transaction, prepared statement, `ExecBatch`, durable commit, huỷ câu lệnh |
| [PL/SQL](docs/plsql.md) | Package, procedure, function, tham số OUT / IN OUT mọi kiểu, REF CURSOR |
| [Kiểu dữ liệu](docs/data-types.md) | Ánh xạ kiểu TimesTen sang Go cho cột, tham số và XLA; các quy tắc về giá trị |
| [XLA](docs/xla.md) | Bookmark, đọc và ack, cấu trúc bản ghi, DDL, quản trị |
| [Production](docs/production.md) | Lỗi và retry, mất kết nối, DDL, timeout, bộ nhớ, lỗi đã biết của driver TimesTen |
| [Kiểm thử](docs/testing.md) | Dev container, các lệnh make, bộ integration test, test trên máy có TimesTen hoặc trên AIX |
| [Xử lý sự cố](docs/troubleshooting.md) | Các lỗi build và lỗi khi chạy thường gặp, cách khắc phục |

### Lưu ý quan trọng

- `""` được lưu thành NULL, theo đúng ngữ nghĩa của Oracle.
- **Không dùng CLOB cho văn bản tiếng Việt khi đọc qua client/server.** Driver client của TimesTen làm
  hỏng CLOB chứa ký tự nhiều byte khi giá trị dài hơn 4 KB. Thư viện phát hiện lỗi này và trả về
  `tt.ErrClientCLOB`. Hãy dùng NCLOB hoặc VARCHAR2 (tối đa 4 MB) thay cho CLOB.
- Trước khi `ALTER TABLE` một bảng đang được XLA subscribe, phải để mọi reader đọc hết và ack các bản
  ghi cũ. Xem [docs/xla.md](docs/xla.md#ddl-and-alter-table).

## Trạng thái

API vẫn ở giai đoạn trước 1.0 (`v0.x`), nên có thể còn thay đổi giữa các bản minor. Danh sách thay đổi
nằm trong [CHANGELOG](CHANGELOG.md). Thư viện đã được test với TimesTen 22.1.1.36 và 22.1.1.37
(Classic). TimesTen Scaleout chưa được test.

## Đóng góp

Chào đón mọi báo lỗi (issue) và pull request. Cách dựng dev container và chạy test có trong
[CONTRIBUTING.md](CONTRIBUTING.md). Lỗ hổng bảo mật xin báo theo hướng dẫn trong [SECURITY.md](SECURITY.md).

## Giấy phép

[Apache License 2.0](LICENSE). Xem thêm [NOTICE](NOTICE).

Oracle và TimesTen là nhãn hiệu đã đăng ký của Oracle và/hoặc các công ty liên kết. go-ttdb là dự án
mã nguồn mở độc lập, không liên kết với Oracle, không được Oracle bảo trợ hay hỗ trợ.
