package exec

import (
	"path/filepath"
	"testing"

	"minidb/btree"
	"minidb/pager"
	"minidb/sql"
)

// openTestTree はテスト用の一時ディレクトリに db を作り、
// 3 件のレコード(id, name, age)を挿入した btree を返す。
// キー 1,2,3 に (1,"Alice",30) (2,"Bob",25) (3,"Carol",41) を格納する。
func openTestTree(t *testing.T) *btree.BTree {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })

	tree, err := btree.Open(pg)
	if err != nil {
		t.Fatal(err)
	}

	rows := []struct {
		id   int64
		name string
		age  int64
	}{
		{1, "Alice", 30},
		{2, "Bob", 25},
		{3, "Carol", 41},
	}
	for _, r := range rows {
		data, err := EncodeRecord([]sql.Value{{Int: r.id}, {IsText: true, Text: r.name}, {Int: r.age}})
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Insert(uint64(r.id), data); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

// fullScanProgram は「SELECT * FROM users」相当の全件スキャンプログラムを組み立てる。
// レジスタ 1=rowid, 2=id, 3=name, 4=age として ResultRow(1,4) で 4 列を出力する。
func fullScanProgram() []Instr {
	return []Instr{
		{Op: OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		{Op: OpOpenRead, P1: 0, Comment: "カーソルを開く"},
		{Op: OpRewind, P1: 0, P2: 9, Comment: "先頭へ。空ならアドレス 9 へ"},
		{Op: OpRowid, P1: 0, P2: 1, Comment: "rowid を r[1] へ"},
		{Op: OpColumn, P1: 0, P2: 0, P3: 2, Comment: "id 列を r[2] へ"},
		{Op: OpColumn, P1: 0, P2: 1, P3: 3, Comment: "name 列を r[3] へ"},
		{Op: OpColumn, P1: 0, P2: 2, P3: 4, Comment: "age 列を r[4] へ"},
		{Op: OpResultRow, P1: 1, P2: 4, Comment: "4 列を出力"},
		{Op: OpNext, P1: 0, P2: 3, Comment: "次の行があればアドレス 3 へ"},
		{Op: OpHalt, Comment: "終了"},
	}
}

// 全件スキャンのプログラムで 3 行が rowid 昇順に出ることを確認する。
func TestVMFullScan(t *testing.T) {
	tree := openTestTree(t)
	program := fullScanProgram()

	vm := NewVM(tree, program)
	var rows [][]sql.Value
	if err := vm.Run(func(row []sql.Value) error {
		rows = append(rows, append([]sql.Value(nil), row...))
		return nil
	}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("出力行数 = %d, want 3", len(rows))
	}
	wantIDs := []int64{1, 2, 3}
	for i, row := range rows {
		if len(row) != 4 {
			t.Fatalf("row[%d] の列数 = %d, want 4", i, len(row))
		}
		if row[1].Int != wantIDs[i] {
			t.Fatalf("row[%d].id = %d, want %d", i, row[1].Int, wantIDs[i])
		}
	}
}

// SeekRowid で完全一致するプログラムは 1 行だけを返す。
func TestVMSeekRowidHit(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpOpenRead, P1: 0},
		{Op: OpInteger, P1: 2, P2: 1, Comment: "r[1]=2"},
		{Op: OpSeekRowid, P1: 0, P2: 6, P3: 1, Comment: "r[1] で Seek。ミスならアドレス 6 へ"},
		{Op: OpColumn, P1: 0, P2: 1, P3: 2, Comment: "name 列を r[2] へ"},
		{Op: OpResultRow, P1: 2, P2: 1},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	var rows [][]sql.Value
	if err := vm.Run(func(row []sql.Value) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("出力行数 = %d, want 1", len(rows))
	}
	if rows[0][0].Text != "Bob" {
		t.Fatalf("name = %q, want %q", rows[0][0].Text, "Bob")
	}
}

// SeekRowid で一致しない場合は 0 行(エラーにはならない)。
func TestVMSeekRowidMiss(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpOpenRead, P1: 0},
		{Op: OpInteger, P1: 999, P2: 1, Comment: "r[1]=999"},
		{Op: OpSeekRowid, P1: 0, P2: 6, P3: 1},
		{Op: OpColumn, P1: 0, P2: 1, P3: 2},
		{Op: OpResultRow, P1: 2, P2: 1},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	var rows [][]sql.Value
	if err := vm.Run(func(row []sql.Value) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("出力行数 = %d, want 0", len(rows))
	}
}

// 比較命令によるフィルタ(age > 26)が正しい行だけを出す。
// ループ内で Column を r[5] に読み、r[5] <= 26 なら OpNext へスキップする
// (Le が成立したら P2=次のループ末尾へジャンプ、成立しなければ結果を出力)。
func TestVMFilterByComparison(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpOpenRead, P1: 0},
		{Op: OpRewind, P1: 0, P2: 9},
		{Op: OpColumn, P1: 0, P2: 2, P3: 5, Comment: "age 列を r[5] へ"},
		{Op: OpInteger, P1: 26, P2: 6, Comment: "r[6]=26"},
		{Op: OpLe, P1: 5, P2: 8, P3: 6, Comment: "r[5] <= r[6] ならスキップ"},
		{Op: OpColumn, P1: 0, P2: 1, P3: 2, Comment: "name 列を r[2] へ"},
		{Op: OpResultRow, P1: 2, P2: 1},
		{Op: OpNext, P1: 0, P2: 3},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	var names []string
	if err := vm.Run(func(row []sql.Value) error {
		names = append(names, row[0].Text)
		return nil
	}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	want := []string{"Alice", "Carol"}
	if len(names) != len(want) {
		t.Fatalf("出力行数 = %d, want %d (%v)", len(names), len(want), names)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("names[%d] = %q, want %q", i, names[i], name)
		}
	}
}

// 範囲外レジスタを指す命令はエラーになる。
func TestVMOutOfRangeRegister(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpInteger, P1: 1, P2: 999, Comment: "範囲外レジスタへ書き込む"},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	if err := vm.Run(func(row []sql.Value) error { return nil }); err == nil {
		t.Fatal("Run error = nil, want エラー")
	}
}

// Valid でないカーソルへの Column はエラーになる。
func TestVMColumnOnInvalidCursor(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpOpenRead, P1: 0},
		{Op: OpInteger, P1: 999, P2: 1},
		{Op: OpSeekRowid, P1: 0, P2: 4, P3: 1, Comment: "存在しないキーなのでミスし r[1] を通過"},
		{Op: OpColumn, P1: 0, P2: 0, P3: 2, Comment: "無効なカーソルから読もうとする"},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	if err := vm.Run(func(row []sql.Value) error { return nil }); err == nil {
		t.Fatal("Run error = nil, want エラー")
	}
}

// 型が食い違う比較(INTEGER と TEXT)はエラーになる。
func TestVMCompareTypeMismatch(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpInit, P2: 1},
		{Op: OpInteger, P1: 1, P2: 1},
		{Op: OpString, P1: 2, P4: "x"},
		{Op: OpEq, P1: 1, P2: 4, P3: 2},
		{Op: OpHalt},
	}

	vm := NewVM(tree, program)
	if err := vm.Run(func(row []sql.Value) error { return nil }); err == nil {
		t.Fatal("Run error = nil, want エラー")
	}
}

// Goto による自己ループはステップ数上限に達してエラーになる。
func TestVMStepLimit(t *testing.T) {
	tree := openTestTree(t)
	program := []Instr{
		{Op: OpGoto, P2: 0, Comment: "自分自身へ無限ループ"},
	}

	vm := NewVM(tree, program)
	err := vm.Run(func(row []sql.Value) error { return nil })
	if err == nil {
		t.Fatal("Run error = nil, want ステップ数上限エラー")
	}
}
