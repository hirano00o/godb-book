// cmd/deletedemo/main.go
// btree を使って mini.db からキーを削除する動作確認デモ。
// コマンドライン引数で削除件数 N を指定する(必須)。
// キー 1..N を Delete するので、cmd/btreedemo で挿入したデータを
// 削除しながら木が縮んでいく様子を cmd/treedump で観察できる。
package main

import (
	"fmt"
	"os"
	"strconv"

	"minidb/btree"
	"minidb/pager"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "使い方: deletedemo <削除件数 N>")
		os.Exit(1)
	}
	n, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pg, err := pager.Open("mini.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	tree, err := btree.Open(pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for k := 1; k <= n; k++ {
		if err := tree.Delete(uint64(k)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if err := pg.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("mini.db からキー 1〜%d を削除しました\n", n)
}
