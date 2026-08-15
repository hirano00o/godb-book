// engine.go は SQL 文字列を受け取り、解析・コンパイル・実行までを
// 束ねる Engine を提供する。REPL(cmd/minidb)はこの Engine を通してのみ
// データベースを操作する。
package exec

import (
	"fmt"

	"minidb/btree"
	"minidb/pager"
	"minidb/sql"
)

// Result は Execute の実行結果。結果行を持つ文(SELECT)では Columns と
// Rows が埋まり、それ以外(CREATE TABLE / INSERT)では Message が埋まる。
type Result struct {
	Columns []string
	Rows    [][]sql.Value
	Message string
}

// Engine は pg 上のデータベースに対して SQL を実行する。
type Engine struct {
	pg  *pager.Pager
	cat *Catalog
}

// NewEngine は pg 上のカタログを開いて Engine を作る。
func NewEngine(pg *pager.Pager) (*Engine, error) {
	cat, err := OpenCatalog(pg)
	if err != nil {
		return nil, err
	}
	return &Engine{pg: pg, cat: cat}, nil
}

// Execute は SQL 文字列 1 文を解析・実行する。
//
// CREATE TABLE だけは特別扱いで、バイトコードを経由せず Catalog を
// 直接呼び出す。本物の SQLite は CREATE TABLE も内部的にはバイトコード
// (sqlite_schema への INSERT など)で実行するが、minidb では
// スキーマ操作は VM を通さないと割り切り、実装を単純にしている。
func (e *Engine) Execute(input string) (*Result, error) {
	stmt, err := sql.Parse(input)
	if err != nil {
		return nil, err
	}

	if create, ok := stmt.(*sql.CreateTableStmt); ok {
		if err := e.cat.CreateTable(create, input); err != nil {
			return nil, err
		}
		return &Result{Message: fmt.Sprintf("テーブル %s を作成しました", create.Table)}, nil
	}

	program, err := Compile(stmt, e.cat)
	if err != nil {
		return nil, err
	}

	vm := NewVM(e.pg, program)
	var rows [][]sql.Value
	if err := vm.Run(func(row []sql.Value) error {
		rows = append(rows, append([]sql.Value(nil), row...))
		return nil
	}); err != nil {
		return nil, err
	}

	if err := e.applyRootMoves(vm.RootMoves()); err != nil {
		return nil, err
	}

	switch s := stmt.(type) {
	case *sql.InsertStmt:
		return &Result{Message: "1 行を挿入しました"}, nil
	case *sql.SelectStmt:
		info, err := e.cat.Get(s.Table)
		if err != nil {
			return nil, err
		}
		columns := s.Columns
		if s.Star {
			columns = make([]string, len(info.Columns))
			for i, col := range info.Columns {
				columns[i] = col.Name
			}
		}
		return &Result{Columns: columns, Rows: rows}, nil
	case *sql.UpdateStmt:
		return e.executeUpdate(s, rows)
	case *sql.DeleteStmt:
		return e.executeDelete(s, rows)
	default:
		return nil, fmt.Errorf("未対応の文です: %T", stmt)
	}
}

// executeUpdate は UPDATE の書き換えフェーズ(2 相方式の第 2 相)。
// rows は探索フェーズ(compileUpdate が生成したバイトコード)が
// ResultRow で列挙した「rowid + PK を除く全列の現在値」の一覧。
//
// カーソルで走査しながら同じ木を書き換えない理由は 2 つある:
//  1. カーソルの契約上、カーソル使用中の木への Insert/Delete は未定義
//     (第 10 章 cursor.go の前提)。
//  2. 走査しながら書き換えると、書き換えた行を走査が再訪してしまう
//     おそれがある(Halloween problem として知られる古典的な問題)。
//
// そのため探索を先に完了させ(バイトコード)、対象行を確定させてから
// 改めて Engine が直接 btree を操作する。
func (e *Engine) executeUpdate(stmt *sql.UpdateStmt, rows [][]sql.Value) (*Result, error) {
	info, err := e.cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	tree := btree.OpenAt(e.pg, info.Root)
	beforeRoot := tree.Root()

	for _, row := range rows {
		rowid := uint64(row[0].Int)
		newValues := applyAssignments(info, row[1:], stmt.Set)
		data, err := EncodeRecord(newValues)
		if err != nil {
			return nil, err
		}
		if err := tree.Delete(rowid); err != nil {
			return nil, err
		}
		if err := tree.Insert(rowid, data); err != nil {
			return nil, err
		}
	}

	if afterRoot := tree.Root(); afterRoot != beforeRoot {
		if err := e.cat.UpdateRoot(info, afterRoot); err != nil {
			return nil, err
		}
	}
	return &Result{Message: fmt.Sprintf("%d 行を更新しました", len(rows))}, nil
}

// executeDelete は DELETE の書き換えフェーズ(2 相方式の第 2 相)。
// rows は探索フェーズが列挙した対象行の rowid の一覧(1 列のみ)。
// 方針は executeUpdate と同じ(コメント参照)。
func (e *Engine) executeDelete(stmt *sql.DeleteStmt, rows [][]sql.Value) (*Result, error) {
	info, err := e.cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	tree := btree.OpenAt(e.pg, info.Root)
	beforeRoot := tree.Root()

	for _, row := range rows {
		rowid := uint64(row[0].Int)
		if err := tree.Delete(rowid); err != nil {
			return nil, err
		}
	}

	if afterRoot := tree.Root(); afterRoot != beforeRoot {
		if err := e.cat.UpdateRoot(info, afterRoot); err != nil {
			return nil, err
		}
	}
	return &Result{Message: fmt.Sprintf("%d 行を削除しました", len(rows))}, nil
}

// applyAssignments は UPDATE の SET 句を、探索フェーズが読み出した
// 「PK を除く全列の現在値」(current、info.Columns の PK を除いた順)に
// 適用し、書き換え後の値の並びを返す。current 自体は書き換えない。
func applyAssignments(info *TableInfo, current []sql.Value, assigns []sql.Assignment) []sql.Value {
	newValues := append([]sql.Value(nil), current...)
	idx := 0
	for i, col := range info.Columns {
		if i == info.PKIndex {
			continue
		}
		for _, a := range assigns {
			if a.Column == col.Name {
				newValues[idx] = a.Value
			}
		}
		idx++
	}
	return newValues
}

// applyRootMoves は VM の実行中にルートが動いたテーブルをカタログへ反映する。
// テーブルへの書き込みで木が分割・収縮しルートページが変わっても、
// カタログの記録を更新しない限り、次回そのテーブルを開いたときに
// 古いルートを参照してしまう(木の残りの大部分を見失う)ため必須の後始末。
func (e *Engine) applyRootMoves(moves map[string]pager.PageID) error {
	for name, newRoot := range moves {
		info, err := e.cat.Get(name)
		if err != nil {
			return err
		}
		if err := e.cat.UpdateRoot(info, newRoot); err != nil {
			return err
		}
	}
	return nil
}

// ExplainSQL は SQL をコンパイルした命令列の EXPLAIN 表を返す(REPL の .explain 用)。
func (e *Engine) ExplainSQL(input string) (string, error) {
	stmt, err := sql.Parse(input)
	if err != nil {
		return "", err
	}
	if _, ok := stmt.(*sql.CreateTableStmt); ok {
		return "", fmt.Errorf("CREATE TABLE はバイトコードを経由しないため EXPLAIN できません")
	}
	program, err := Compile(stmt, e.cat)
	if err != nil {
		return "", err
	}
	return Explain(program), nil
}

// Tables はカタログに登録された全テーブル名を ID 順に返す(.tables 用)。
func (e *Engine) Tables() ([]string, error) {
	infos, err := e.cat.List()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	return names, nil
}
