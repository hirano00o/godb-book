// cmd/treedump/main.go
// mini.db を開き、ルートページから B-Tree をたどって各ページの種別・
// セル数・(内部ノードなら)セルの一覧と木の高さを表示するツール。
// SQLite ファイルを対象にした cmd/inspect の、minidb 版に相当する。
//
// ページの中身は btree パッケージが管理する非公開の型だが、
// バイト列としてのレイアウトは第 5・7 章で解説した固定フォーマットなので、
// inspect と同様にここでも自前でバイト列を解析する。
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"minidb/pager"
)

const (
	leafPageType       = 0x0d
	interiorPageType   = 0x05
	interiorHeaderSize = 12
	cellPointerSize    = 2

	// maxShownCells は内部ノードのセルを先頭から何個まで表示するかの上限。
	maxShownCells = 5
)

func main() {
	path := "mini.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	pg, err := pager.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pg.Close()

	root, err := pg.RootPage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if root == 0 {
		fmt.Println("B-Tree はまだ作られていません")
		return
	}

	height, err := dumpPage(pg, root, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\n木の高さ: %d (ルート = ページ %d)\n", height, root)
	fmt.Printf("総ページ数: %d\n", pg.NumPages())
	fmt.Printf("フリーリスト: 先頭=%d ページ数=%d\n", pg.FreelistHead(), pg.FreelistCount())
}

// interiorCell は表示用の (キー, 子ページ番号) の組。
type interiorCell struct {
	key   uint64
	child pager.PageID
}

// dumpPage はページ id を根とする部分木を、インデントで階層を表しながら
// 表示する。戻り値はその部分木の高さ(リーフを 1 とする)。
func dumpPage(pg *pager.Pager, id pager.PageID, depth int) (int, error) {
	page, err := pg.Get(id)
	if err != nil {
		return 0, err
	}
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}

	cellCount := int(binary.BigEndian.Uint16(page.Data[1:3]))

	if page.Data[0] == leafPageType {
		fmt.Printf("%sページ %d: リーフ セル数=%d\n", indent, id, cellCount)
		return 1, nil
	}

	rightmost := pager.PageID(binary.BigEndian.Uint32(page.Data[5:9]))
	fmt.Printf("%sページ %d: 内部ノード セル数=%d 右端の子=%d\n", indent, id, cellCount, rightmost)

	cells := make([]interiorCell, cellCount)
	for i := 0; i < cellCount; i++ {
		p := interiorHeaderSize + cellPointerSize*i
		off := int(binary.BigEndian.Uint16(page.Data[p : p+2]))
		cells[i] = interiorCell{
			key:   binary.BigEndian.Uint64(page.Data[off : off+8]),
			child: pager.PageID(binary.BigEndian.Uint32(page.Data[off+8 : off+12])),
		}
	}

	shown := cells
	omitted := 0
	if len(shown) > maxShownCells {
		omitted = len(shown) - maxShownCells
		shown = shown[:maxShownCells]
	}
	for _, c := range shown {
		fmt.Printf("%s  セル: キー=%d 子=%d\n", indent, c.key, c.child)
	}
	if omitted > 0 {
		fmt.Printf("%s  ...(他 %d 件省略)\n", indent, omitted)
	}

	height := 0
	for _, c := range cells {
		h, err := dumpPage(pg, c.child, depth+1)
		if err != nil {
			return 0, err
		}
		if h > height {
			height = h
		}
	}
	h, err := dumpPage(pg, rightmost, depth+1)
	if err != nil {
		return 0, err
	}
	if h > height {
		height = h
	}
	return height + 1, nil
}
