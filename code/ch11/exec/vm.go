package exec

import (
	"fmt"

	"minidb/btree"
	"minidb/pager"
	"minidb/sql"
)

// Op は VM の命令の種類。SQLite の VDBE(Virtual Database Engine)の
// オペコードを、この章で必要な最小限まで削ったもの。
type Op int

const (
	// OpInit は P2 番地へジャンプして実行を開始する(SQLite の Init と同じ)。
	OpInit Op = iota
	// OpOpenRead はカーソル P1 を、ルートページ P2 の木の上に開く(読み取り用)。
	// 本物の SQLite の EXPLAIN で見る "OpenRead 0 2" がまさに
	// 「カーソル 0 をルートページ 2 の木に開く」という意味であり、
	// この章から VM が複数の木(テーブル)を同時に扱えるようになったことで
	// 同じ意味論に揃った(第 10 章までは木が 1 本しかなく、P2 は使っていなかった)。
	OpOpenRead
	// OpOpenWrite は OpenRead と同じだが書き込み用にカーソルを開く。
	// P4 にはテーブル名を入れる。この名前は RootMoves がルート移動を
	// テーブル名で報告するために覚えておく(カタログ更新は名前で行うため)。
	OpOpenWrite
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
	// OpNewRowid はカーソル P1 の木の最大キー + 1 をレジスタ P2 に置く
	// (木が空なら 1)。INTEGER PRIMARY KEY を持たないテーブルへの
	// INSERT で rowid を自動採番するために使う。
	OpNewRowid
	// OpMakeRecord はレジスタ P1 から P2 個の値をレコード形式にエンコードする。
	// 結果のバイト列はレジスタには置かず、VM が内部に持つ 1 本の
	// 一時領域(record フィールド)へ書き込む。P3 は「本来ならレジスタ番号」
	// だが、この章の割り切りとして未使用のまま 0 にしておく
	// (詳しくは VM.record のコメントを参照)。
	OpMakeRecord
	// OpInsert はカーソル P1 の木に、キー=レジスタ P2、値=直前の
	// OpMakeRecord が作ったレコード(VM.record)を挿入する。
	OpInsert
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
	case OpOpenWrite:
		return "OpenWrite"
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
	case OpNewRowid:
		return "NewRowid"
	case OpMakeRecord:
		return "MakeRecord"
	case OpInsert:
		return "Insert"
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

// cursorState はカーソル 1 つぶんの状態。開いている木、走査位置
// (btree.Cursor)、そして書き込み用カーソルであれば「どのテーブルの
// カーソルか」を保持する。テーブル名は RootMoves がルート移動を報告する
// ときの キーに使う(カタログはテーブル名で行を引き直すため)。
type cursorState struct {
	tree      *btree.BTree
	cursor    *btree.Cursor
	writable  bool
	tableName string
}

// VM はバイトコードのプログラムを実行する仮想マシン。
//
// 第 10 章までは木が 1 本に固定されていたが、この章からカタログと
// 複数のテーブルの木を同時に扱えるよう、カーソルを P1 をキーにした
// map で管理する。木はもう VM の外から渡されず、OpOpenRead/OpOpenWrite
// が P2(ルートページ番号)を使ってその場で btree.OpenAt する。
type VM struct {
	pg      *pager.Pager
	program []Instr
	pc      int
	regs    []sql.Value
	cursors map[int]*cursorState

	// record は OpMakeRecord が作ったレコードのバイト列を保持する
	// 一時領域。本来なら「P3 で指定したレジスタに置く」ところだが、
	// レジスタ(sql.Value)はバイト列を持てる型ではないため、この章では
	// レジスタとは別にこの 1 本のフィールドに置き、直後の OpInsert が
	// そこから読む方式に簡略化する(OpMakeRecord の P3 は未使用)。
	// 複数のレコードを同時に組み立てて使い回すような高度なプログラムは
	// この章のコンパイラは生成しないので、この割り切りで十分足りる。
	record []byte

	// rootMoves は OpInsert によって木のルートが変わったテーブルの
	// 名前から新しいルートページ番号への対応。OpOpenWrite の P4 で
	// 覚えたテーブル名をキーにする。Engine は Run の後にこれを読んで
	// カタログの記録を更新する。
	rootMoves map[string]pager.PageID
}

// NewVM は pg の上で program を実行する VM を作る。
func NewVM(pg *pager.Pager, program []Instr) *VM {
	return &VM{
		pg:        pg,
		program:   program,
		regs:      make([]sql.Value, numRegisters),
		cursors:   make(map[int]*cursorState),
		rootMoves: make(map[string]pager.PageID),
	}
}

// RootMoves は Run の実行中に OpInsert でルートが変わったテーブルの
// 一覧を、テーブル名から新しいルートページ番号への対応として返す。
func (v *VM) RootMoves() map[string]pager.PageID {
	return v.rootMoves
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
			cs, err := v.openCursor(instr.P2, false, "")
			if err != nil {
				return fmt.Errorf("OpenRead: %w", err)
			}
			v.cursors[instr.P1] = cs

		case OpOpenWrite:
			cs, err := v.openCursor(instr.P2, true, instr.P4)
			if err != nil {
				return fmt.Errorf("OpenWrite: %w", err)
			}
			v.cursors[instr.P1] = cs

		case OpRewind:
			cs, err := v.requireCursor(instr.P1)
			if err != nil {
				return err
			}
			// カーソルを作り直して先頭へ巻き戻す。OpenRead 直後は既に
			// 先頭にいるが、同じプログラム内で 2 周目を回す場合にも
			// 「先頭へ位置づける」という意味論を守るため。
			c, err := cs.tree.NewCursor()
			if err != nil {
				return fmt.Errorf("Rewind: %w", err)
			}
			cs.cursor = c
			if !cs.cursor.Valid() {
				next = instr.P2
			}

		case OpNext:
			cs, err := v.requireCursor(instr.P1)
			if err != nil {
				return err
			}
			if err := cs.cursor.Next(); err != nil {
				return fmt.Errorf("Next: %w", err)
			}
			if cs.cursor.Valid() {
				next = instr.P2
			}

		case OpSeekRowid:
			cs, err := v.requireCursor(instr.P1)
			if err != nil {
				return err
			}
			key, err := v.regInt(instr.P3)
			if err != nil {
				return fmt.Errorf("SeekRowid: %w", err)
			}
			if err := cs.cursor.Seek(uint64(key)); err != nil {
				return fmt.Errorf("SeekRowid: %w", err)
			}
			if !cs.cursor.Valid() || cs.cursor.Key() != uint64(key) {
				next = instr.P2
			}

		case OpColumn:
			cs, err := v.requireValidCursor(instr.P1)
			if err != nil {
				return err
			}
			values, err := DecodeRecord(cs.cursor.Value())
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
			cs, err := v.requireValidCursor(instr.P1)
			if err != nil {
				return err
			}
			if err := v.setReg(instr.P2, sql.Value{Int: int64(cs.cursor.Key())}); err != nil {
				return fmt.Errorf("Rowid: %w", err)
			}

		case OpNewRowid:
			cs, err := v.requireCursor(instr.P1)
			if err != nil {
				return err
			}
			maxKey, ok, err := cs.tree.MaxKey()
			if err != nil {
				return fmt.Errorf("NewRowid: %w", err)
			}
			rowid := uint64(1)
			if ok {
				rowid = maxKey + 1
			}
			if err := v.setReg(instr.P2, sql.Value{Int: int64(rowid)}); err != nil {
				return fmt.Errorf("NewRowid: %w", err)
			}

		case OpMakeRecord:
			values, err := v.readRegs(instr.P1, instr.P2)
			if err != nil {
				return fmt.Errorf("MakeRecord: %w", err)
			}
			data, err := EncodeRecord(values)
			if err != nil {
				return fmt.Errorf("MakeRecord: %w", err)
			}
			v.record = data

		case OpInsert:
			cs, err := v.requireCursor(instr.P1)
			if err != nil {
				return err
			}
			if !cs.writable {
				return fmt.Errorf("Insert: カーソル %d は書き込み用に開かれていません(OpenWrite が必要です)", instr.P1)
			}
			key, err := v.regInt(instr.P2)
			if err != nil {
				return fmt.Errorf("Insert: %w", err)
			}
			beforeRoot := cs.tree.Root()
			if err := cs.tree.Insert(uint64(key), v.record); err != nil {
				return fmt.Errorf("Insert: %w", err)
			}
			if afterRoot := cs.tree.Root(); afterRoot != beforeRoot && cs.tableName != "" {
				v.rootMoves[cs.tableName] = afterRoot
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

// openCursor はルートページ root の木を開き、その上にカーソルを立てる。
func (v *VM) openCursor(root int, writable bool, tableName string) (*cursorState, error) {
	tree := btree.OpenAt(v.pg, pager.PageID(root))
	c, err := tree.NewCursor()
	if err != nil {
		return nil, err
	}
	return &cursorState{tree: tree, cursor: c, writable: writable, tableName: tableName}, nil
}

// requireCursor はカーソル P1 が開かれていることを確認して返す(Valid である必要はない)。
func (v *VM) requireCursor(p1 int) (*cursorState, error) {
	cs, ok := v.cursors[p1]
	if !ok {
		return nil, fmt.Errorf("カーソル %d が開かれていません(OpenRead/OpenWrite が必要です)", p1)
	}
	return cs, nil
}

// requireValidCursor はカーソル P1 が開かれ、かつ有効な行を指していることを確認する。
func (v *VM) requireValidCursor(p1 int) (*cursorState, error) {
	cs, err := v.requireCursor(p1)
	if err != nil {
		return nil, err
	}
	if !cs.cursor.Valid() {
		return nil, fmt.Errorf("カーソルが有効な行を指していません")
	}
	return cs, nil
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
