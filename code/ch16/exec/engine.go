// engine.go は SQL 文字列を受け取り、解析・コンパイル・実行までを
// 束ねる Engine を提供する。REPL(cmd/minidb)はこの Engine を通してのみ
// データベースを操作する。
package exec

import (
	"errors"
	"fmt"
	"strings"

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
//
// explicitTx は BEGIN 文で開始され COMMIT/ROLLBACK まで続く「明示
// トランザクション」中かどうかを表す。明示トランザクション中でない
// 文は autocommit(1 文 = 1 トランザクション)として実行される。
type Engine struct {
	pg         *pager.Pager
	cat        *Catalog
	explicitTx bool
}

// NewEngine は pg 上のカタログを開いて Engine を作る。
//
// OpenCatalog は、カタログの木がまだ存在しなければルートページを新規に
// 確保して dirty にする。これは BEGIN されたトランザクションの外側で
// 起きる Engine 構築時の一度きりの初期化なので、ここで確定させておく。
// そうしないと、以降 autocommit や明示トランザクションが Rollback した
// ときにこの初期化まで巻き戻ってしまい、カタログのルートページを
// 見失う(コミット時ジャーナリングは「トランザクション開始時点で
// dirty ページが 1 枚もない」ことを前提にしている)。
func NewEngine(pg *pager.Pager) (*Engine, error) {
	cat, err := OpenCatalog(pg)
	if err != nil {
		return nil, err
	}
	if err := pg.Flush(); err != nil {
		return nil, err
	}
	return &Engine{pg: pg, cat: cat}, nil
}

// Execute は SQL 文字列 1 文を解析・実行する。
//
// BEGIN/COMMIT/ROLLBACK はトランザクション制御そのものなので、ここで
// 直接処理する。それ以外の文は、明示トランザクション中(BEGIN 済み)
// ならそのまま execStmt に渡し、そうでなければ「この 1 文だけの
// トランザクション」(autocommit)として pg.Begin/Commit/Rollback で
// 包んで実行する。これにより「1 文 = 1 トランザクション」がデフォルトになる。
func (e *Engine) Execute(input string) (*Result, error) {
	stmt, err := sql.Parse(input)
	if err != nil {
		return nil, err
	}

	switch s := stmt.(type) {
	case *sql.BeginStmt:
		return e.executeBegin()
	case *sql.CommitStmt:
		return e.executeCommit()
	case *sql.RollbackStmt:
		return e.executeRollback()
	case *sql.PragmaStmt:
		// PRAGMA はトランザクション制御と同様、autocommit の対象外
		// (Tx を張らずに直接実行する。SetJournalMode/Checkpoint 自身が
		// 完結した操作なので、包む意味がない)。
		return e.executePragma(s)
	}

	if e.explicitTx {
		return e.execStmt(stmt, input)
	}
	return e.autocommit(stmt, input)
}

// autocommit は stmt を「この 1 文だけのトランザクション」として実行する。
// 実行または Commit が失敗したときは Rollback するが、呼び出し元にとって
// 重要なのは「なぜ失敗したか」なので、Rollback 自体のエラーより元のエラーを
// 優先して返す。
//
// Commit の失敗(例えば ErrBusy)でも Rollback するのは、明示トランザクション
// (BEGIN...COMMIT)とは事情が異なるため。明示トランザクションなら
// pg.InTx() が真のまま残り、利用者は改めて COMMIT を打って再試行するか
// ROLLBACK を選べる。しかし autocommit の場合、利用者に見えている操作は
// 「元の SQL 文を再実行する」ことだけであり、そこには Begin が含まれる。
// ここで Rollback せずにトランザクションを生かしたままにすると、次の
// autocommit の Begin が「トランザクションは既に開始しています」に
// 阻まれ、REPL がそれ以降ずっと行き詰まってしまう。
func (e *Engine) autocommit(stmt sql.Statement, input string) (*Result, error) {
	if err := e.pg.Begin(); err != nil {
		return nil, err
	}
	res, err := e.execStmt(stmt, input)
	if err != nil {
		_ = e.pg.Rollback()
		return nil, err
	}
	if err := e.pg.Commit(); err != nil {
		_ = e.pg.Rollback()
		return nil, err
	}
	return res, nil
}

// executeBegin は明示トランザクションを開始する。
func (e *Engine) executeBegin() (*Result, error) {
	if e.explicitTx {
		return nil, errors.New("トランザクションは既に開始しています")
	}
	if err := e.pg.Begin(); err != nil {
		return nil, err
	}
	e.explicitTx = true
	return &Result{Message: "トランザクションを開始しました"}, nil
}

// executeCommit は明示トランザクションを確定する。
func (e *Engine) executeCommit() (*Result, error) {
	if !e.explicitTx {
		return nil, errors.New("トランザクションが開始されていません")
	}
	if err := e.pg.Commit(); err != nil {
		return nil, err
	}
	e.explicitTx = false
	return &Result{Message: "コミットしました"}, nil
}

// executeRollback は明示トランザクションを取り消す。
func (e *Engine) executeRollback() (*Result, error) {
	if !e.explicitTx {
		return nil, errors.New("トランザクションが開始されていません")
	}
	if err := e.pg.Rollback(); err != nil {
		return nil, err
	}
	e.explicitTx = false
	return &Result{Message: "ロールバックしました"}, nil
}

// executePragma は PRAGMA 文を実行する。対応するのは journal_mode
// (閲覧・rollback/wal 切替)と wal_checkpoint(明示チェックポイント)のみ。
func (e *Engine) executePragma(stmt *sql.PragmaStmt) (*Result, error) {
	switch strings.ToLower(stmt.Name) {
	case "journal_mode":
		return e.pragmaJournalMode(stmt.Value)
	case "wal_checkpoint":
		if stmt.Value != "" {
			return nil, fmt.Errorf("PRAGMA wal_checkpoint は値を取りません")
		}
		if err := e.pg.Checkpoint(); err != nil {
			return nil, err
		}
		return &Result{Message: "チェックポイントを実行しました"}, nil
	default:
		return nil, fmt.Errorf("未対応の PRAGMA です: %s", stmt.Name)
	}
}

// pragmaJournalMode は PRAGMA journal_mode を処理する。値が指定されて
// いなければ現在のモードを返し、指定されていればモードを切り替える。
// 表示名は WAL/DELETE という SQLite の PRAGMA 語彙に合わせる
// (pager.JournalMode.String() が返す rollback/wal という Go 内部向けの
// 名前とは、あえて分けている)。
func (e *Engine) pragmaJournalMode(value string) (*Result, error) {
	if value == "" {
		return &Result{Message: fmt.Sprintf("journal_mode = %s", pragmaModeName(e.pg.JournalMode()))}, nil
	}
	switch strings.ToUpper(value) {
	case "WAL":
		if err := e.pg.SetJournalMode(pager.JournalModeWAL); err != nil {
			return nil, err
		}
		return &Result{Message: "journal_mode を wal に変更しました"}, nil
	case "DELETE":
		if err := e.pg.SetJournalMode(pager.JournalModeRollback); err != nil {
			return nil, err
		}
		return &Result{Message: "journal_mode を delete に変更しました"}, nil
	default:
		return nil, fmt.Errorf("未対応の journal_mode です: %s", value)
	}
}

// pragmaModeName は JournalMode を PRAGMA journal_mode の表示名(SQLite に
// 合わせて WAL は "wal"、ロールバックジャーナルは "delete")に変換する。
func pragmaModeName(m pager.JournalMode) string {
	if m == pager.JournalModeWAL {
		return "wal"
	}
	return "delete"
}

// Close は Engine の後始末を行う。明示トランザクションが残っていれば
// Rollback してコミットされていない変更を破棄する。Pager 自体を
// 閉じるかどうかは呼び出し側の責務のままとする(Engine は Pager を
// 所有していないため)。rolled は明示トランザクションを実際に
// ロールバックしたかどうかを返す(呼び出し側が警告表示などに使う)。
func (e *Engine) Close() (rolled bool, err error) {
	if !e.explicitTx {
		return false, nil
	}
	if err := e.pg.Rollback(); err != nil {
		return false, err
	}
	e.explicitTx = false
	return true, nil
}

// execStmt は BEGIN/COMMIT/ROLLBACK 以外の SQL 文を実行する。input は
// CREATE TABLE 文をカタログへそのまま保存するための元の SQL 文字列。
//
// CREATE TABLE だけは特別扱いで、バイトコードを経由せず Catalog を
// 直接呼び出す。本物の SQLite は CREATE TABLE も内部的にはバイトコード
// (sqlite_schema への INSERT など)で実行するが、minidb では
// スキーマ操作は VM を通さないと割り切り、実装を単純にしている。
func (e *Engine) execStmt(stmt sql.Statement, input string) (*Result, error) {
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
