package exec

import (
	"fmt"

	"minidb/btree"
	"minidb/sql"
)

// Op は VM の命令の種類。SQLite の VDBE(Virtual Database Engine)の
// オペコードを、この章で必要な最小限まで削ったもの。
type Op int

const (
	// OpInit は P2 番地へジャンプして実行を開始する(SQLite の Init と同じ)。
	OpInit Op = iota
	// OpOpenRead はカーソル P1 を開く。この章ではデータベースの唯一の木を対象にする。
	OpOpenRead
	// OpRewind はカーソル P1 を先頭へ位置づける。木が空なら P2 番地へジャンプする。
	OpRewind
	// OpNext はカーソル P1 を次のセルへ進める。まだ行があれば P2 番地へジャンプする
	// (ループの後端)。
	OpNext
	// OpSeekRowid はレジスタ P3 のキーでカーソル P1 を Seek する。
	// 完全一致しなければ P2 番地へジャンプする。
	OpSeekRowid
	// OpColumn はカーソル P1 の現在行の第 P2 列をレジスタ P3 に読み出す。
	OpColumn
	// OpRowid はカーソル P1 の現在行のキー(rowid)をレジスタ P2 に読み出す。
	OpRowid
	// OpInteger は整数 P1 をレジスタ P2 に置く。
	OpInteger
	// OpString は文字列 P4 をレジスタ P1 に置く。
	OpString
	// OpResultRow はレジスタ P1 から P2 個を 1 行の結果として出力する。
	OpResultRow
	// OpEq はレジスタ P1 と P3 が等しければ P2 番地へジャンプする。
	OpEq
	// OpNe はレジスタ P1 と P3 が等しくなければ P2 番地へジャンプする。
	OpNe
	// OpLt はレジスタ P1 が P3 より小さければ P2 番地へジャンプする。
	OpLt
	// OpGt はレジスタ P1 が P3 より大きければ P2 番地へジャンプする。
	OpGt
	// OpLe はレジスタ P1 が P3 以下なら P2 番地へジャンプする。
	OpLe
	// OpGe はレジスタ P1 が P3 以上なら P2 番地へジャンプする。
	OpGe
	// OpHalt は実行を終了する。
	OpHalt
	// OpGoto は P2 番地へ無条件にジャンプする。
	OpGoto
)

// String はオペコード名を SQLite の EXPLAIN と同じ見た目("Op" 接頭辞なし)で返す。
func (op Op) String() string {
	switch op {
	case OpInit:
		return "Init"
	case OpOpenRead:
		return "OpenRead"
	case OpRewind:
		return "Rewind"
	case OpNext:
		return "Next"
	case OpSeekRowid:
		return "SeekRowid"
	case OpColumn:
		return "Column"
	case OpRowid:
		return "Rowid"
	case OpInteger:
		return "Integer"
	case OpString:
		return "String"
	case OpResultRow:
		return "ResultRow"
	case OpEq:
		return "Eq"
	case OpNe:
		return "Ne"
	case OpLt:
		return "Lt"
	case OpGt:
		return "Gt"
	case OpLe:
		return "Le"
	case OpGe:
		return "Ge"
	case OpHalt:
		return "Halt"
	case OpGoto:
		return "Goto"
	default:
		return fmt.Sprintf("Op(%d)", int(op))
	}
}

// Instr は命令 1 つ。P1〜P3 は整数オペランド、P4 は文字列オペランド。
// 各フィールドの意味は命令ごとに違う(SQLite の P1〜P4 と同じ流儀で、
// 使わないフィールドはゼロ値のままにする)。
type Instr struct {
	Op         Op
	P1, P2, P3 int
	P4         string

	// Comment は EXPLAIN 表示用の注釈。実行には一切影響しない。
	Comment string
}

const (
	// numRegisters は VM が持つレジスタの本数。
	numRegisters = 32
	// maxSteps は無限ループ対策としての実行ステップ数の上限。
	maxSteps = 1_000_000
)

// VM はバイトコードのプログラムを実行する仮想マシン。
// この章では読み取り専用で、対象は 1 本の B-Tree に固定する
// (複数テーブル・複数カーソルへの対応は後の章で扱う)。
type VM struct {
	tree       *btree.BTree
	program    []Instr
	pc         int
	regs       []sql.Value
	cursor     *btree.Cursor
	haveCursor bool
}

// NewVM は tree の上で program を実行する VM を作る。
func NewVM(tree *btree.BTree, program []Instr) *VM {
	return &VM{
		tree:    tree,
		program: program,
		regs:    make([]sql.Value, numRegisters),
	}
}

// Run はプログラムを実行し、OpResultRow のたびに fn を呼ぶ。
// fn がエラーを返したら実行を打ち切りそのエラーを返す。
func (v *VM) Run(fn func(row []sql.Value) error) error {
	v.pc = 0
	steps := 0

	for {
		if v.pc < 0 || v.pc >= len(v.program) {
			return fmt.Errorf("pc がプログラム範囲外です: pc=%d, プログラム長=%d", v.pc, len(v.program))
		}
		steps++
		if steps > maxSteps {
			return fmt.Errorf("実行ステップ数が上限 %d を超えました(無限ループの疑い)", maxSteps)
		}

		instr := v.program[v.pc]
		next := v.pc + 1

		switch instr.Op {
		case OpInit:
			next = instr.P2

		case OpOpenRead:
			c, err := v.tree.NewCursor()
			if err != nil {
				return fmt.Errorf("OpenRead: %w", err)
			}
			v.cursor = c
			v.haveCursor = true

		case OpRewind:
			if err := v.requireCursor(); err != nil {
				return err
			}
			// カーソルを作り直して先頭へ巻き戻す。OpenRead 直後は既に
			// 先頭にいるが、同じプログラム内で 2 周目を回す場合にも
			// 「先頭へ位置づける」という意味論を守るため。
			c, err := v.tree.NewCursor()
			if err != nil {
				return fmt.Errorf("Rewind: %w", err)
			}
			v.cursor = c
			if !v.cursor.Valid() {
				next = instr.P2
			}

		case OpNext:
			if err := v.requireCursor(); err != nil {
				return err
			}
			if err := v.cursor.Next(); err != nil {
				return fmt.Errorf("Next: %w", err)
			}
			if v.cursor.Valid() {
				next = instr.P2
			}

		case OpSeekRowid:
			if err := v.requireCursor(); err != nil {
				return err
			}
			key, err := v.regInt(instr.P3)
			if err != nil {
				return fmt.Errorf("SeekRowid: %w", err)
			}
			if err := v.cursor.Seek(uint64(key)); err != nil {
				return fmt.Errorf("SeekRowid: %w", err)
			}
			if !v.cursor.Valid() || v.cursor.Key() != uint64(key) {
				next = instr.P2
			}

		case OpColumn:
			if err := v.requireValidCursor(); err != nil {
				return err
			}
			values, err := DecodeRecord(v.cursor.Value())
			if err != nil {
				return fmt.Errorf("Column: レコードのデコードに失敗しました: %w", err)
			}
			if instr.P2 < 0 || instr.P2 >= len(values) {
				return fmt.Errorf("Column: 列番号 %d が範囲外です(列数 %d)", instr.P2, len(values))
			}
			if err := v.setReg(instr.P3, values[instr.P2]); err != nil {
				return fmt.Errorf("Column: %w", err)
			}

		case OpRowid:
			if err := v.requireValidCursor(); err != nil {
				return err
			}
			if err := v.setReg(instr.P2, sql.Value{Int: int64(v.cursor.Key())}); err != nil {
				return fmt.Errorf("Rowid: %w", err)
			}

		case OpInteger:
			if err := v.setReg(instr.P2, sql.Value{Int: int64(instr.P1)}); err != nil {
				return fmt.Errorf("Integer: %w", err)
			}

		case OpString:
			if err := v.setReg(instr.P1, sql.Value{IsText: true, Text: instr.P4}); err != nil {
				return fmt.Errorf("String: %w", err)
			}

		case OpResultRow:
			row, err := v.readRegs(instr.P1, instr.P2)
			if err != nil {
				return fmt.Errorf("ResultRow: %w", err)
			}
			if err := fn(row); err != nil {
				return err
			}

		case OpEq, OpNe, OpLt, OpGt, OpLe, OpGe:
			ok, err := v.compare(instr)
			if err != nil {
				return err
			}
			if ok {
				next = instr.P2
			}

		case OpGoto:
			next = instr.P2

		case OpHalt:
			return nil

		default:
			return fmt.Errorf("不正な命令です: pc=%d, op=%v", v.pc, instr.Op)
		}

		v.pc = next
	}
}

// requireCursor はカーソルが開かれていることを確認する(Valid である必要はない)。
func (v *VM) requireCursor() error {
	if !v.haveCursor {
		return fmt.Errorf("カーソルが開かれていません(OpenRead が必要です)")
	}
	return nil
}

// requireValidCursor はカーソルが開かれ、かつ有効な行を指していることを確認する。
func (v *VM) requireValidCursor() error {
	if err := v.requireCursor(); err != nil {
		return err
	}
	if !v.cursor.Valid() {
		return fmt.Errorf("カーソルが有効な行を指していません")
	}
	return nil
}

// reg はレジスタ番号の範囲を検査してから値を返す。
func (v *VM) reg(n int) (sql.Value, error) {
	if n < 0 || n >= len(v.regs) {
		return sql.Value{}, fmt.Errorf("レジスタ番号 %d が範囲外です(0..%d)", n, len(v.regs)-1)
	}
	return v.regs[n], nil
}

// setReg はレジスタ番号の範囲を検査してから値を書き込む。
func (v *VM) setReg(n int, value sql.Value) error {
	if n < 0 || n >= len(v.regs) {
		return fmt.Errorf("レジスタ番号 %d が範囲外です(0..%d)", n, len(v.regs)-1)
	}
	v.regs[n] = value
	return nil
}

// regInt はレジスタから整数値を取り出す。TEXT が入っていればエラーになる。
func (v *VM) regInt(n int) (int64, error) {
	value, err := v.reg(n)
	if err != nil {
		return 0, err
	}
	if value.IsText {
		return 0, fmt.Errorf("レジスタ %d は TEXT です(INTEGER が必要)", n)
	}
	return value.Int, nil
}

// readRegs はレジスタ start から count 個ぶんの値をコピーして返す。
func (v *VM) readRegs(start, count int) ([]sql.Value, error) {
	row := make([]sql.Value, count)
	for i := 0; i < count; i++ {
		value, err := v.reg(start + i)
		if err != nil {
			return nil, err
		}
		row[i] = value
	}
	return row, nil
}

// compare は比較命令を実行し、「r[P1] op r[P3] が成立したら true」を返す。
// 整数同士・文字列同士の比較だけを許し、型が食い違ったらエラーにする
// (この VM の型検査は素朴なもので割り切る)。
func (v *VM) compare(instr Instr) (bool, error) {
	left, err := v.reg(instr.P1)
	if err != nil {
		return false, fmt.Errorf("%v: %w", instr.Op, err)
	}
	right, err := v.reg(instr.P3)
	if err != nil {
		return false, fmt.Errorf("%v: %w", instr.Op, err)
	}
	if left.IsText != right.IsText {
		return false, fmt.Errorf("%v: 型が一致しません(r[%d]=%s, r[%d]=%s)",
			instr.Op, instr.P1, typeName(left), instr.P3, typeName(right))
	}

	var cmp int
	if left.IsText {
		cmp = compareString(left.Text, right.Text)
	} else {
		cmp = compareInt(left.Int, right.Int)
	}

	switch instr.Op {
	case OpEq:
		return cmp == 0, nil
	case OpNe:
		return cmp != 0, nil
	case OpLt:
		return cmp < 0, nil
	case OpGt:
		return cmp > 0, nil
	case OpLe:
		return cmp <= 0, nil
	case OpGe:
		return cmp >= 0, nil
	default:
		return false, fmt.Errorf("compare: 比較命令ではありません: %v", instr.Op)
	}
}

func typeName(v sql.Value) string {
	if v.IsText {
		return "TEXT"
	}
	return "INTEGER"
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
