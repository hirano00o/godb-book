// cmd/pagerdemo/main.go
// pager を使って mini.db を作り、1 ページ書き込む動作確認デモ。
package main

import (
	"fmt"
	"os"

	"minidb/pager"
)

func main() {
	p, err := pager.Open("mini.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	page, err := p.Allocate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	copy(page.Data[:], "hello, minidb")
	p.MarkDirty(page.ID)
	if err := p.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("mini.db を作成しました(2 ページ)")
}
