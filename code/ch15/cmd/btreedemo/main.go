// cmd/btreedemo/main.go
// btree を使って mini.db にキーと値を挿入する動作確認デモ。
// コマンドライン引数で挿入件数 N を指定できる(省略時は 3)。
// キー 1..N に値 "user<N>" を挿入するので、N を増やせばリーフ分割や
// 内部ノードの生成(第 7 章の実装)を実際に観察できる。
package main

import (
	"fmt"
	"os"
	"strconv"

	"minidb/btree"
	"minidb/pager"
)

func main() {
	n := 3
	if len(os.Args) > 1 {
		v, err := strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		n = v
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
		value := fmt.Sprintf("user%d", k)
		if err := tree.Insert(uint64(k), []byte(value)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if err := pg.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("mini.db に %d 件を挿入しました\n", n)
}
