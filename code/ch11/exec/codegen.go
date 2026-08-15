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

// Compile は構文木からバイトコードを生成する。SELECT(WHERE なし)と
// INSERT だけに対応する。WHERE 付き SELECT や UPDATE、DELETE は
// 第 12 章でカーソルの Seek や条件分岐を組み合わせて対応するため、
// ここでは「未対応です」というエラーを返す。
func Compile(stmt sql.Statement, cat *Catalog) ([]Instr, error) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		if s.Where != nil {
			return nil, fmt.Errorf("WHERE 付きの SELECT は第 12 章で実装します")
		}
		return compileSelect(s, cat)
	case *sql.InsertStmt:
		return compileInsert(s, cat)
	case *sql.UpdateStmt:
		return nil, fmt.Errorf("UPDATE は第 12 章で実装します")
	case *sql.DeleteStmt:
		return nil, fmt.Errorf("DELETE は第 12 章で実装します")
	case *sql.CreateTableStmt:
		return nil, fmt.Errorf("CREATE TABLE はバイトコードを経由せず Engine が直接実行します")
	default:
		return nil, fmt.Errorf("未対応の文です: %T", stmt)
	}
}

// resolvedColumn は SELECT の列 1 つを、実行時にどう読み出すかへ解決した結果。
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
	// PK を除いた列がレコード内で何番目になるかの対応表を先に作る。
	recordIndex := make([]int, len(info.Columns))
	idx := 0
	for i := range info.Columns {
		if i == info.PKIndex {
			recordIndex[i] = -1
			continue
		}
		recordIndex[i] = idx
		idx++
	}

	resolveOne := func(name string) (resolvedColumn, error) {
		for i, col := range info.Columns {
			if col.Name != name {
				continue
			}
			if i == info.PKIndex {
				return resolvedColumn{name: name, isRowid: true}, nil
			}
			return resolvedColumn{name: name, recordIndex: recordIndex[i]}, nil
		}
		return resolvedColumn{}, fmt.Errorf("列 %s は見つかりません", name)
	}

	if star {
		cols := make([]resolvedColumn, len(info.Columns))
		for i, col := range info.Columns {
			rc, err := resolveOne(col.Name)
			if err != nil {
				return nil, err
			}
			cols[i] = rc
		}
		return cols, nil
	}

	cols := make([]resolvedColumn, len(names))
	for i, name := range names {
		rc, err := resolveOne(name)
		if err != nil {
			return nil, err
		}
		cols[i] = rc
	}
	return cols, nil
}

// compileSelect は WHERE のない SELECT を、
// OpenRead → Rewind → (列の読み出し) → ResultRow → Next のループへ
// コンパイルする。
func compileSelect(stmt *sql.SelectStmt, cat *Catalog) ([]Instr, error) {
	info, err := cat.Get(stmt.Table)
	if err != nil {
		return nil, err
	}
	cols, err := resolveColumns(info, stmt.Star, stmt.Columns)
	if err != nil {
		return nil, err
	}

	// レジスタ 1..len(cols) に各列を読み出す。
	baseReg := 1
	var program []Instr
	program = append(program,
		Instr{Op: OpInit, P2: 1, Comment: "アドレス 1 へジャンプ"},
		Instr{Op: OpOpenRead, P1: cursorNum, P2: int(info.Root), Comment: fmt.Sprintf("%s を開く", info.Name)},
	)

	rewindAddr := len(program)
	program = append(program, Instr{Op: OpRewind, P1: cursorNum, Comment: "先頭へ"})
	loopStart := len(program)

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
	program = append(program, Instr{Op: OpNext, P1: cursorNum, P2: loopStart, Comment: "次の行があれば戻る"})

	haltAddr := len(program)
	program = append(program, Instr{Op: OpHalt, Comment: "終了"})
	program[rewindAddr].P2 = haltAddr

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

// checkValueType は INSERT の値がその列の型と一致するかをコンパイル時に検査する。
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
