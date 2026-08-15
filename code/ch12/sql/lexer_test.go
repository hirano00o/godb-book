package sql

import "testing"

func TestTokenize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Token
	}{
		{
			name:  "本書の定番クエリ",
			input: "SELECT name FROM users WHERE id = 2",
			want: []Token{
				{Kind: TokenKeyword, Text: "SELECT", Pos: 0},
				{Kind: TokenIdent, Text: "name", Pos: 7},
				{Kind: TokenKeyword, Text: "FROM", Pos: 12},
				{Kind: TokenIdent, Text: "users", Pos: 17},
				{Kind: TokenKeyword, Text: "WHERE", Pos: 23},
				{Kind: TokenIdent, Text: "id", Pos: 29},
				{Kind: TokenSymbol, Text: "=", Pos: 32},
				{Kind: TokenNumber, Text: "2", Pos: 34},
				{Kind: TokenEOF, Text: "", Pos: 35},
			},
		},
		{
			name:  "キーワードは大文字小文字を区別せず大文字に正規化される",
			input: "select * from t",
			want: []Token{
				{Kind: TokenKeyword, Text: "SELECT", Pos: 0},
				{Kind: TokenSymbol, Text: "*", Pos: 7},
				{Kind: TokenKeyword, Text: "FROM", Pos: 9},
				{Kind: TokenIdent, Text: "t", Pos: 14},
				{Kind: TokenEOF, Text: "", Pos: 15},
			},
		},
		{
			name:  "2文字の比較演算子",
			input: "<= >= !=",
			want: []Token{
				{Kind: TokenSymbol, Text: "<=", Pos: 0},
				{Kind: TokenSymbol, Text: ">=", Pos: 3},
				{Kind: TokenSymbol, Text: "!=", Pos: 6},
				{Kind: TokenEOF, Text: "", Pos: 8},
			},
		},
		{
			name:  "1文字の比較演算子は2文字版と誤認しない",
			input: "< > =",
			want: []Token{
				{Kind: TokenSymbol, Text: "<", Pos: 0},
				{Kind: TokenSymbol, Text: ">", Pos: 2},
				{Kind: TokenSymbol, Text: "=", Pos: 4},
				{Kind: TokenEOF, Text: "", Pos: 5},
			},
		},
		{
			name:  "単項マイナスは記号として1つのトークンになる",
			input: "-42",
			want: []Token{
				{Kind: TokenSymbol, Text: "-", Pos: 0},
				{Kind: TokenNumber, Text: "42", Pos: 1},
				{Kind: TokenEOF, Text: "", Pos: 3},
			},
		},
		{
			name:  "文字列リテラル",
			input: "'Alice'",
			want: []Token{
				{Kind: TokenString, Text: "Alice", Pos: 0},
				{Kind: TokenEOF, Text: "", Pos: 7},
			},
		},
		{
			name:  "空文字列リテラル",
			input: "''",
			want: []Token{
				{Kind: TokenString, Text: "", Pos: 0},
				{Kind: TokenEOF, Text: "", Pos: 2},
			},
		},
		{
			name:  "文字列中で引用符を2個連続させると引用符自体にエスケープされる",
			input: "'it''s'",
			want: []Token{
				{Kind: TokenString, Text: "it's", Pos: 0},
				{Kind: TokenEOF, Text: "", Pos: 7},
			},
		},
		{
			name:  "CREATE TABLE の定番クエリ",
			input: "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)",
			want: []Token{
				{Kind: TokenKeyword, Text: "CREATE", Pos: 0},
				{Kind: TokenKeyword, Text: "TABLE", Pos: 7},
				{Kind: TokenIdent, Text: "users", Pos: 13},
				{Kind: TokenSymbol, Text: "(", Pos: 19},
				{Kind: TokenIdent, Text: "id", Pos: 20},
				{Kind: TokenKeyword, Text: "INTEGER", Pos: 23},
				{Kind: TokenKeyword, Text: "PRIMARY", Pos: 31},
				{Kind: TokenKeyword, Text: "KEY", Pos: 39},
				{Kind: TokenSymbol, Text: ",", Pos: 42},
				{Kind: TokenIdent, Text: "name", Pos: 44},
				{Kind: TokenKeyword, Text: "TEXT", Pos: 49},
				{Kind: TokenSymbol, Text: ",", Pos: 53},
				{Kind: TokenIdent, Text: "age", Pos: 55},
				{Kind: TokenKeyword, Text: "INTEGER", Pos: 59},
				{Kind: TokenSymbol, Text: ")", Pos: 66},
				{Kind: TokenEOF, Text: "", Pos: 67},
			},
		},
		{
			name:  "末尾のセミコロンも記号として読み取る",
			input: "DELETE FROM t;",
			want: []Token{
				{Kind: TokenKeyword, Text: "DELETE", Pos: 0},
				{Kind: TokenKeyword, Text: "FROM", Pos: 7},
				{Kind: TokenIdent, Text: "t", Pos: 12},
				{Kind: TokenSymbol, Text: ";", Pos: 13},
				{Kind: TokenEOF, Text: "", Pos: 14},
			},
		},
		{
			name:  "識別子はキーワードと違い元の大文字小文字のまま",
			input: "SeLeCt Users",
			want: []Token{
				{Kind: TokenKeyword, Text: "SELECT", Pos: 0},
				{Kind: TokenIdent, Text: "Users", Pos: 7},
				{Kind: TokenEOF, Text: "", Pos: 12},
			},
		},
		{
			name:  "空入力はEOFのみ",
			input: "",
			want: []Token{
				{Kind: TokenEOF, Text: "", Pos: 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokenize(tt.input)
			if err != nil {
				t.Fatalf("Tokenize() 予期しないエラー: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("トークン数 = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("トークン[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTokenizeErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "未知の文字", input: "SELECT @ FROM t"},
		{name: "未終端の文字列リテラル", input: "SELECT 'abc FROM t"},
		{name: "末尾で終わる未終端の文字列リテラル", input: "'abc"},
		{name: "感嘆符単体では2文字演算子にならず未知の文字", input: "id ! 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Tokenize(tt.input)
			if err == nil {
				t.Fatalf("Tokenize(%q) エラーなし、エラーを期待", tt.input)
			}
		})
	}
}
