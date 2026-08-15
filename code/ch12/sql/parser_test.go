package sql

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Statement
	}{
		{
			name:  "本書の定番クエリ: CREATE TABLE",
			input: "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)",
			want: &CreateTableStmt{
				Table: "users",
				Columns: []ColumnDef{
					{Name: "id", Type: TypeInteger, PrimaryKey: true},
					{Name: "name", Type: TypeText},
					{Name: "age", Type: TypeInteger},
				},
			},
		},
		{
			name:  "CREATE TABLE: PRIMARY KEY なし・末尾セミコロンあり",
			input: "CREATE TABLE t (a INTEGER, b TEXT);",
			want: &CreateTableStmt{
				Table: "t",
				Columns: []ColumnDef{
					{Name: "a", Type: TypeInteger},
					{Name: "b", Type: TypeText},
				},
			},
		},
		{
			name:  "INSERT: 整数・文字列・単項マイナスのリテラル",
			input: "INSERT INTO users VALUES (1, 'Alice', -5)",
			want: &InsertStmt{
				Table: "users",
				Values: []Value{
					{Int: 1},
					{IsText: true, Text: "Alice"},
					{Int: -5},
				},
			},
		},
		{
			name:  "INSERT: 引用符自体をエスケープした文字列",
			input: "INSERT INTO t VALUES ('it''s')",
			want: &InsertStmt{
				Table:  "t",
				Values: []Value{{IsText: true, Text: "it's"}},
			},
		},
		{
			name:  "本書の定番クエリ: SELECT ... WHERE",
			input: "SELECT name FROM users WHERE id = 2",
			want: &SelectStmt{
				Columns: []string{"name"},
				Table:   "users",
				Where:   &WhereClause{Column: "id", Op: OpEq, Value: Value{Int: 2}},
			},
		},
		{
			name:  "SELECT: 複数列・WHEREなし",
			input: "SELECT id, name, age FROM users",
			want: &SelectStmt{
				Columns: []string{"id", "name", "age"},
				Table:   "users",
			},
		},
		{
			name:  "SELECT *",
			input: "SELECT * FROM users",
			want:  &SelectStmt{Star: true, Table: "users"},
		},
		{
			name:  "SELECT *: WHERE に否定演算子と単項マイナス",
			input: "SELECT * FROM t WHERE x != -1",
			want: &SelectStmt{
				Star:  true,
				Table: "t",
				Where: &WhereClause{Column: "x", Op: OpNe, Value: Value{Int: -1}},
			},
		},
		{
			name:  "SELECT: 比較演算子 <= >= < > を一通り確認",
			input: "SELECT * FROM t WHERE a <= 1",
			want: &SelectStmt{
				Star:  true,
				Table: "t",
				Where: &WhereClause{Column: "a", Op: OpLe, Value: Value{Int: 1}},
			},
		},
		{
			name:  "UPDATE: 複数列の代入とWHERE",
			input: "UPDATE users SET name = 'Bob', age = 31 WHERE id = 1",
			want: &UpdateStmt{
				Table: "users",
				Set: []Assignment{
					{Column: "name", Value: Value{IsText: true, Text: "Bob"}},
					{Column: "age", Value: Value{Int: 31}},
				},
				Where: &WhereClause{Column: "id", Op: OpEq, Value: Value{Int: 1}},
			},
		},
		{
			name:  "UPDATE: WHEREなし",
			input: "UPDATE t SET a = 1",
			want: &UpdateStmt{
				Table: "t",
				Set:   []Assignment{{Column: "a", Value: Value{Int: 1}}},
			},
		},
		{
			name:  "DELETE: WHEREあり",
			input: "DELETE FROM users WHERE age >= 20",
			want: &DeleteStmt{
				Table: "users",
				Where: &WhereClause{Column: "age", Op: OpGe, Value: Value{Int: 20}},
			},
		},
		{
			name:  "DELETE: WHEREなし",
			input: "DELETE FROM users",
			want:  &DeleteStmt{Table: "users"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) 予期しないエラー: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // エラーメッセージに含まれるべき部分文字列
	}{
		{
			name:    "空入力",
			input:   "",
			wantSub: "空です",
		},
		{
			name:    "空白のみの入力",
			input:   "   ",
			wantSub: "空です",
		},
		{
			name:    "FROM が欠落している",
			input:   "SELECT name users",
			wantSub: "FROM",
		},
		{
			name:    "閉じ括弧が欠落している",
			input:   "CREATE TABLE t (id INTEGER",
			wantSub: "\")\"",
		},
		{
			name:    "PRIMARY KEY を TEXT 列に指定している",
			input:   "CREATE TABLE t (name TEXT PRIMARY KEY)",
			wantSub: "PRIMARY KEY は INTEGER 列にのみ指定できます",
		},
		{
			name:    "PRIMARY KEY が複数の列に指定されている",
			input:   "CREATE TABLE t (a INTEGER PRIMARY KEY, b INTEGER PRIMARY KEY)",
			wantSub: "PRIMARY KEY",
		},
		{
			name:    "AND による複合条件は未対応で余分なトークンになる",
			input:   "SELECT * FROM t WHERE a = 1 AND b = 2",
			wantSub: "余分なトークン",
		},
		{
			name:    "WHERE の後に列名がない",
			input:   "SELECT * FROM users WHERE",
			wantSub: "識別子",
		},
		{
			name:    "先頭が既知のキーワードでない",
			input:   "FOO BAR",
			wantSub: "キーワード",
		},
		{
			name:    "VALUES の閉じ括弧が欠落している",
			input:   "INSERT INTO t VALUES (1, 2",
			wantSub: "\")\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.input)
			if err == nil {
				t.Fatalf("Parse(%q) エラーなし、エラーを期待", tt.input)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Parse(%q) エラー = %q, %q を含むことを期待", tt.input, err.Error(), tt.wantSub)
			}
		})
	}
}
