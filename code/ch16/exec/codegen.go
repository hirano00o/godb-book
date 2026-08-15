// codegen.go は構文木(sql.Statement)からバイトコード(Instr の列)を
// 生成するコンパイラを担当する。
package exec

import (
	"fmt"

	"minidb/sql"
)

// カーソル番号は 0 固定とする。この章のコンパイラが生成するプログラムは
// テーブルを 1 つしか開かないので、複数カーソルを使い分ける必要がない。
const cursorNum = 0

// Compile は構文木からバイトコードを生成する。
func Compile(stmt sql.Statement, cat *Catalog) ([]Instr, error) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return compileSelect(s, cat)
	case *sql.InsertStmt:
		return compileInsert(s, cat)
	case *sql.UpdateStmt:
		return compileUpdate(s, cat)
	case *sql.DeleteStmt:
		return compileDelete(s, cat)
	case *sql.CreateTableStmt:
		return nil, fmt.Errorf("CREATE TABLE はバイトコードを経由せず Engine が直接実行します")
	default:
		return nil, fmt.Errorf("未対応の文です: %T", stmt)
	}
}

// resolvedColumn は結果行の列 1 つを、実行時にどう読み出すかへ解決した結果。
// isRowid が true なら OpRowid、そうでなければ OpColumn で recordIndex 番目
// (PK を除いた、レコード内での位置)を読む。
type resolvedColumn struct {
	name        string
	isRowid     bool
	recordIndex int
}

// resolveColumns は SELECT の列指定(* または列名リスト)を、
// テーブルの列定義をもとに resolvedColumn の列へ解決する。
//
// PK 列(INTEGER PRIMARY KEY)は rowid そのものであり、レコードには
// 保存されていない。そのため、レコード内の列番号は「PK 列を除いた
// 位置」に詰めて対応づける必要がある。例えば列が (id PK, name, age) なら
// レコードには (name, age) の 2 列しか入っておらず、name は
// recordIndex=0、age は recordIndex=1 になる。
func resolveColumns(info *TableInfo, star bool, names []string) ([]resolvedColumn, error) {
	if star {
		names = make([]string, len(info.Columns))
		for i, col := range info.Columns {
			names[i] = col.Name
		}
	}

	cols := make([]resolvedColumn, len(names))
	for i, name := range names {
		rc, err := resolveColumn(info, name)
		if err != nil {
			return nil, err
		}
		cols[i] = rc
	}
	return cols, nil
}

// resolveColumn は列名 1 つを resolvedColumn へ解決する。
func resolveColumn(info *TableInfo, name string) (resolvedColumn, error) {
	// PK を除いた列がレコード内で何番目になるかを、対象の列に着くまで数える。
	idx := 0
	for i, col := range info.Columns {
		if col.Name != name {
			if i != info.PKIndex {
				idx++
			}
			continue
		}
		if i == info.PKIndex {
			return resolvedColumn{name: name, isRowid: true}, nil
		}
		return resolvedColumn{name: name, recordIndex: idx}, nil
	}
	return resolvedColumn{}, fmt.Errorf("列 %s は見つかりません", name)
}

// findColumn は列名から ColumnDef(型情報を含む)を引く。
func findColumn(info *TableInfo, name string) (sql.ColumnDef, error) {
	for _, col := range info.Columns {
		if col.Name == name {
			return col, nil
		}
	}
	return sql.ColumnDef{}, fmt.Errorf("列 %s は見つかりません", name)
}

// checkWhereType は WHERE 句の列とリテラルの型が一致するかをコンパイル時に検査する。
func checkWhereType(info *TableInfo, where *sql.WhereClause) error {
	col, err := findColumn(info, where.Column)
	if err != nil {
		return err
	}
	return checkValueType(col, where.Value)
}

// isPKEquality は where が「PK 列 = 整数リテラル」の形かどうかを判定する。
// この形のときだけ、全件スキャンではなく SeekRowid によるポイント
// ルックアップへコンパイルできる(第 10 章の point lookup と同じ形)。
func isPKEquality(info *TableInfo, where *sql.WhereClause) bool {
	if where == nil || info.PKIndex < 0 {
		return false
	}
	if where.Op != sql.OpEq {
		return false
	}
	if where.Column != info.Columns[info.PKIndex].Name {
		return false
	}
	return !where.Value.IsText
}

// invertOp は WHERE の比較演算子 op を否定した演算子の VM 命令を返す
// (Eq↔Ne, Lt↔Ge, Gt↔Le)。フルスキャンのループで「条件が成立する行だけ
// 残す」ことを、「条件が不成立の行は否定条件が真になった時点で
// 次の行へ読み飛ばす」という形で実現するために使う。
func invertOp(op sql.CompareOp) Op {
	switch op {
	case sql.OpEq:
		return OpNe
	case sql.OpNe:
		return OpEq
	case sql.OpLt:
		return OpGe
	case sql.OpGe:
		return OpLt
	case sql.OpGt:
		return OpLe
	default: // sql.OpLe
		return OpGt
	}
}

// compileSelect は SELECT を、WHERE の有無・形に応じて次のどちらかへ
// コンパイルする。
//
//   - WHERE が「PK 列 = 整数リテラル」: compileSeekRowid(最適化)
//   - それ以外(WHERE なし、または一般の WHERE): compileFullScan
func compileSelect(stmt *sql.SelectStmt, cat *Catalog) ([]Instr, error) {
	info, err := cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	cols, err := resolveColumns(info, stmt.Star, stmt.Columns)
	if err != nil {
		return nil, err
	}
	if stmt.Where != nil {
		if err := checkWhereType(info, stmt.Where); err != nil {
			return nil, err
		}
	}

	if isPKEquality(info, stmt.Where) {
		return compileSeekRowid(info, stmt.Where, cols), nil
	}
	return compileFullScan(info, stmt.Where, cols)
}

// compileSeekRowid は「PK 列 = 整数リテラル」の WHERE を、
// Init / OpenRead / Integer(キーを r[1] へ) / SeekRowid(不一致なら Halt へ)
// / 列読み出し / ResultRow / Halt という、全件スキャンを伴わないポイント
// ルックアップへコンパイルする。cols は投影(出力)する列。
func compileSeekRowid(info *TableInfo, where *sql.WhereClause, cols []resolvedColumn) []Instr {
	const keyReg = 1
	const baseReg = 2

	program := []Instr{
		{Op: OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		{Op: OpOpenRead, P1: cursorNum, P2: int(info.Root), Comment: fmt.Sprintf("%s を開く", info.Name)},
		{Op: OpInteger, P1: int(where.Value.Int), P2: keyReg, Comment: fmt.Sprintf("PK の探索値 %d を r[%d] へ", where.Value.Int, keyReg)},
	}

	seekAddr := len(program)
	program = append(program, Instr{Op: OpSeekRowid, P1: cursorNum, P3: keyReg, Comment: "r[1] で Seek。一致しなければ終了"})

	for i, col := range cols {
		reg := baseReg + i
		if col.isRowid {
			program = append(program, Instr{Op: OpRowid, P1: cursorNum, P2: reg,
				Comment: fmt.Sprintf("%s(rowid)を r[%d] へ", col.name, reg)})
			continue
		}
		program = append(program, Instr{Op: OpColumn, P1: cursorNum, P2: col.recordIndex, P3: reg,
			Comment: fmt.Sprintf("%s を r[%d] へ", col.name, reg)})
	}
	program = append(program, Instr{Op: OpResultRow, P1: baseReg, P2: len(cols), Comment: "1 行を出力"})

	haltAddr := len(program)
	program = append(program, Instr{Op: OpHalt, Comment: "終了"})
	program[seekAddr].P2 = haltAddr

	return program
}

// compileFullScan は SELECT/UPDATE/DELETE に共通する「全件スキャン +
// (あれば)行ごとのフィルタ」プログラムを生成する。cols は投影する列。
//
// WHERE があるループ本体は、行ごとに次を行う:
//
//  1. 比較対象の列値をレジスタへ(PK 列なら OpRowid、他は OpColumn)
//  2. リテラルをレジスタへ(OpInteger / OpString)
//  3. invertOp(where.Op) の比較命令で、条件が不成立ならこの行を
//     スキップする(Next のアドレスへジャンプ)
//  4. 通過したら cols を読み出して ResultRow
func compileFullScan(info *TableInfo, where *sql.WhereClause, cols []resolvedColumn) ([]Instr, error) {
	var program []Instr
	program = append(program,
		Instr{Op: OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		Instr{Op: OpOpenRead, P1: cursorNum, P2: int(info.Root), Comment: fmt.Sprintf("%s を開く", info.Name)},
	)

	rewindAddr := len(program)
	program = append(program, Instr{Op: OpRewind, P1: cursorNum, Comment: "先頭へ"})
	loopStart := len(program)

	baseReg := 1
	filterJumpAddr := -1
	if where != nil {
		whereCol, err := resolveColumn(info, where.Column)
		if err != nil {
			return nil, err
		}
		const whereReg = 1
		const litReg = 2
		if whereCol.isRowid {
			program = append(program, Instr{Op: OpRowid, P1: cursorNum, P2: whereReg,
				Comment: fmt.Sprintf("WHERE 列 %s(rowid)を r[%d] へ", whereCol.name, whereReg)})
		} else {
			program = append(program, Instr{Op: OpColumn, P1: cursorNum, P2: whereCol.recordIndex, P3: whereReg,
				Comment: fmt.Sprintf("WHERE 列 %s を r[%d] へ", whereCol.name, whereReg)})
		}
		if where.Value.IsText {
			program = append(program, Instr{Op: OpString, P1: litReg, P4: where.Value.Text,
				Comment: fmt.Sprintf("リテラル %q を r[%d] へ", where.Value.Text, litReg)})
		} else {
			program = append(program, Instr{Op: OpInteger, P1: int(where.Value.Int), P2: litReg,
				Comment: fmt.Sprintf("リテラル %d を r[%d] へ", where.Value.Int, litReg)})
		}
		filterJumpAddr = len(program)
		program = append(program, Instr{Op: invertOp(where.Op), P1: whereReg, P3: litReg,
			Comment: fmt.Sprintf("WHERE %s %s ... が不成立なら次の行へ", where.Column, where.Op)})
		baseReg = 3
	}

	for i, col := range cols {
		reg := baseReg + i
		if col.isRowid {
			program = append(program, Instr{Op: OpRowid, P1: cursorNum, P2: reg,
				Comment: fmt.Sprintf("%s(rowid)を r[%d] へ", col.name, reg)})
			continue
		}
		program = append(program, Instr{Op: OpColumn, P1: cursorNum, P2: col.recordIndex, P3: reg,
			Comment: fmt.Sprintf("%s を r[%d] へ", col.name, reg)})
	}
	program = append(program, Instr{Op: OpResultRow, P1: baseReg, P2: len(cols), Comment: "1 行を出力"})

	nextAddr := len(program)
	program = append(program, Instr{Op: OpNext, P1: cursorNum, P2: loopStart, Comment: "次の行があれば戻る"})

	haltAddr := len(program)
	program = append(program, Instr{Op: OpHalt, Comment: "終了"})
	program[rewindAddr].P2 = haltAddr
	if filterJumpAddr >= 0 {
		program[filterJumpAddr].P2 = nextAddr
	}

	return program, nil
}

// compileInsert は INSERT を、
//
//   - INTEGER PRIMARY KEY 列があれば、その値をそのまま rowid として使う
//   - なければ OpNewRowid で自動採番する
//
// のどちらかでキーを決め、PK 以外の列を MakeRecord でレコード化して
// OpInsert する形にコンパイルする。列数の一致・型の一致はここ
// (コンパイル時)で検査する。
func compileInsert(stmt *sql.InsertStmt, cat *Catalog) ([]Instr, error) {
	info, err := cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	if len(stmt.Values) != len(info.Columns) {
		return nil, fmt.Errorf("値の数が列数と一致しません: 値 %d 個, 列 %d 個", len(stmt.Values), len(info.Columns))
	}
	for i, col := range info.Columns {
		if err := checkValueType(col, stmt.Values[i]); err != nil {
			return nil, err
		}
	}

	const keyReg = 1
	const recordStartReg = 2

	program := []Instr{
		{Op: OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		{Op: OpOpenWrite, P1: cursorNum, P2: int(info.Root), P4: info.Name, Comment: fmt.Sprintf("%s を書き込み用に開く", info.Name)},
	}

	if info.PKIndex >= 0 {
		v := stmt.Values[info.PKIndex]
		program = append(program, Instr{Op: OpInteger, P1: int(v.Int), P2: keyReg, Comment: "PK の値を rowid として r[1] へ"})
	} else {
		program = append(program, Instr{Op: OpNewRowid, P1: cursorNum, P2: keyReg, Comment: "rowid を自動採番して r[1] へ"})
	}

	reg := recordStartReg
	count := 0
	for i, col := range info.Columns {
		if i == info.PKIndex {
			continue
		}
		v := stmt.Values[i]
		if col.Type == sql.TypeText {
			program = append(program, Instr{Op: OpString, P1: reg, P4: v.Text, Comment: fmt.Sprintf("%s を r[%d] へ", col.Name, reg)})
		} else {
			program = append(program, Instr{Op: OpInteger, P1: int(v.Int), P2: reg, Comment: fmt.Sprintf("%s を r[%d] へ", col.Name, reg)})
		}
		reg++
		count++
	}

	program = append(program,
		Instr{Op: OpMakeRecord, P1: recordStartReg, P2: count, Comment: "PK を除く列をレコード化"},
		Instr{Op: OpInsert, P1: cursorNum, P2: keyReg, Comment: "rowid=r[1] でレコードを挿入"},
		Instr{Op: OpHalt, Comment: "終了"},
	)
	return program, nil
}

// checkValueType は値がその列の型と一致するかをコンパイル時に検査する。
// INSERT の値検査と WHERE / UPDATE SET のリテラル検査を兼ねる。
func checkValueType(col sql.ColumnDef, v sql.Value) error {
	switch col.Type {
	case sql.TypeInteger:
		if v.IsText {
			return fmt.Errorf("列 %s は INTEGER ですが TEXT の値が指定されました", col.Name)
		}
	case sql.TypeText:
		if !v.IsText {
			return fmt.Errorf("列 %s は TEXT ですが INTEGER の値が指定されました", col.Name)
		}
	default:
		return fmt.Errorf("列 %s の型が不明です", col.Name)
	}
	return nil
}

// updateProjectionColumns は UPDATE の探索プログラムが出力する列を返す。
// 「rowid + PK を除く全列の現在値」を、書き換えフェーズ(Engine)が
// 使える形で列挙する(compileUpdate のコメントを参照)。
func updateProjectionColumns(info *TableInfo) []resolvedColumn {
	cols := []resolvedColumn{{name: "rowid", isRowid: true}}
	idx := 0
	for i, col := range info.Columns {
		if i == info.PKIndex {
			continue
		}
		cols = append(cols, resolvedColumn{name: col.Name, recordIndex: idx})
		idx++
	}
	return cols
}

// deleteProjectionColumns は DELETE の探索プログラムが出力する列(rowid のみ)を返す。
func deleteProjectionColumns() []resolvedColumn {
	return []resolvedColumn{{name: "rowid", isRowid: true}}
}

// checkAssignments は UPDATE の SET 句をコンパイル時に検査する。
//   - 代入先の列が存在するか
//   - 型が一致するか
//   - PK 列への代入でないか(rowid の変更は木のキー変更を伴い、
//     この章の 2 相方式では扱えないため未対応)
func checkAssignments(info *TableInfo, assigns []sql.Assignment) error {
	for _, a := range assigns {
		if info.PKIndex >= 0 && info.Columns[info.PKIndex].Name == a.Column {
			return fmt.Errorf("PRIMARY KEY 列の UPDATE は未対応です(rowid の変更を伴うため)")
		}
		col, err := findColumn(info, a.Column)
		if err != nil {
			return err
		}
		if err := checkValueType(col, a.Value); err != nil {
			return err
		}
	}
	return nil
}

// compileUpdate は UPDATE を「対象行を列挙する SELECT」へ脱糖する。
//
// UPDATE は探索(どの行が対象か)と書き換え(木への Insert/Delete)の
// 2 相に分けて実行する(方針は engine.go の executeUpdate のコメントを
// 参照)。ここでコンパイルするのは探索フェーズのみで、WHERE の扱いは
// SELECT と全く同じ機構(compileSeekRowid / compileFullScan)を使い、
// ResultRow で「rowid + PK を除く全列の現在値」を出力する。
func compileUpdate(stmt *sql.UpdateStmt, cat *Catalog) ([]Instr, error) {
	info, err := cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	if err := checkAssignments(info, stmt.Set); err != nil {
		return nil, err
	}
	if stmt.Where != nil {
		if err := checkWhereType(info, stmt.Where); err != nil {
			return nil, err
		}
	}

	cols := updateProjectionColumns(info)
	if isPKEquality(info, stmt.Where) {
		return compileSeekRowid(info, stmt.Where, cols), nil
	}
	return compileFullScan(info, stmt.Where, cols)
}

// compileDelete は DELETE を「対象行の rowid を列挙する SELECT」へ脱糖する。
// UPDATE と同じく、探索フェーズだけをここでコンパイルし、実際の
// btree.Delete は engine.go の executeDelete(2 相の第 2 相)が行う。
func compileDelete(stmt *sql.DeleteStmt, cat *Catalog) ([]Instr, error) {
	info, err := cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	if stmt.Where != nil {
		if err := checkWhereType(info, stmt.Where); err != nil {
			return nil, err
		}
	}

	cols := deleteProjectionColumns()
	if isPKEquality(info, stmt.Where) {
		return compileSeekRowid(info, stmt.Where, cols), nil
	}
	return compileFullScan(info, stmt.Where, cols)
}
