// cmd/btreedemo/main.go
// btree を使って mini.db にキーと値を挿入する動作確認デモ。
package main

import (
	"fmt"
	"os"

	"minidb/btree"
	"minidb/pager"
)

func main() {
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

	records := []struct {
		key   uint64
		value string
	}{
		{1, "Alice,30"},
		{2, "Bob,25"},
		{3, "Carol,41"},
	}
	for _, r := range records {
		if err := tree.Insert(r.key, []byte(r.value)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if err := pg.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("mini.db に 3 件を挿入しました")
}
