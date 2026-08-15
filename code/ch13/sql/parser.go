// parser.go は Tokenize が生成したトークン列を再帰下降パーサで構文木に変換する。
package sql

import (
	"fmt"
	"strconv"
)

// parser はトークン列を先頭から読み進める。
type parser struct {
	tokens []Token
	pos    int
}

// Parse は SQL 文字列 1 文を解析して構文木を返す。
func Parse(input string) (Statement, error) {
	tokens, err := Tokenize(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}

	tok := p.peek()
	if tok.Kind == TokenEOF {
		return nil, fmt.Errorf("位置 %d: SQL 文が空です", tok.Pos)
	}

	var stmt Statement
	switch {
	case p.isKeyword("CREATE"):
		stmt, err = p.parseCreateTable()
	case p.isKeyword("INSERT"):
		stmt, err = p.parseInsert()
	case p.isKeyword("SELECT"):
		stmt, err = p.parseSelect()
	case p.isKeyword("UPDATE"):
		stmt, err = p.parseUpdate()
	case p.isKeyword("DELETE"):
		stmt, err = p.parseDelete()
	case p.isKeyword("BEGIN"):
		stmt, err = p.parseBegin()
	case p.isKeyword("COMMIT"):
		stmt, err = p.parseCommit()
	case p.isKeyword("ROLLBACK"):
		stmt, err = p.parseRollback()
	default:
		return nil, fmt.Errorf("位置 %d: 文の先頭となるキーワードが必要ですが %q があります", tok.Pos, tok.Text)
	}
	if err != nil {
		return nil, err
	}

	if tok := p.peek(); tok.Kind == TokenSymbol && tok.Text == ";" {
		p.next()
	}
	if tok := p.peek(); tok.Kind != TokenEOF {
		return nil, fmt.Errorf("位置 %d: 余分なトークン %q があります", tok.Pos, tok.Text)
	}
	return stmt, nil
}

func (p *parser) peek() Token { return p.tokens[p.pos] }

func (p *parser) next() Token {
	tok := p.tokens[p.pos]
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return tok
}

func (p *parser) isKeyword(kw string) bool {
	tok := p.peek()
	return tok.Kind == TokenKeyword && tok.Text == kw
}

func (p *parser) expectKeyword(kw string) (Token, error) {
	tok := p.peek()
	if tok.Kind != TokenKeyword || tok.Text != kw {
		return Token{}, fmt.Errorf("位置 %d: %s が必要ですが %q があります", tok.Pos, kw, tok.Text)
	}
	return p.next(), nil
}

func (p *parser) expectSymbol(sym string) (Token, error) {
	tok := p.peek()
	if tok.Kind != TokenSymbol || tok.Text != sym {
		return Token{}, fmt.Errorf("位置 %d: %q が必要ですが %q があります", tok.Pos, sym, tok.Text)
	}
	return p.next(), nil
}

func (p *parser) expectIdent() (Token, error) {
	tok := p.peek()
	if tok.Kind != TokenIdent {
		return Token{}, fmt.Errorf("位置 %d: 識別子が必要ですが %q があります", tok.Pos, tok.Text)
	}
	return p.next(), nil
}

func (p *parser) expectNumber() (Token, error) {
	tok := p.peek()
	if tok.Kind != TokenNumber {
		return Token{}, fmt.Errorf("位置 %d: 数値が必要ですが %q があります", tok.Pos, tok.Text)
	}
	return p.next(), nil
}

// parseCreateTable は CREATE TABLE t (col INTEGER [PRIMARY KEY], col TEXT, ...) を解析する。
func (p *parser) parseCreateTable() (Statement, error) {
	if _, err := p.expectKeyword("CREATE"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectSymbol("("); err != nil {
		return nil, err
	}

	var columns []ColumnDef
	hasPrimaryKey := false
	for {
		col, primaryPos, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		if col.PrimaryKey {
			if hasPrimaryKey {
				return nil, fmt.Errorf("位置 %d: PRIMARY KEY は 1 つのテーブルに 1 つまでです", primaryPos)
			}
			hasPrimaryKey = true
		}
		columns = append(columns, col)

		if tok := p.peek(); tok.Kind == TokenSymbol && tok.Text == "," {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return &CreateTableStmt{Table: nameTok.Text, Columns: columns}, nil
}

// parseColumnDef は列定義 1 つを解析する。primaryPos は PRIMARY KEY が
// 指定されていた場合にその位置を返し(重複検出に使う)、なければ -1。
func (p *parser) parseColumnDef() (col ColumnDef, primaryPos int, err error) {
	primaryPos = -1

	nameTok, err := p.expectIdent()
	if err != nil {
		return ColumnDef{}, 0, err
	}

	var typ ColumnType
	switch {
	case p.isKeyword("INTEGER"):
		p.next()
		typ = TypeInteger
	case p.isKeyword("TEXT"):
		p.next()
		typ = TypeText
	default:
		tok := p.peek()
		return ColumnDef{}, 0, fmt.Errorf("位置 %d: 列の型(INTEGER または TEXT)が必要ですが %q があります", tok.Pos, tok.Text)
	}

	primaryKey := false
	if p.isKeyword("PRIMARY") {
		primaryTok := p.peek()
		p.next()
		if _, err := p.expectKeyword("KEY"); err != nil {
			return ColumnDef{}, 0, err
		}
		if typ != TypeInteger {
			return ColumnDef{}, 0, fmt.Errorf("位置 %d: PRIMARY KEY は INTEGER 列にのみ指定できます", primaryTok.Pos)
		}
		primaryKey = true
		primaryPos = primaryTok.Pos
	}

	return ColumnDef{Name: nameTok.Text, Type: typ, PrimaryKey: primaryKey}, primaryPos, nil
}

// parseInsert は INSERT INTO t VALUES (expr, ...) を解析する。
func (p *parser) parseInsert() (Statement, error) {
	if _, err := p.expectKeyword("INSERT"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	if _, err := p.expectSymbol("("); err != nil {
		return nil, err
	}

	var values []Value
	for {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		values = append(values, v)

		if tok := p.peek(); tok.Kind == TokenSymbol && tok.Text == "," {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return &InsertStmt{Table: nameTok.Text, Values: values}, nil
}

// parseValue はリテラル(整数、単項マイナス付き整数、または文字列)を解析する。
func (p *parser) parseValue() (Value, error) {
	tok := p.peek()

	if tok.Kind == TokenSymbol && tok.Text == "-" {
		p.next()
		numTok, err := p.expectNumber()
		if err != nil {
			return Value{}, err
		}
		n, err := strconv.ParseInt(numTok.Text, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("位置 %d: 数値リテラルが不正です: %v", numTok.Pos, err)
		}
		return Value{Int: -n}, nil
	}
	if tok.Kind == TokenNumber {
		p.next()
		n, err := strconv.ParseInt(tok.Text, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("位置 %d: 数値リテラルが不正です: %v", tok.Pos, err)
		}
		return Value{Int: n}, nil
	}
	if tok.Kind == TokenString {
		p.next()
		return Value{IsText: true, Text: tok.Text}, nil
	}
	return Value{}, fmt.Errorf("位置 %d: リテラル(数値または文字列)が必要ですが %q があります", tok.Pos, tok.Text)
}

// parseSelect は SELECT col, ... FROM t [WHERE cond] と
// SELECT * FROM t [WHERE cond] を解析する。
func (p *parser) parseSelect() (Statement, error) {
	if _, err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}

	stmt := &SelectStmt{}
	if tok := p.peek(); tok.Kind == TokenSymbol && tok.Text == "*" {
		p.next()
		stmt.Star = true
	} else {
		cols, err := p.parseColumnList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = cols
	}

	if _, err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	stmt.Table = nameTok.Text

	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	stmt.Where = where
	return stmt, nil
}

// parseColumnList は "col, col, ..." 形式の列名リストを解析する。
func (p *parser) parseColumnList() ([]string, error) {
	var cols []string
	for {
		tok, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		cols = append(cols, tok.Text)

		if next := p.peek(); next.Kind == TokenSymbol && next.Text == "," {
			p.next()
			continue
		}
		break
	}
	return cols, nil
}

// parseOptionalWhere は WHERE 句があれば解析し、なければ nil を返す。
func (p *parser) parseOptionalWhere() (*WhereClause, error) {
	if !p.isKeyword("WHERE") {
		return nil, nil
	}
	p.next()

	colTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	op, err := p.parseCompareOp()
	if err != nil {
		return nil, err
	}
	value, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return &WhereClause{Column: colTok.Text, Op: op, Value: value}, nil
}

// parseCompareOp は比較演算子(= != < > <= >=)を解析する。
func (p *parser) parseCompareOp() (CompareOp, error) {
	tok := p.peek()
	if tok.Kind != TokenSymbol {
		return 0, fmt.Errorf("位置 %d: 比較演算子が必要ですが %q があります", tok.Pos, tok.Text)
	}
	var op CompareOp
	switch tok.Text {
	case "=":
		op = OpEq
	case "!=":
		op = OpNe
	case "<":
		op = OpLt
	case ">":
		op = OpGt
	case "<=":
		op = OpLe
	case ">=":
		op = OpGe
	default:
		return 0, fmt.Errorf("位置 %d: 比較演算子が必要ですが %q があります", tok.Pos, tok.Text)
	}
	p.next()
	return op, nil
}

// parseUpdate は UPDATE t SET col = expr, ... [WHERE cond] を解析する。
func (p *parser) parseUpdate() (Statement, error) {
	if _, err := p.expectKeyword("UPDATE"); err != nil {
		return nil, err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}

	var assigns []Assignment
	for {
		colTok, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectSymbol("="); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		assigns = append(assigns, Assignment{Column: colTok.Text, Value: v})

		if tok := p.peek(); tok.Kind == TokenSymbol && tok.Text == "," {
			p.next()
			continue
		}
		break
	}

	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return &UpdateStmt{Table: nameTok.Text, Set: assigns, Where: where}, nil
}

// parseDelete は DELETE FROM t [WHERE cond] を解析する。
func (p *parser) parseDelete() (Statement, error) {
	if _, err := p.expectKeyword("DELETE"); err != nil {
		return nil, err
	}
	if _, err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return &DeleteStmt{Table: nameTok.Text, Where: where}, nil
}

// parseBegin は BEGIN 文を解析する。TRANSACTION キーワードは受け付けない。
func (p *parser) parseBegin() (Statement, error) {
	if _, err := p.expectKeyword("BEGIN"); err != nil {
		return nil, err
	}
	return &BeginStmt{}, nil
}

// parseCommit は COMMIT 文を解析する。
func (p *parser) parseCommit() (Statement, error) {
	if _, err := p.expectKeyword("COMMIT"); err != nil {
		return nil, err
	}
	return &CommitStmt{}, nil
}

// parseRollback は ROLLBACK 文を解析する。
func (p *parser) parseRollback() (Statement, error) {
	if _, err := p.expectKeyword("ROLLBACK"); err != nil {
		return nil, err
	}
	return &RollbackStmt{}, nil
}
