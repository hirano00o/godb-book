// Package btree は minidb のテーブル用 B-Tree を担当する。
// この章ではリーフページ 1 枚だけからなる木を扱う。
// ページが満杯になったときの分割(ノード数の増加)は第 7 章で実装する。
package btree

import (
	"encoding/binary"
	"errors"
	"sort"

	"minidb/pager"
)

const (
	leafPageType    = 0x0d // SQLite のテーブルリーフと同じ値
	leafHeaderSize  = 8    // 種別(1) + セル数(2) + セル本体開始(2) + 予約(3)
	cellPointerSize = 2
	// leafCellOverhead はセル 1 つあたりのキーと値の長さのバイト数。
	leafCellOverhead = 10 // キー(8) + 値の長さ(2)

	// MaxValueSize は 1 セルがページに収まるための値サイズ上限。
	MaxValueSize = pager.PageSize - leafHeaderSize - cellPointerSize - leafCellOverhead
)

var (
	ErrKeyNotFound   = errors.New("キーが見つかりません")
	ErrDuplicateKey  = errors.New("キーが重複しています")
	ErrValueTooLarge = errors.New("値が大きすぎます")
	ErrPageFull      = errors.New("ページに空きがありません(分割は第 7 章で実装)")
)

// BTree はキーと値の組を管理する索引構造。
// 現時点ではルートが常にリーフページであり、木の高さは 1 に固定される。
type BTree struct {
	pg   *pager.Pager
	root pager.PageID
}

// Open は pg 上の B-Tree を開く。ヘッダページにルートページ番号が
// 記録されていなければ、空のリーフページを 1 枚確保してルートにする。
func Open(pg *pager.Pager) (*BTree, error) {
	root, err := pg.RootPage()
	if err != nil {
		return nil, err
	}
	if root == 0 {
		page, err := pg.Allocate()
		if err != nil {
			return nil, err
		}
		initLeaf(page)
		pg.MarkDirty(page.ID)
		if err := pg.SetRootPage(page.ID); err != nil {
			return nil, err
		}
		root = page.ID
	}
	return &BTree{pg: pg, root: root}, nil
}

// Insert はキーと値を挿入する。
// 値が MaxValueSize を超える場合は ErrValueTooLarge、
// キーが既に存在する場合は ErrDuplicateKey、
// ページに空きがない場合は ErrPageFull を返す。
func (t *BTree) Insert(key uint64, value []byte) error {
	if len(value) > MaxValueSize {
		return ErrValueTooLarge
	}

	page, err := t.pg.Get(t.root)
	if err != nil {
		return err
	}
	l := leaf{page: page}

	idx, found := l.findSlot(key)
	if found {
		return ErrDuplicateKey
	}
	if err := l.insert(idx, key, value); err != nil {
		return err
	}
	t.pg.MarkDirty(page.ID)
	return nil
}

// Search はキーに対応する値を検索する。
// 見つかった場合はページの内容をコピーして返す(呼び出し側に
// ページ内部のメモリを直接握らせないため)。見つからなければ
// ErrKeyNotFound を返す。
func (t *BTree) Search(key uint64) ([]byte, error) {
	page, err := t.pg.Get(t.root)
	if err != nil {
		return nil, err
	}
	l := leaf{page: page}

	idx, found := l.findSlot(key)
	if !found {
		return nil, ErrKeyNotFound
	}

	src := l.valueAt(idx)
	value := make([]byte, len(src))
	copy(value, src)
	return value, nil
}

// leaf はリーフページ 1 枚を薄くラップし、ページ内のバイト列を
// キー・値の単位で読み書きするための型。
//
// ページレイアウト:
//
//	オフセット 0    : 種別(0x0d 固定)
//	オフセット 1-2  : セル数(ビッグエンディアン uint16)
//	オフセット 3-4  : セル本体開始オフセット(ビッグエンディアン uint16)
//	オフセット 5-7  : 予約領域
//	オフセット 8-   : セルポインタ配列(各 2 バイト、セル数ぶん)
//	...
//	末尾から手前へ  : セル本体(キー 8 バイト + 値長 2 バイト + 値)
//
// セルポインタ配列はキーの昇順を保つ。セル本体はページ末尾側から
// 詰めて書き込み、断片化を避けるためポインタ配列とは逆方向に伸びる。
type leaf struct {
	page *pager.Page
}

// initLeaf はページをセル数 0 の空のリーフとして初期化する。
func initLeaf(page *pager.Page) {
	page.Data[0] = leafPageType
	l := leaf{page: page}
	l.setCellCount(0)
	l.setContentStart(pager.PageSize)
}

func (l leaf) cellCount() int {
	return int(binary.BigEndian.Uint16(l.page.Data[1:3]))
}

func (l leaf) setCellCount(n int) {
	binary.BigEndian.PutUint16(l.page.Data[1:3], uint16(n))
}

func (l leaf) contentStart() int {
	return int(binary.BigEndian.Uint16(l.page.Data[3:5]))
}

func (l leaf) setContentStart(offset int) {
	binary.BigEndian.PutUint16(l.page.Data[3:5], uint16(offset))
}

// pointerOffset はセルポインタ配列の i 番目の要素の位置を返す。
func (l leaf) pointerOffset(i int) int {
	return leafHeaderSize + cellPointerSize*i
}

func (l leaf) cellOffset(i int) int {
	p := l.pointerOffset(i)
	return int(binary.BigEndian.Uint16(l.page.Data[p : p+2]))
}

func (l leaf) setCellOffset(i, offset int) {
	p := l.pointerOffset(i)
	binary.BigEndian.PutUint16(l.page.Data[p:p+2], uint16(offset))
}

func (l leaf) keyAt(i int) uint64 {
	off := l.cellOffset(i)
	return binary.BigEndian.Uint64(l.page.Data[off : off+8])
}

func (l leaf) valueAt(i int) []byte {
	off := l.cellOffset(i)
	vlen := int(binary.BigEndian.Uint16(l.page.Data[off+8 : off+10]))
	return l.page.Data[off+10 : off+10+vlen]
}

// findSlot はキーが入るべきセルポインタ配列上の位置を二分探索で求める。
// 戻り値の bool は、そのキーが既に存在するかどうかを表す。
func (l leaf) findSlot(key uint64) (int, bool) {
	n := l.cellCount()
	i := sort.Search(n, func(i int) bool {
		return l.keyAt(i) >= key
	})
	if i < n && l.keyAt(i) == key {
		return i, true
	}
	return i, false
}

// insert はセルポインタ配列の idx 番目にキーと値のセルを挿入する。
// 空き容量が足りない場合は ErrPageFull を返す。
func (l leaf) insert(idx int, key uint64, value []byte) error {
	cellSize := leafCellOverhead + len(value)
	n := l.cellCount()

	// 空き容量 = セル本体開始位置 - (ヘッダ + 既存ポインタ配列)
	free := l.contentStart() - (leafHeaderSize + cellPointerSize*n)
	if free < cellSize+cellPointerSize {
		return ErrPageFull
	}

	// セル本体はセル本体開始位置の手前に詰めて書き込む。
	newStart := l.contentStart() - cellSize
	binary.BigEndian.PutUint64(l.page.Data[newStart:newStart+8], key)
	binary.BigEndian.PutUint16(l.page.Data[newStart+8:newStart+10], uint16(len(value)))
	copy(l.page.Data[newStart+10:newStart+10+len(value)], value)
	l.setContentStart(newStart)

	// ポインタ配列の idx 以降を 1 つ後ろへずらし、挿入位置を空ける。
	for i := n; i > idx; i-- {
		l.setCellOffset(i, l.cellOffset(i-1))
	}
	l.setCellOffset(idx, newStart)
	l.setCellCount(n + 1)
	return nil
}
