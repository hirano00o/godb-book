// lexer.go は SQL 文字列を字句解析してトークン列に分割する。
// pager や btree には依存しない、純粋なテキスト処理。
package sql

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TokenKind はトークンの種別を表す。
type TokenKind int

const (
	TokenKeyword TokenKind = iota // SELECT, FROM, WHERE, CREATE, TABLE, INSERT, INTO, VALUES, UPDATE, SET, DELETE, INTEGER, TEXT, PRIMARY, KEY, BEGIN, COMMIT, ROLLBACK
	TokenIdent                    // テーブル名・列名
	TokenNumber                   // 整数リテラル(符号なし。負数はパーサが単項マイナスとして処理する)
	TokenString                   // '...' 文字列リテラル(Text にはエスケープ解決済みの中身が入る)
	TokenSymbol                   // ( ) , * ; = != < > <= >= -
	TokenEOF
)

// Token は字句解析結果の 1 単位。
type Token struct {
	Kind TokenKind
	Text string // キーワードは大文字に正規化される。文字列リテラルは引用符を除いた中身
	Pos  int    // 入力文字列中のバイト位置(エラーメッセージ用)
}

// keywords は識別子のうちキーワードとして扱う語の集合(大文字で保持)。
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "CREATE": true, "TABLE": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true,
	"DELETE": true, "INTEGER": true, "TEXT": true, "PRIMARY": true, "KEY": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true,
}

// Tokenize は input を字句解析してトークン列を返す。
// 末尾には必ず TokenEOF のトークンが 1 つ付く。
func Tokenize(input string) ([]Token, error) {
	var tokens []Token
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			tok, next, err := scanString(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i = next
		case isDigit(c):
			tok, next := scanNumber(input, i)
			tokens = append(tokens, tok)
			i = next
		case isIdentStart(c):
			tok, next := scanIdent(input, i)
			tokens = append(tokens, tok)
			i = next
		default:
			tok, next, err := scanSymbol(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i = next
		}
	}
	tokens = append(tokens, Token{Kind: TokenEOF, Pos: len(input)})
	return tokens, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

func scanNumber(input string, start int) (Token, int) {
	i := start
	for i < len(input) && isDigit(input[i]) {
		i++
	}
	return Token{Kind: TokenNumber, Text: input[start:i], Pos: start}, i
}

func scanIdent(input string, start int) (Token, int) {
	i := start
	for i < len(input) && isIdentPart(input[i]) {
		i++
	}
	text := input[start:i]
	upper := strings.ToUpper(text)
	if keywords[upper] {
		return Token{Kind: TokenKeyword, Text: upper, Pos: start}, i
	}
	return Token{Kind: TokenIdent, Text: text, Pos: start}, i
}

// scanString は先頭のクオート文字の位置 start から文字列リテラルを読み取る。
// クオート文字を 2 個連続させると、引用符自体を表すエスケープとして
// 1 個のクオート文字に解決する(SQLite 流)。
func scanString(input string, start int) (Token, int, error) {
	quote := input[start]
	var b strings.Builder
	i := start + 1
	for {
		if i >= len(input) {
			return Token{}, 0, fmt.Errorf("位置 %d: 閉じられていない文字列リテラルです", start)
		}
		if input[i] == quote {
			if i+1 < len(input) && input[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			i++
			break
		}
		b.WriteByte(input[i])
		i++
	}
	return Token{Kind: TokenString, Text: b.String(), Pos: start}, i, nil
}

// scanSymbol は記号 1 つを読み取る。<=, >=, != の 2 文字演算子は
// 1 文字目だけで判定しないよう先読みする。
func scanSymbol(input string, start int) (Token, int, error) {
	if start+1 < len(input) {
		switch input[start : start+2] {
		case "<=", ">=", "!=":
			return Token{Kind: TokenSymbol, Text: input[start : start+2], Pos: start}, start + 2, nil
		}
	}
	switch input[start] {
	case '(', ')', ',', '*', ';', '=', '<', '>', '-':
		return Token{Kind: TokenSymbol, Text: string(input[start]), Pos: start}, start + 1, nil
	}
	r, _ := utf8.DecodeRuneInString(input[start:])
	return Token{}, 0, fmt.Errorf("位置 %d: 予期しない文字 %q", start, r)
}
