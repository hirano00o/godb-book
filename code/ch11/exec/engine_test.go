package exec

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"minidb/btree"
	"minidb/pager"
)

// openTestEngine はテスト用の一時ディレクトリに db を作り、Engine を開く。
func openTestEngine(t *testing.T) (*Engine, *pager.Pager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })

	e, err := NewEngine(pg)
	if err != nil {
		t.Fatal(err)
	}
	return e, pg
}

// mustExec は Execute を呼び、エラーがあればテストを失敗させる。
func mustExec(t *testing.T, e *Engine, sqlText string) *Result {
	t.Helper()
	res, err := e.Execute(sqlText)
	if err != nil {
		t.Fatalf("Execute(%q) failed: %v", sqlText, err)
	}
	return res
}

// CREATE → INSERT → SELECT の一連の流れが動くことを確認する。
func TestEngineCreateInsertSelect(t *testing.T) {
	e, _ := openTestEngine(t)

	res := mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	if res.Message != "テーブル users を作成しました" {
		t.Fatalf("Message = %q", res.Message)
	}

	res = mustExec(t, e, "INSERT INTO users VALUES (1, 'Alice', 30)")
	if res.Message != "1 行を挿入しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	mustExec(t, e, "INSERT INTO users VALUES (2, 'Bob', 25)")
	mustExec(t, e, "INSERT INTO users VALUES (3, 'Carol', 41)")

	res = mustExec(t, e, "SELECT * FROM users")
	wantCols := []string{"id", "name", "age"}
	if len(res.Columns) != len(wantCols) {
		t.Fatalf("Columns = %v, want %v", res.Columns, wantCols)
	}
	for i, c := range wantCols {
		if res.Columns[i] != c {
			t.Fatalf("Columns[%d] = %q, want %q", i, res.Columns[i], c)
		}
	}
	if len(res.Rows) != 3 {
		t.Fatalf("Rows 件数 = %d, want 3", len(res.Rows))
	}
	if res.Rows[0][1].Text != "Alice" || res.Rows[1][1].Text != "Bob" || res.Rows[2][1].Text != "Carol" {
		t.Fatalf("Rows = %v", res.Rows)
	}

	res = mustExec(t, e, "SELECT name FROM users")
	if len(res.Columns) != 1 || res.Columns[0] != "name" {
		t.Fatalf("Columns = %v", res.Columns)
	}
	if len(res.Rows) != 3 || res.Rows[0][0].Text != "Alice" {
		t.Fatalf("Rows = %v", res.Rows)
	}
}

// PK を指定した INSERT では、その値がそのまま rowid(木のキー)として
// 使われることを白箱で確認する。
func TestEngineInsertWithPKUsesValueAsRowid(t *testing.T) {
	e, pg := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	mustExec(t, e, "INSERT INTO t VALUES (42, 'x')")

	info, err := e.cat.Get("t")
	if err != nil {
		t.Fatal(err)
	}
	tree := btree.OpenAt(pg, info.Root)
	if _, err := tree.Search(42); err != nil {
		t.Fatalf("Search(42) failed: %v(rowid が PK 値と一致していない)", err)
	}
}

// PK のないテーブルへの INSERT は rowid を 1 から自動採番する。
func TestEngineInsertWithoutPKAutoIncrements(t *testing.T) {
	e, pg := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (name TEXT, age INTEGER)")
	mustExec(t, e, "INSERT INTO t VALUES ('a', 1)")
	mustExec(t, e, "INSERT INTO t VALUES ('b', 2)")
	mustExec(t, e, "INSERT INTO t VALUES ('c', 3)")

	info, err := e.cat.Get("t")
	if err != nil {
		t.Fatal(err)
	}
	tree := btree.OpenAt(pg, info.Root)
	for _, key := range []uint64{1, 2, 3} {
		if _, err := tree.Search(key); err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
	}
}

// 存在しないテーブルへの SELECT/INSERT はエラーになる。
func TestEngineTableNotFound(t *testing.T) {
	e, _ := openTestEngine(t)
	if _, err := e.Execute("SELECT * FROM nope"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("error = %v, want ErrTableNotFound", err)
	}
	if _, err := e.Execute("INSERT INTO nope VALUES (1)"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("error = %v, want ErrTableNotFound", err)
	}
}

// 存在しない列名を SELECT するとコンパイルエラーになる。
func TestEngineColumnNotFound(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	if _, err := e.Execute("SELECT nope FROM t"); err == nil {
		t.Fatal("error = nil, want エラー")
	}
}

// 列の型と食い違う値の INSERT はコンパイルエラーになる。
func TestEngineInsertTypeMismatch(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	if _, err := e.Execute("INSERT INTO t VALUES (1, 2)"); err == nil {
		t.Fatal("error = nil, want エラー(name は TEXT)")
	}
}

// 値の数が列数と一致しない INSERT はコンパイルエラーになる。
func TestEngineInsertValueCountMismatch(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	if _, err := e.Execute("INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("error = nil, want エラー(値が 1 個しかない)")
	}
}

// 同名テーブルの CREATE TABLE はエラーになる。
func TestEngineCreateTableDuplicate(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if _, err := e.Execute("CREATE TABLE t (id INTEGER PRIMARY KEY)"); !errors.Is(err, ErrTableExists) {
		t.Fatalf("error = %v, want ErrTableExists", err)
	}
}

// 大量 INSERT(500 行)で木の分割が起きても、カタログのルート追跡
// (UpdateRoot)が働き、Engine を再オープンした後も SELECT で全件読める
// ことを確認する(UpdateRoot の統合テスト)。
func TestEngineManyInsertsTracksRootAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(pg)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")

	const n = 500
	for i := 1; i <= n; i++ {
		stmt := fmt.Sprintf("INSERT INTO t VALUES (%d, 'row%d')", i, i)
		mustExec(t, e, stmt)
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	// 再オープンして SELECT * が全件読めることを確認する。
	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg2.Close() })
	e2, err := NewEngine(pg2)
	if err != nil {
		t.Fatal(err)
	}

	res := mustExec(t, e2, "SELECT * FROM t")
	if len(res.Rows) != n {
		t.Fatalf("Rows 件数 = %d, want %d", len(res.Rows), n)
	}
	for i, row := range res.Rows {
		wantName := fmt.Sprintf("row%d", i+1)
		if row[1].Text != wantName {
			t.Fatalf("Rows[%d].name = %q, want %q", i, row[1].Text, wantName)
		}
	}
}

// ExplainSQL は EXPLAIN 表(ヘッダ行を含む)を返す。
func TestEngineExplainSQL(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")

	out, err := e.ExplainSQL("SELECT name FROM t")
	if err != nil {
		t.Fatalf("ExplainSQL failed: %v", err)
	}
	if !strings.Contains(out, "OpenRead") {
		t.Fatalf("ExplainSQL 出力に OpenRead が含まれません: %s", out)
	}
}
