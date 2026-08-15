// cmd/minidb/main.go
// minidb の対話的な REPL(Read-Eval-Print Loop)。
//
// 使い方:
//
//	go run ./cmd/minidb <db ファイル>
//
// 1 行に SQL 文を 1 つ入力するとその場で実行し、結果を表示する。
// ドットで始まる行はメタコマンドとして扱う(.exit / .tables / .explain)。
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"minidb/exec"
	"minidb/pager"
	"minidb/sql"
)

const prompt = "minidb> "

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "使い方: minidb <db ファイル>")
		os.Exit(1)
	}

	pg, err := pager.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pg.Close()

	engine, err := exec.NewEngine(pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 標準入力が端末(対話)かどうかを調べる。パイプやリダイレクトで
	// 実行例を採取するとき、表示を対話セッションと同じ見た目にするため。
	info, statErr := os.Stdin.Stat()
	interactive := statErr == nil && info.Mode()&os.ModeCharDevice != 0

	runREPL(engine, os.Stdin, os.Stdout, interactive)
}

// runREPL は 1 行 1 文の SQL(またはメタコマンド)を読み取って実行する
// メインループ。".exit" を読むか入力が尽きたら戻る。
// interactive が true なら読み取り前にプロンプトを表示し、false
// (パイプ経由)ならプロンプトに続けて読み取った行をエコーする。
func runREPL(engine *exec.Engine, in io.Reader, out io.Writer, interactive bool) {
	scanner := bufio.NewScanner(in)
	for {
		if interactive {
			fmt.Fprint(out, prompt)
		}
		if !scanner.Scan() {
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if !interactive {
			fmt.Fprintln(out, prompt+line)
		}
		switch {
		case line == "":
			continue
		case line == ".exit":
			return
		case strings.HasPrefix(line, "."):
			runMeta(engine, line, out)
		default:
			runStatement(engine, line, out)
		}
	}
}

// runMeta は "." で始まるメタコマンドを実行する。
func runMeta(engine *exec.Engine, line string, out io.Writer) {
	switch {
	case line == ".tables":
		names, err := engine.Tables()
		if err != nil {
			fmt.Fprintf(out, "エラー: %v\n", err)
			return
		}
		for _, name := range names {
			fmt.Fprintln(out, name)
		}

	case strings.HasPrefix(line, ".explain "):
		sqlText := strings.TrimSpace(strings.TrimPrefix(line, ".explain "))
		table, err := engine.ExplainSQL(sqlText)
		if err != nil {
			fmt.Fprintf(out, "エラー: %v\n", err)
			return
		}
		fmt.Fprint(out, table)

	default:
		fmt.Fprintf(out, "エラー: 不明なメタコマンドです: %s\n", line)
	}
}

// runStatement は SQL 文を 1 つ実行し、結果を表示する。
// エラーは "エラー: ..." として表示するだけで、REPL は続行する。
func runStatement(engine *exec.Engine, line string, out io.Writer) {
	res, err := engine.Execute(line)
	if err != nil {
		fmt.Fprintf(out, "エラー: %v\n", err)
		return
	}
	if res.Message != "" {
		fmt.Fprintln(out, res.Message)
		return
	}
	printRows(res, out)
}

// printRows は SELECT の結果を「列名ヘッダ + 区切り線 + 行」の単純な表で
// 表示する。列幅を揃える必要はなく、" | " 区切りで並べるだけでよい。
func printRows(res *exec.Result, out io.Writer) {
	header := strings.Join(res.Columns, " | ")
	fmt.Fprintln(out, header)
	fmt.Fprintln(out, strings.Repeat("-", len([]rune(header))))
	for _, row := range res.Rows {
		fmt.Fprintln(out, strings.Join(formatValues(row), " | "))
	}
}

// formatValues は 1 行分の値を表示用の文字列に変換する。
func formatValues(row []sql.Value) []string {
	cells := make([]string, len(row))
	for i, v := range row {
		if v.IsText {
			cells[i] = v.Text
		} else {
			cells[i] = fmt.Sprintf("%d", v.Int)
		}
	}
	return cells
}
