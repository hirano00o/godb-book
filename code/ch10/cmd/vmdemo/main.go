// cmd/vmdemo/main.go
// exec パッケージのバイトコード VM の動作確認デモ。
//
// mini.db を開いて(木が空なら)users 相当のレコードを 3 件挿入したうえで、
// 手で組んだ 2 種類のプログラム
//
//   - SELECT name FROM users WHERE id = 2
//   - SELECT * FROM users(全件スキャン)
//
// をそれぞれ EXPLAIN で表示してから実行し、結果行を表示する。
package main

import (
	"fmt"
	"os"

	"minidb/btree"
	"minidb/exec"
	"minidb/pager"
	"minidb/sql"
)

func main() {
	pg, err := pager.Open("mini.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pg.Close()

	tree, err := btree.Open(pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := seedUsers(tree); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println("=== SELECT name FROM users WHERE id = 2 ===")
	pointLookup := pointLookupProgram()
	fmt.Println(exec.Explain(pointLookup))
	if err := runAndPrint(tree, pointLookup); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("=== SELECT * FROM users ===")
	fullScan := fullScanProgram()
	fmt.Println(exec.Explain(fullScan))
	if err := runAndPrint(tree, fullScan); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// seedUsers は木が空であれば、キー 1,2,3 に users 相当のレコード
// (id, name, age)を挿入する。id 列には rowid と同じ値を格納する。
func seedUsers(tree *btree.BTree) error {
	c, err := tree.NewCursor()
	if err != nil {
		return err
	}
	if c.Valid() {
		// 既にデータがある(2 回目以降の実行): 何もしない。
		return nil
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
		data, err := exec.EncodeRecord([]sql.Value{{Int: r.id}, {IsText: true, Text: r.name}, {Int: r.age}})
		if err != nil {
			return err
		}
		if err := tree.Insert(uint64(r.id), data); err != nil {
			return err
		}
	}
	return nil
}

// pointLookupProgram は「SELECT name FROM users WHERE id = 2」相当の
// プログラムを組み立てる。カーソル P1=0、レジスタ r[1]=検索キー、r[2]=結果の name。
func pointLookupProgram() []exec.Instr {
	return []exec.Instr{
		{Op: exec.OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		{Op: exec.OpOpenRead, P1: 0, Comment: "カーソルを開く"},
		{Op: exec.OpInteger, P1: 2, P2: 1, Comment: "r[1]=2"},
		{Op: exec.OpSeekRowid, P1: 0, P2: 6, P3: 1, Comment: "r[1] で Seek。一致しなければアドレス 6 へ"},
		{Op: exec.OpColumn, P1: 0, P2: 1, P3: 2, Comment: "name 列を r[2] へ"},
		{Op: exec.OpResultRow, P1: 2, P2: 1, Comment: "r[2] を 1 列の結果として出力"},
		{Op: exec.OpHalt, Comment: "終了"},
	}
}

// fullScanProgram は「SELECT * FROM users」相当の全件スキャンプログラムを組み立てる。
// カーソル P1=0、レジスタ r[1]=rowid, r[2]=id, r[3]=name, r[4]=age。
func fullScanProgram() []exec.Instr {
	return []exec.Instr{
		{Op: exec.OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		{Op: exec.OpOpenRead, P1: 0, Comment: "カーソルを開く"},
		{Op: exec.OpRewind, P1: 0, P2: 9, Comment: "先頭へ。木が空ならアドレス 9 へ"},
		{Op: exec.OpRowid, P1: 0, P2: 1, Comment: "rowid を r[1] へ"},
		{Op: exec.OpColumn, P1: 0, P2: 0, P3: 2, Comment: "id 列を r[2] へ"},
		{Op: exec.OpColumn, P1: 0, P2: 1, P3: 3, Comment: "name 列を r[3] へ"},
		{Op: exec.OpColumn, P1: 0, P2: 2, P3: 4, Comment: "age 列を r[4] へ"},
		{Op: exec.OpResultRow, P1: 1, P2: 4, Comment: "r[1..4] を 1 行として出力"},
		{Op: exec.OpNext, P1: 0, P2: 3, Comment: "次の行があればアドレス 3 へ"},
		{Op: exec.OpHalt, Comment: "終了"},
	}
}

// runAndPrint はプログラムを実行し、結果行を "| 値 | 値 | ..." の簡素な形式で表示する。
func runAndPrint(tree *btree.BTree, program []exec.Instr) error {
	vm := exec.NewVM(tree, program)
	return vm.Run(func(row []sql.Value) error {
		fmt.Print("|")
		for _, v := range row {
			if v.IsText {
				fmt.Printf(" %s |", v.Text)
			} else {
				fmt.Printf(" %d |", v.Int)
			}
		}
		fmt.Println()
		return nil
	})
}
