// ast.go は sql パッケージが扱う構文木のノード型を定義する。
package sql

import "fmt"

// Statement は SQL 文を表す構文木のルート型。
type Statement interface{ stmtNode() }

// ColumnType は CREATE TABLE における列の型。
type ColumnType int

const (
	TypeInteger ColumnType = iota + 1
	TypeText
)

// String は列型を SQL のキーワード表記で返す。
func (t ColumnType) String() string {
	switch t {
	case TypeInteger:
		return "INTEGER"
	case TypeText:
		return "TEXT"
	default:
		return fmt.Sprintf("ColumnType(%d)", int(t))
	}
}

// ColumnDef は CREATE TABLE の列定義 1 つ。
type ColumnDef struct {
	Name       string
	Type       ColumnType
	PrimaryKey bool
}

// Value はリテラル値(整数または文字列)を表す。
type Value struct {
	IsText bool
	Int    int64
	Text   string
}

// CompareOp は WHERE 句の比較演算子。
type CompareOp int

const (
	OpEq CompareOp = iota
	OpNe
	OpLt
	OpGt
	OpLe
	OpGe
)

// String は比較演算子を SQL の記号表記で返す。
func (op CompareOp) String() string {
	switch op {
	case OpEq:
		return "="
	case OpNe:
		return "!="
	case OpLt:
		return "<"
	case OpGt:
		return ">"
	case OpLe:
		return "<="
	case OpGe:
		return ">="
	default:
		return fmt.Sprintf("CompareOp(%d)", int(op))
	}
}

// WhereClause は「列名 演算子 リテラル」の単一比較を表す。
// このサブセットでは AND/OR による複合条件は扱わない。
type WhereClause struct {
	Column string
	Op     CompareOp
	Value  Value
}

// CreateTableStmt は CREATE TABLE 文。
type CreateTableStmt struct {
	Table   string
	Columns []ColumnDef
}

func (*CreateTableStmt) stmtNode() {}

// InsertStmt は INSERT INTO 文。値はリテラルのみを受け付ける。
type InsertStmt struct {
	Table  string
	Values []Value
}

func (*InsertStmt) stmtNode() {}

// SelectStmt は SELECT 文。Star が true のときは Columns を無視する。
type SelectStmt struct {
	Star    bool
	Columns []string
	Table   string
	Where   *WhereClause
}

func (*SelectStmt) stmtNode() {}

// Assignment は UPDATE の SET 句 1 つ分(列名と代入する値)。
type Assignment struct {
	Column string
	Value  Value
}

// UpdateStmt は UPDATE 文。
type UpdateStmt struct {
	Table string
	Set   []Assignment
	Where *WhereClause
}

func (*UpdateStmt) stmtNode() {}

// DeleteStmt は DELETE FROM 文。
type DeleteStmt struct {
	Table string
	Where *WhereClause
}

func (*DeleteStmt) stmtNode() {}

// BeginStmt は BEGIN 文(トランザクション開始)。"BEGIN;" の形のみを
// 扱い、TRANSACTION キーワードには対応しない。
type BeginStmt struct{}

func (*BeginStmt) stmtNode() {}

// CommitStmt は COMMIT 文(トランザクション確定)。
type CommitStmt struct{}

func (*CommitStmt) stmtNode() {}

// RollbackStmt は ROLLBACK 文(トランザクション取り消し)。
type RollbackStmt struct{}

func (*RollbackStmt) stmtNode() {}
