package exec

import (
	"errors"
	"fmt"
	"os"
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

// seedWhereTestTable は WHERE のテスト用に t(id PK, name TEXT, age INTEGER)
// へ 5 行(id は 1..5、age に重複を含む)を挿入する。
func seedWhereTestTable(t *testing.T, e *Engine) {
	t.Helper()
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	mustExec(t, e, "INSERT INTO t VALUES (1, 'Alice', 30)")
	mustExec(t, e, "INSERT INTO t VALUES (2, 'Bob', 25)")
	mustExec(t, e, "INSERT INTO t VALUES (3, 'Carol', 41)")
	mustExec(t, e, "INSERT INTO t VALUES (4, 'Dave', 25)")
	mustExec(t, e, "INSERT INTO t VALUES (5, 'Eve', 50)")
}

// rowIDs は SELECT 結果から id 列(先頭列)を取り出す(id で SELECT する前提)。
func rowIDs(res *Result) []int64 {
	ids := make([]int64, len(res.Rows))
	for i, row := range res.Rows {
		ids[i] = row[0].Int
	}
	return ids
}

func assertIntSlice(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("件数 = %d, want %d(got=%v, want=%v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v, want=%v", got, want)
		}
	}
}

// WHERE の 6 演算子すべて(INTEGER 列 age)と TEXT 列(name)の等値/不等値が、
// それぞれ正しい行集合(id の一覧)を返すことを確認する。
func TestEngineWhereAllOperators(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []int64
	}{
		{"age =", "SELECT id FROM t WHERE age = 25", []int64{2, 4}},
		{"age !=", "SELECT id FROM t WHERE age != 25", []int64{1, 3, 5}},
		{"age <", "SELECT id FROM t WHERE age < 30", []int64{2, 4}},
		{"age >", "SELECT id FROM t WHERE age > 30", []int64{3, 5}},
		{"age <=", "SELECT id FROM t WHERE age <= 30", []int64{1, 2, 4}},
		{"age >=", "SELECT id FROM t WHERE age >= 30", []int64{1, 3, 5}},
		{"name =", "SELECT id FROM t WHERE name = 'Bob'", []int64{2}},
		{"name !=", "SELECT id FROM t WHERE name != 'Bob'", []int64{1, 3, 4, 5}},
		// PK 列(id)でも Eq 以外は isPKEquality が false になり、
		// フルスキャン + フィルタのプラン(WHERE 列読み出しに OpRowid を使う経路)を通る。
		{"id >(PK, 非 Eq)", "SELECT id FROM t WHERE id > 3", []int64{4, 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := openTestEngine(t)
			seedWhereTestTable(t, e)
			res := mustExec(t, e, tc.query)
			assertIntSlice(t, rowIDs(res), tc.want)
		})
	}
}

// WHERE 列の型とリテラルの型が食い違うとコンパイルエラーになる。
func TestEngineWhereTypeMismatch(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	if _, err := e.Execute("SELECT id FROM t WHERE age = 'x'"); err == nil {
		t.Fatal("error = nil, want エラー(age は INTEGER)")
	}
	if _, err := e.Execute("SELECT id FROM t WHERE name = 1"); err == nil {
		t.Fatal("error = nil, want エラー(name は TEXT)")
	}
}

// プラン選択の白箱テスト: WHERE が「PK 列 = 整数リテラル」なら SeekRowid
// (Next を使わない)、それ以外は Next を使うフルスキャン(SeekRowid を
// 使わない)になることを EXPLAIN の出力で確認する。
func TestEngineWherePlanSelection(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	seekPlan, err := e.ExplainSQL("SELECT name FROM t WHERE id = 2")
	if err != nil {
		t.Fatalf("ExplainSQL failed: %v", err)
	}
	if !strings.Contains(seekPlan, "SeekRowid") {
		t.Fatalf("WHERE id = 2 の計画に SeekRowid が含まれません: %s", seekPlan)
	}
	if strings.Contains(seekPlan, "Next") {
		t.Fatalf("WHERE id = 2 の計画に Next が含まれています(全件スキャンになっている): %s", seekPlan)
	}

	scanPlan, err := e.ExplainSQL("SELECT name FROM t WHERE age = 30")
	if err != nil {
		t.Fatalf("ExplainSQL failed: %v", err)
	}
	if !strings.Contains(scanPlan, "Next") {
		t.Fatalf("WHERE age = 30 の計画に Next が含まれません: %s", scanPlan)
	}
	if strings.Contains(scanPlan, "SeekRowid") {
		t.Fatalf("WHERE age = 30 の計画に SeekRowid が含まれています: %s", scanPlan)
	}
}

// UPDATE: WHERE あり(対象行だけ更新され、他行は変化しない)。
func TestEngineUpdateWithWhere(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	res := mustExec(t, e, "UPDATE t SET age = 99 WHERE name = 'Bob'")
	if res.Message != "1 行を更新しました" {
		t.Fatalf("Message = %q", res.Message)
	}

	got := mustExec(t, e, "SELECT age FROM t WHERE id = 2")
	if len(got.Rows) != 1 || got.Rows[0][0].Int != 99 {
		t.Fatalf("Bob の age = %v, want 99", got.Rows)
	}
	other := mustExec(t, e, "SELECT age FROM t WHERE id = 1")
	if len(other.Rows) != 1 || other.Rows[0][0].Int != 30 {
		t.Fatalf("Alice の age が変化しています: %v", other.Rows)
	}
}

// UPDATE: WHERE なしは全行が対象になる。
func TestEngineUpdateWithoutWhere(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	res := mustExec(t, e, "UPDATE t SET age = 0")
	if res.Message != "5 行を更新しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	all := mustExec(t, e, "SELECT age FROM t")
	for _, row := range all.Rows {
		if row[0].Int != 0 {
			t.Fatalf("age が 0 になっていない行があります: %v", all.Rows)
		}
	}
}

// UPDATE: 複数列の同時代入。
func TestEngineUpdateMultipleAssignments(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	mustExec(t, e, "UPDATE t SET name = 'X', age = 1 WHERE id = 3")
	got := mustExec(t, e, "SELECT name, age FROM t WHERE id = 3")
	if len(got.Rows) != 1 || got.Rows[0][0].Text != "X" || got.Rows[0][1].Int != 1 {
		t.Fatalf("Rows = %v, want name=X age=1", got.Rows)
	}
}

// UPDATE: WHERE が PK 等値のときは SeekRowid の計画(全件スキャンなし)を
// 使うが、それでも正しく対象行だけを更新できることを確認する。
func TestEngineUpdateSeekRowidPlan(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	plan, err := e.ExplainSQL("UPDATE t SET age = 77 WHERE id = 2")
	if err != nil {
		t.Fatalf("ExplainSQL failed: %v", err)
	}
	if !strings.Contains(plan, "SeekRowid") {
		t.Fatalf("UPDATE ... WHERE id = 2 の計画に SeekRowid が含まれません: %s", plan)
	}

	res := mustExec(t, e, "UPDATE t SET age = 77 WHERE id = 2")
	if res.Message != "1 行を更新しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	got := mustExec(t, e, "SELECT name, age FROM t WHERE id = 2")
	if len(got.Rows) != 1 || got.Rows[0][0].Text != "Bob" || got.Rows[0][1].Int != 77 {
		t.Fatalf("Rows = %v, want name=Bob age=77", got.Rows)
	}
}

// DELETE: WHERE が PK 等値のときも SeekRowid の計画で正しく削除できることを確認する。
func TestEngineDeleteSeekRowidPlan(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	plan, err := e.ExplainSQL("DELETE FROM t WHERE id = 2")
	if err != nil {
		t.Fatalf("ExplainSQL failed: %v", err)
	}
	if !strings.Contains(plan, "SeekRowid") {
		t.Fatalf("DELETE ... WHERE id = 2 の計画に SeekRowid が含まれません: %s", plan)
	}

	res := mustExec(t, e, "DELETE FROM t WHERE id = 2")
	if res.Message != "1 行を削除しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	remaining := mustExec(t, e, "SELECT id FROM t")
	assertIntSlice(t, rowIDs(remaining), []int64{1, 3, 4, 5})
}

// UPDATE: PRIMARY KEY 列への代入はコンパイルエラーになる。
func TestEngineUpdatePKAssignmentError(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	_, err := e.Execute("UPDATE t SET id = 100 WHERE id = 1")
	if err == nil {
		t.Fatal("error = nil, want エラー(PK 列への UPDATE)")
	}
	if !strings.Contains(err.Error(), "PRIMARY KEY") {
		t.Fatalf("error = %v, want PRIMARY KEY に言及するエラー", err)
	}
}

// DELETE: WHERE あり(対象行だけ消える)。
func TestEngineDeleteWithWhere(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	res := mustExec(t, e, "DELETE FROM t WHERE age = 25")
	if res.Message != "2 行を削除しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	remaining := mustExec(t, e, "SELECT id FROM t")
	assertIntSlice(t, rowIDs(remaining), []int64{1, 3, 5})
}

// DELETE: WHERE なしは全行が対象になり、削除後の再 INSERT も動く。
func TestEngineDeleteWithoutWhereThenReinsert(t *testing.T) {
	e, _ := openTestEngine(t)
	seedWhereTestTable(t, e)

	res := mustExec(t, e, "DELETE FROM t")
	if res.Message != "5 行を削除しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	empty := mustExec(t, e, "SELECT id FROM t")
	if len(empty.Rows) != 0 {
		t.Fatalf("全削除後も行が残っています: %v", empty.Rows)
	}

	mustExec(t, e, "INSERT INTO t VALUES (10, 'Zoe', 20)")
	after := mustExec(t, e, "SELECT id, name FROM t")
	if len(after.Rows) != 1 || after.Rows[0][0].Int != 10 || after.Rows[0][1].Text != "Zoe" {
		t.Fatalf("再 INSERT 後の Rows = %v", after.Rows)
	}
}

// 2 相方式の統合テスト: 500 行に対する UPDATE(約半数が対象)が
// エラーなく完了し、件数・結果が正しいことを確認する。
func TestEngineUpdateManyRows(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	const n = 500
	wantUpdated := 0
	for i := 1; i <= n; i++ {
		age := i % 100
		if age < 50 {
			wantUpdated++
		}
		mustExec(t, e, fmt.Sprintf("INSERT INTO users VALUES (%d, 'user%d', %d)", i, i, age))
	}

	res := mustExec(t, e, "UPDATE users SET age = 200 WHERE age < 50")
	wantMsg := fmt.Sprintf("%d 行を更新しました", wantUpdated)
	if res.Message != wantMsg {
		t.Fatalf("Message = %q, want %q", res.Message, wantMsg)
	}

	all := mustExec(t, e, "SELECT id, age FROM users")
	if len(all.Rows) != n {
		t.Fatalf("Rows 件数 = %d, want %d", len(all.Rows), n)
	}
	got200 := 0
	for _, row := range all.Rows {
		id := int(row[0].Int)
		wantOld := id % 100
		if wantOld < 50 {
			got200++
			if row[1].Int != 200 {
				t.Fatalf("id=%d の age = %d, want 200", id, row[1].Int)
			}
		} else if row[1].Int != int64(wantOld) {
			t.Fatalf("id=%d の age = %d, want %d(更新対象外)", id, row[1].Int, wantOld)
		}
	}
	if got200 != wantUpdated {
		t.Fatalf("age=200 の件数 = %d, want %d", got200, wantUpdated)
	}
}

// 2 相方式の統合テスト: 500 行に対する DELETE(約半数が対象)が
// エラーなく完了し、件数・結果が正しいことを確認する。
func TestEngineDeleteManyRows(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	const n = 500
	wantDeleted := 0
	for i := 1; i <= n; i++ {
		age := i % 100
		if age < 50 {
			wantDeleted++
		}
		mustExec(t, e, fmt.Sprintf("INSERT INTO users VALUES (%d, 'user%d', %d)", i, i, age))
	}

	res := mustExec(t, e, "DELETE FROM users WHERE age < 50")
	wantMsg := fmt.Sprintf("%d 行を削除しました", wantDeleted)
	if res.Message != wantMsg {
		t.Fatalf("Message = %q, want %q", res.Message, wantMsg)
	}

	all := mustExec(t, e, "SELECT id, age FROM users")
	if len(all.Rows) != n-wantDeleted {
		t.Fatalf("Rows 件数 = %d, want %d", len(all.Rows), n-wantDeleted)
	}
	for _, row := range all.Rows {
		if row[1].Int < 50 {
			t.Fatalf("削除されるべき行が残っています: id=%d age=%d", row[0].Int, row[1].Int)
		}
	}
}

// UPDATE/DELETE の後にファイルを閉じて再オープンしても結果が保たれる
// (2 相の書き換えフェーズで木の再配置がカタログへ正しく反映される)ことを確認する。
func TestEngineUpdateDeleteAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(pg)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")

	const n = 300
	for i := 1; i <= n; i++ {
		mustExec(t, e, fmt.Sprintf("INSERT INTO t VALUES (%d, 'row%d', %d)", i, i, i%100))
	}
	mustExec(t, e, "UPDATE t SET age = 1000 WHERE age >= 50")
	mustExec(t, e, "DELETE FROM t WHERE age < 10")

	wantRows := mustExec(t, e, "SELECT id, age FROM t")
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg2.Close() })
	e2, err := NewEngine(pg2)
	if err != nil {
		t.Fatal(err)
	}
	gotRows := mustExec(t, e2, "SELECT id, age FROM t")

	if len(gotRows.Rows) != len(wantRows.Rows) {
		t.Fatalf("再オープン後の件数 = %d, want %d", len(gotRows.Rows), len(wantRows.Rows))
	}
	for i := range wantRows.Rows {
		if gotRows.Rows[i][0].Int != wantRows.Rows[i][0].Int || gotRows.Rows[i][1].Int != wantRows.Rows[i][1].Int {
			t.Fatalf("Rows[%d] = %v, want %v", i, gotRows.Rows[i], wantRows.Rows[i])
		}
	}
}

// autocommit(明示 BEGIN なし)で実行した 1 文が、Close を待たずその場で
// ディスクへ確定していることをファイルレベルで確認する:
//  1. 実行直後にジャーナルファイルが残っていないこと
//  2. 元の Pager とは別の Pager で同じ DB ファイルを開いても
//     (= ディスクの内容だけで)INSERT した行が読めること
func TestEngineAutocommitDurability(t *testing.T) {
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

	mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")
	mustExec(t, e, "INSERT INTO users VALUES (1, 'Alice')")

	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Fatalf("autocommit 直後にジャーナルファイルが残っています: err=%v", err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg2.Close()
	e2, err := NewEngine(pg2)
	if err != nil {
		t.Fatal(err)
	}
	res := mustExec(t, e2, "SELECT * FROM users")
	if len(res.Rows) != 1 || res.Rows[0][1].Text != "Alice" {
		t.Fatalf("別 Pager からの SELECT 結果 = %v, want Alice の 1 行", res.Rows)
	}
}

// BEGIN ... ROLLBACK は複数文にまたがる変更をまとめて破棄し、
// BEGIN ... COMMIT はまとめて確定することを確認する。
func TestEngineExplicitTx(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)")

	mustExec(t, e, "BEGIN")
	mustExec(t, e, "INSERT INTO users VALUES (1, 'Alice')")
	mustExec(t, e, "INSERT INTO users VALUES (2, 'Bob')")
	mustExec(t, e, "ROLLBACK")

	res := mustExec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 0 {
		t.Fatalf("ROLLBACK 後の行数 = %d, want 0: %v", len(res.Rows), res.Rows)
	}

	mustExec(t, e, "BEGIN")
	mustExec(t, e, "INSERT INTO users VALUES (1, 'Alice')")
	mustExec(t, e, "INSERT INTO users VALUES (2, 'Bob')")
	mustExec(t, e, "COMMIT")

	res = mustExec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 2 {
		t.Fatalf("COMMIT 後の行数 = %d, want 2: %v", len(res.Rows), res.Rows)
	}
}

// トランザクションを開始せずに COMMIT/ROLLBACK することと、
// 既に開始済みの状態でさらに BEGIN することは、いずれもエラーになる。
func TestEngineTxErrors(t *testing.T) {
	e, _ := openTestEngine(t)

	if _, err := e.Execute("COMMIT"); err == nil {
		t.Fatal("error = nil, want エラー(トランザクションを開始せず COMMIT)")
	}
	if _, err := e.Execute("ROLLBACK"); err == nil {
		t.Fatal("error = nil, want エラー(トランザクションを開始せず ROLLBACK)")
	}

	mustExec(t, e, "BEGIN")
	if _, err := e.Execute("BEGIN"); err == nil {
		t.Fatal("error = nil, want エラー(二重 BEGIN)")
	}
	mustExec(t, e, "ROLLBACK") // 後始末
}

// PRAGMA journal_mode は、値なしなら現在のモードを、値ありならモードを
// 切り替えて確認メッセージを返す。既定は rollback(delete)。
func TestEnginePragmaJournalMode(t *testing.T) {
	e, pg := openTestEngine(t)

	res := mustExec(t, e, "PRAGMA journal_mode")
	if res.Message != "journal_mode = delete" {
		t.Fatalf("Message = %q, want %q", res.Message, "journal_mode = delete")
	}
	if pg.JournalMode() != pager.JournalModeRollback {
		t.Fatalf("JournalMode() = %v, want %v", pg.JournalMode(), pager.JournalModeRollback)
	}

	res = mustExec(t, e, "PRAGMA journal_mode = WAL")
	if res.Message != "journal_mode を wal に変更しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	if pg.JournalMode() != pager.JournalModeWAL {
		t.Fatalf("JournalMode() = %v, want %v", pg.JournalMode(), pager.JournalModeWAL)
	}

	res = mustExec(t, e, "PRAGMA journal_mode")
	if res.Message != "journal_mode = wal" {
		t.Fatalf("Message = %q, want %q", res.Message, "journal_mode = wal")
	}

	res = mustExec(t, e, "PRAGMA journal_mode = DELETE")
	if res.Message != "journal_mode を delete に変更しました" {
		t.Fatalf("Message = %q", res.Message)
	}
	if pg.JournalMode() != pager.JournalModeRollback {
		t.Fatalf("JournalMode() = %v, want %v", pg.JournalMode(), pager.JournalModeRollback)
	}
}

// 未対応の PRAGMA 名や journal_mode の未知の値、値を取らない
// wal_checkpoint への値指定は、いずれもエラーになる。
func TestEnginePragmaErrors(t *testing.T) {
	e, _ := openTestEngine(t)

	if _, err := e.Execute("PRAGMA no_such_pragma"); err == nil {
		t.Fatal("error = nil, want エラー(未対応の PRAGMA)")
	}
	if _, err := e.Execute("PRAGMA journal_mode = FOO"); err == nil {
		t.Fatal("error = nil, want エラー(未対応の journal_mode)")
	}
	if _, err := e.Execute("PRAGMA wal_checkpoint = WAL"); err == nil {
		t.Fatal("error = nil, want エラー(wal_checkpoint は値を取らない)")
	}
}

// rollback モードで PRAGMA wal_checkpoint を実行するとエラーになる。
func TestEnginePragmaCheckpointRequiresWAL(t *testing.T) {
	e, _ := openTestEngine(t)

	if _, err := e.Execute("PRAGMA wal_checkpoint"); err == nil {
		t.Fatal("error = nil, want エラー(rollback モードでのチェックポイントは不可)")
	}
}

// PRAGMA wal_checkpoint は WAL モードでチェックポイントを実行し、
// 溜まっていた変更を DB 本体へ反映する。
func TestEnginePragmaCheckpoint(t *testing.T) {
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

	mustExec(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	mustExec(t, e, "PRAGMA journal_mode = WAL")
	mustExec(t, e, "INSERT INTO t VALUES (1, 'Alice')")

	res := mustExec(t, e, "PRAGMA wal_checkpoint")
	if res.Message != "チェックポイントを実行しました" {
		t.Fatalf("Message = %q", res.Message)
	}

	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 16 {
		t.Fatalf("チェックポイント後の WAL サイズ = %d, want 16(ヘッダのみ)", info.Size())
	}
}

// WAL モードでも CRUD が一連の流れとして正しく動くことを確認する
// (既存の TestEngineCreateInsertSelect と同じシナリオを WAL で 1 本)。
func TestEngineCRUDInWALMode(t *testing.T) {
	e, _ := openTestEngine(t)
	mustExec(t, e, "PRAGMA journal_mode = WAL")

	mustExec(t, e, "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
	mustExec(t, e, "INSERT INTO users VALUES (1, 'Alice', 30)")
	mustExec(t, e, "INSERT INTO users VALUES (2, 'Bob', 25)")

	res := mustExec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 2 {
		t.Fatalf("Rows 件数 = %d, want 2: %v", len(res.Rows), res.Rows)
	}

	mustExec(t, e, "UPDATE users SET age = 31 WHERE id = 1")
	res = mustExec(t, e, "SELECT age FROM users WHERE id = 1")
	if len(res.Rows) != 1 || res.Rows[0][0].Int != 31 {
		t.Fatalf("UPDATE 後の age = %v, want 31", res.Rows)
	}

	mustExec(t, e, "DELETE FROM users WHERE id = 2")
	res = mustExec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 1 {
		t.Fatalf("DELETE 後の行数 = %d, want 1: %v", len(res.Rows), res.Rows)
	}

	mustExec(t, e, "BEGIN")
	mustExec(t, e, "INSERT INTO users VALUES (3, 'Carol', 41)")
	mustExec(t, e, "ROLLBACK")
	res = mustExec(t, e, "SELECT * FROM users")
	if len(res.Rows) != 1 {
		t.Fatalf("ROLLBACK 後の行数 = %d, want 1: %v", len(res.Rows), res.Rows)
	}
}
