// cmd/parsedemo/main.go
// SQL 文字列を 1 つ受け取り、字句解析したトークン列と、
// パースした構文木を表示するデモツール。
// sql パッケージのトークナイザ・パーサの動作を目視で確認するために使う。
package main

import (
	"fmt"
	"os"
	"strings"

	"minidb/sql"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "使い方: %s \"SQL 文\"\n", os.Args[0])
		os.Exit(1)
	}
	input := os.Args[1]

	tokens, err := sql.Tokenize(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("トークン列:")
	for _, tok := range tokens {
		fmt.Printf("  %-7s %-15q pos=%d\n", tokenKindName(tok.Kind), tok.Text, tok.Pos)
	}

	stmt, err := sql.Parse(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("\n構文木:")
	printStatement(stmt)
}

// tokenKindName は TokenKind を表示用の短い名前に変換する。
func tokenKindName(k sql.TokenKind) string {
	switch k {
	case sql.TokenKeyword:
		return "KEYWORD"
	case sql.TokenIdent:
		return "IDENT"
	case sql.TokenNumber:
		return "NUMBER"
	case sql.TokenString:
		return "STRING"
	case sql.TokenSymbol:
		return "SYMBOL"
	case sql.TokenEOF:
		return "EOF"
	default:
		return "?"
	}
}

// printStatement は構文木を文の種類ごとに整形して表示する。
// %#v による Go の内部表現ではなく、SQL の構造が読み取れる形にする。
func printStatement(stmt sql.Statement) {
	switch s := stmt.(type) {
	case *sql.CreateTableStmt:
		fmt.Printf("CreateTableStmt\n")
		fmt.Printf("  Table: %s\n", s.Table)
		fmt.Printf("  Columns:\n")
		for _, c := range s.Columns {
			pk := ""
			if c.PrimaryKey {
				pk = " PRIMARY KEY"
			}
			fmt.Printf("    - %s %s%s\n", c.Name, c.Type, pk)
		}

	case *sql.InsertStmt:
		fmt.Printf("InsertStmt\n")
		fmt.Printf("  Table: %s\n", s.Table)
		fmt.Printf("  Values:\n")
		for _, v := range s.Values {
			fmt.Printf("    - %s\n", formatValue(v))
		}

	case *sql.SelectStmt:
		fmt.Printf("SelectStmt\n")
		fmt.Printf("  Table: %s\n", s.Table)
		if s.Star {
			fmt.Printf("  Columns: *\n")
		} else {
			fmt.Printf("  Columns: %s\n", strings.Join(s.Columns, ", "))
		}
		printWhere(s.Where)

	case *sql.UpdateStmt:
		fmt.Printf("UpdateStmt\n")
		fmt.Printf("  Table: %s\n", s.Table)
		fmt.Printf("  Set:\n")
		for _, a := range s.Set {
			fmt.Printf("    - %s = %s\n", a.Column, formatValue(a.Value))
		}
		printWhere(s.Where)

	case *sql.DeleteStmt:
		fmt.Printf("DeleteStmt\n")
		fmt.Printf("  Table: %s\n", s.Table)
		printWhere(s.Where)

	default:
		fmt.Printf("%#v\n", stmt)
	}
}

// printWhere は WHERE 句を 1 段インデントして表示する。なければ "なし" と表示する。
func printWhere(w *sql.WhereClause) {
	if w == nil {
		fmt.Printf("  Where: なし\n")
		return
	}
	fmt.Printf("  Where: %s %s %s\n", w.Column, w.Op, formatValue(w.Value))
}

func formatValue(v sql.Value) string {
	if v.IsText {
		return fmt.Sprintf("%q", v.Text)
	}
	return fmt.Sprintf("%d", v.Int)
}
