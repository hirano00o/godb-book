// cmd/inspect/main.go
// SQLite データベースファイルの先頭 100 バイト(ファイルヘッダ)と
// 各ページの B-Tree ページヘッダを解析して表示するツール。
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// SQLite ファイルヘッダ(先頭 100 バイト)のうち、本章で扱うフィールド。
// すべてビッグエンディアンで格納されている。
type FileHeader struct {
	Magic         [16]byte // "SQLite format 3\x00"
	PageSize      uint16   // オフセット 16: ページサイズ
	WriteVersion  uint8    // オフセット 18: 1=journal, 2=WAL
	ReadVersion   uint8    // オフセット 19: 1=journal, 2=WAL
	PageCount     uint32   // オフセット 28: ファイル内の総ページ数
	FreelistHead  uint32   // オフセット 32: フリーリスト先頭ページ番号
	FreelistCount uint32   // オフセット 36: フリーリストのページ数
	SchemaCookie  uint32   // オフセット 40: スキーマ変更のたびに増える
	TextEncoding  uint32   // オフセット 56: 1=UTF-8, 2=UTF-16le, 3=UTF-16be
}

// ReadFileHeader は r から 100 バイト読み取り、ヘッダとして解析する。
func ReadFileHeader(r io.Reader) (*FileHeader, error) {
	buf := make([]byte, 100)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("ヘッダの読み取りに失敗: %w", err)
	}

	h := &FileHeader{}
	copy(h.Magic[:], buf[0:16])
	if string(h.Magic[:]) != "SQLite format 3\x00" {
		return nil, fmt.Errorf("SQLite ファイルではありません")
	}

	h.PageSize = binary.BigEndian.Uint16(buf[16:18])
	h.WriteVersion = buf[18]
	h.ReadVersion = buf[19]
	h.PageCount = binary.BigEndian.Uint32(buf[28:32])
	h.FreelistHead = binary.BigEndian.Uint32(buf[32:36])
	h.FreelistCount = binary.BigEndian.Uint32(buf[36:40])
	h.SchemaCookie = binary.BigEndian.Uint32(buf[40:44])
	h.TextEncoding = binary.BigEndian.Uint32(buf[56:60])
	return h, nil
}

// B-Tree ページヘッダ。ページ先頭(ページ 1 だけはオフセット 100)に置かれる。
type BTreePageHeader struct {
	PageType    uint8  // 0x02/0x05=内部, 0x0a/0x0d=リーフ
	FreeBlock   uint16 // ページ内フリーブロックの先頭
	CellCount   uint16 // このページに入っているセル(レコード)の数
	CellContent uint16 // セル本体領域の開始オフセット
}

func pageTypeName(t uint8) string {
	switch t {
	case 0x02:
		return "index interior(インデックス内部ノード)"
	case 0x05:
		return "table interior(テーブル内部ノード)"
	case 0x0a:
		return "index leaf(インデックスリーフ)"
	case 0x0d:
		return "table leaf(テーブルリーフ)"
	default:
		return fmt.Sprintf("不明 (0x%02x)", t)
	}
}

// ReadBTreePageHeader は page(1 ページ分のバイト列)から
// B-Tree ページヘッダを解析する。pageNo が 1 のときだけ
// ファイルヘッダ 100 バイトの直後から始まる点に注意。
func ReadBTreePageHeader(page []byte, pageNo uint32) BTreePageHeader {
	offset := 0
	if pageNo == 1 {
		offset = 100
	}
	return BTreePageHeader{
		PageType:    page[offset],
		FreeBlock:   binary.BigEndian.Uint16(page[offset+1 : offset+3]),
		CellCount:   binary.BigEndian.Uint16(page[offset+3 : offset+5]),
		CellContent: binary.BigEndian.Uint16(page[offset+5 : offset+7]),
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "使い方: inspect <sqlite ファイル>")
		os.Exit(1)
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	h, err := ReadFileHeader(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println("=== ファイルヘッダ ===")
	fmt.Printf("マジック文字列   : %q\n", string(h.Magic[:15]))
	fmt.Printf("ページサイズ     : %d バイト\n", h.PageSize)
	fmt.Printf("総ページ数       : %d\n", h.PageCount)
	fmt.Printf("ジャーナル方式   : write=%d read=%d (1=rollback journal, 2=WAL)\n",
		h.WriteVersion, h.ReadVersion)
	fmt.Printf("フリーリスト     : 先頭ページ=%d, ページ数=%d\n",
		h.FreelistHead, h.FreelistCount)
	fmt.Printf("スキーマクッキー : %d\n", h.SchemaCookie)
	fmt.Printf("テキスト符号化   : %d (1=UTF-8)\n", h.TextEncoding)

	// 各ページの B-Tree ページヘッダを表示する
	fmt.Println("\n=== 各ページの B-Tree ヘッダ ===")
	page := make([]byte, h.PageSize)
	for no := uint32(1); no <= h.PageCount; no++ {
		// ページ番号 no は、ファイル先頭から (no-1)*PageSize バイト目
		if _, err := f.ReadAt(page, int64(no-1)*int64(h.PageSize)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bh := ReadBTreePageHeader(page, no)
		fmt.Printf("ページ %d: 種別=%s セル数=%d セル本体開始=%d\n",
			no, pageTypeName(bh.PageType), bh.CellCount, bh.CellContent)
	}
}
