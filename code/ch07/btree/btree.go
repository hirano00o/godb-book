// Package btree は minidb のテーブル用 B-Tree を担当する。
// リーフページが満杯になると分割し、内部ノードを介した複数ページの
// 木構造へと成長する。
package btree

import (
	"encoding/binary"
	"errors"
	"slices"
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

	interiorPageType   = 0x05 // SQLite のテーブル内部ノードと同じ値
	interiorHeaderSize = 12   // 種別(1) + セル数(2) + セル本体開始(2) + 右端の子(4) + 予約(3)
	interiorCellSize   = 12   // キー(8) + 子ページ番号(4)、内部ノードのセルは固定長

	// maxInteriorCells は 1 ページに収まる内部ノードのセル数の上限。
	maxInteriorCells = (pager.PageSize - interiorHeaderSize) / (interiorCellSize + cellPointerSize)
)

var (
	ErrKeyNotFound   = errors.New("キーが見つかりません")
	ErrDuplicateKey  = errors.New("キーが重複しています")
	ErrValueTooLarge = errors.New("値が大きすぎます")
	ErrPageFull      = errors.New("ページに空きがありません")
)

// BTree はキーと値の組を管理する索引構造。
// ルートページ番号は木が成長する(ルートが割れる)たびに変わる。
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

// splitResult は子ページの分割が発生したときに親へ伝える情報。
// sep 以下のキーは分割された元のページが、sep より大きいキーは
// 新しいページ right が受け持つ。
type splitResult struct {
	sep   uint64
	right pager.PageID
}

// Insert はキーと値を挿入する。
// 値が MaxValueSize を超える場合は ErrValueTooLarge、
// キーが既に存在する場合は ErrDuplicateKey を返す。
func (t *BTree) Insert(key uint64, value []byte) error {
	if len(value) > MaxValueSize {
		return ErrValueTooLarge
	}

	split, err := t.insertInto(t.root, key, value)
	if err != nil {
		return err
	}
	if split == nil {
		return nil
	}

	// ルートが割れた: 新しい内部ページをルートにし、木を 1 段高くする。
	// SQLite はルートページ番号を固定して中身だけを移すが、
	// minidb では単純さを優先し、成長のたびにルートページ番号が変わる。
	newRoot, err := t.pg.Allocate()
	if err != nil {
		return err
	}
	initInterior(newRoot, split.right)
	interior{page: newRoot}.appendCell(split.sep, t.root)
	t.pg.MarkDirty(newRoot.ID)

	if err := t.pg.SetRootPage(newRoot.ID); err != nil {
		return err
	}
	t.root = newRoot.ID
	return nil
}

// Search はキーに対応する値を検索する。
// 見つかった場合はページの内容をコピーして返す(呼び出し側に
// ページ内部のメモリを直接握らせないため)。見つからなければ
// ErrKeyNotFound を返す。
func (t *BTree) Search(key uint64) ([]byte, error) {
	pageID := t.root
	for {
		page, err := t.pg.Get(pageID)
		if err != nil {
			return nil, err
		}
		if !isInterior(page) {
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
		pageID = interior{page: page}.findChild(key)
	}
}

// Scan は全キーを昇順にたどり、キーと値のペアごとに fn を呼ぶ。
// fn がエラーを返したら走査を打ち切りそのエラーを返す。
func (t *BTree) Scan(fn func(key uint64, value []byte) error) error {
	return t.scanPage(t.root, fn)
}

// scanPage はページ pageID を根とする部分木を中間順に走査する。
// 内部ノードでは、セル 0, セル 1, ..., 右端の子の順に子を辿る。
func (t *BTree) scanPage(pageID pager.PageID, fn func(key uint64, value []byte) error) error {
	page, err := t.pg.Get(pageID)
	if err != nil {
		return err
	}

	if !isInterior(page) {
		l := leaf{page: page}
		for i := 0; i < l.cellCount(); i++ {
			src := l.valueAt(i)
			value := make([]byte, len(src))
			copy(value, src)
			if err := fn(l.keyAt(i), value); err != nil {
				return err
			}
		}
		return nil
	}

	it := interior{page: page}
	for i := 0; i < it.cellCount(); i++ {
		if err := t.scanPage(it.childAt(i), fn); err != nil {
			return err
		}
	}
	return t.scanPage(it.rightmost(), fn)
}

// isInterior はページが内部ノードかどうかを、先頭バイトの種別で判定する。
func isInterior(page *pager.Page) bool {
	return page.Data[0] == interiorPageType
}

// insertInto はページ pageID を根とする部分木にキーと値を再帰的に挿入する。
// そのページ自身が分割された場合は splitResult を返す(分割しなければ nil)。
func (t *BTree) insertInto(pageID pager.PageID, key uint64, value []byte) (*splitResult, error) {
	page, err := t.pg.Get(pageID)
	if err != nil {
		return nil, err
	}
	if isInterior(page) {
		return t.insertIntoInterior(page, key, value)
	}
	return t.insertIntoLeaf(page, key, value)
}

// insertIntoLeaf はリーフページへの挿入を試み、あふれた場合は splitLeaf で分割する。
func (t *BTree) insertIntoLeaf(page *pager.Page, key uint64, value []byte) (*splitResult, error) {
	l := leaf{page: page}
	idx, found := l.findSlot(key)
	if found {
		return nil, ErrDuplicateKey
	}

	err := l.insert(idx, key, value)
	if err == nil {
		t.pg.MarkDirty(page.ID)
		return nil, nil
	}
	if !errors.Is(err, ErrPageFull) {
		return nil, err
	}

	sep, right, err := splitLeaf(t.pg, l, idx, key, value)
	if err != nil {
		return nil, err
	}
	return &splitResult{sep: sep, right: right}, nil
}

// insertIntoInterior は key を含みうる子へ再帰的に挿入し、子が割れて
// 戻ってきた場合はこのページのセル列に (sep, 新ページ) を組み込む。
// 組み込んだ結果このページ自身があふれた場合は splitInterior で分割する。
func (t *BTree) insertIntoInterior(page *pager.Page, key uint64, value []byte) (*splitResult, error) {
	it := interior{page: page}
	childID := it.findChild(key)

	split, err := t.insertInto(childID, key, value)
	if err != nil || split == nil {
		return nil, err
	}

	cells := it.readCells()
	rightmost := it.rightmost()

	inserted := false
	for i, c := range cells {
		if c.child == childID {
			// 子 childID はセル i の子だった: セル i の子を新ページに差し替え、
			// その手前に (sep, 元の子) を挿入する。
			cells[i].child = split.right
			cells = slices.Insert(cells, i, interiorCell{key: split.sep, child: childID})
			inserted = true
			break
		}
	}
	if !inserted {
		// 子 childID は右端の子だった: 末尾に (sep, 元の子) を追加し、
		// 右端の子を新ページに差し替える。
		cells = append(cells, interiorCell{key: split.sep, child: childID})
		rightmost = split.right
	}

	if len(cells) <= maxInteriorCells {
		it.writeCells(cells, rightmost)
		t.pg.MarkDirty(page.ID)
		return nil, nil
	}

	sep, right, err := splitInterior(t.pg, it, cells, rightmost)
	if err != nil {
		return nil, err
	}
	return &splitResult{sep: sep, right: right}, nil
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

// leafCell はリーフノードのセル 1 つぶんの論理表現。分割のときだけ使う。
type leafCell struct {
	key   uint64
	value []byte
}

// splitLeaf はリーフページ l があふれて挿入できなかったときに呼ぶ。
// 挿入予定だったキーと値も含めた論理セル列をいったん組み立て、
// セル数で二等分してから元のページと新しいページに書き戻す
// (「分割してから改めて挿入」ではなく「挿入した状態で分割」する)。
//
// 戻り値は分割後の左ページの最大キー(セパレータ)と、新しいページの ID。
func splitLeaf(pg *pager.Pager, l leaf, idx int, key uint64, value []byte) (uint64, pager.PageID, error) {
	n := l.cellCount()
	cells := make([]leafCell, 0, n+1)
	for i := 0; i < idx; i++ {
		cells = append(cells, leafCell{key: l.keyAt(i), value: append([]byte(nil), l.valueAt(i)...)})
	}
	cells = append(cells, leafCell{key: key, value: value})
	for i := idx; i < n; i++ {
		cells = append(cells, leafCell{key: l.keyAt(i), value: append([]byte(nil), l.valueAt(i)...)})
	}

	// 左に入れる件数。奇数個は左を 1 件多くする。
	mid := (len(cells) + 1) / 2

	rightPage, err := pg.Allocate()
	if err != nil {
		return 0, 0, err
	}
	initLeaf(rightPage)
	right := leaf{page: rightPage}

	// 左は元のページをゼロクリアしてから詰め直す。
	// 値は上で cells にコピー済みなので、ここで元のバイト列を潰してよい。
	l.page.Data = [pager.PageSize]byte{}
	initLeaf(l.page)
	for i, c := range cells[:mid] {
		if err := l.insert(i, c.key, c.value); err != nil {
			return 0, 0, err
		}
	}
	for i, c := range cells[mid:] {
		if err := right.insert(i, c.key, c.value); err != nil {
			return 0, 0, err
		}
	}

	pg.MarkDirty(l.page.ID)
	pg.MarkDirty(rightPage.ID)

	sep := cells[mid-1].key
	return sep, rightPage.ID, nil
}

// interior は内部ページ 1 枚を薄くラップする。
//
// ページレイアウト:
//
//	オフセット 0    : 種別(0x05 固定)
//	オフセット 1-2  : セル数(ビッグエンディアン uint16)
//	オフセット 3-4  : セル本体開始オフセット(ビッグエンディアン uint16)
//	オフセット 5-8  : 右端の子ページ番号(ビッグエンディアン uint32)
//	オフセット 9-11 : 予約領域
//	オフセット 12-  : セルポインタ配列(各 2 バイト、セル数ぶん)
//	...
//	末尾から手前へ  : セル本体(キー 8 バイト + 子ページ番号 4 バイト、固定 12 バイト)
//
// セル i の子が指す部分木のキーはすべて「セル i のキー以下」である。
// 右端の子はどのセルのキーよりも大きいキーを持つ。セルはキー昇順に並ぶ。
type interior struct {
	page *pager.Page
}

// initInterior はページをセル数 0 の内部ノードとして初期化する。
// rightmost には初期状態での右端の子ページ番号を渡す。
func initInterior(page *pager.Page, rightmost pager.PageID) {
	page.Data[0] = interiorPageType
	it := interior{page: page}
	it.setCellCount(0)
	it.setContentStart(pager.PageSize)
	it.setRightmost(rightmost)
}

func (it interior) cellCount() int {
	return int(binary.BigEndian.Uint16(it.page.Data[1:3]))
}

func (it interior) setCellCount(n int) {
	binary.BigEndian.PutUint16(it.page.Data[1:3], uint16(n))
}

func (it interior) contentStart() int {
	return int(binary.BigEndian.Uint16(it.page.Data[3:5]))
}

func (it interior) setContentStart(offset int) {
	binary.BigEndian.PutUint16(it.page.Data[3:5], uint16(offset))
}

func (it interior) rightmost() pager.PageID {
	return pager.PageID(binary.BigEndian.Uint32(it.page.Data[5:9]))
}

func (it interior) setRightmost(id pager.PageID) {
	binary.BigEndian.PutUint32(it.page.Data[5:9], uint32(id))
}

func (it interior) pointerOffset(i int) int {
	return interiorHeaderSize + cellPointerSize*i
}

func (it interior) cellOffset(i int) int {
	p := it.pointerOffset(i)
	return int(binary.BigEndian.Uint16(it.page.Data[p : p+2]))
}

func (it interior) setCellOffset(i, offset int) {
	p := it.pointerOffset(i)
	binary.BigEndian.PutUint16(it.page.Data[p:p+2], uint16(offset))
}

func (it interior) keyAt(i int) uint64 {
	off := it.cellOffset(i)
	return binary.BigEndian.Uint64(it.page.Data[off : off+8])
}

func (it interior) childAt(i int) pager.PageID {
	off := it.cellOffset(i)
	return pager.PageID(binary.BigEndian.Uint32(it.page.Data[off+8 : off+12]))
}

// appendCell はセル本体開始位置の手前にセルを追記し、ポインタ配列の
// 末尾に登録する。writeCells がキー昇順のセル列を詰め直すときにだけ
// 使うので、途中への挿入は考慮しない。
func (it interior) appendCell(key uint64, child pager.PageID) {
	n := it.cellCount()
	newStart := it.contentStart() - interiorCellSize
	binary.BigEndian.PutUint64(it.page.Data[newStart:newStart+8], key)
	binary.BigEndian.PutUint32(it.page.Data[newStart+8:newStart+12], uint32(child))
	it.setContentStart(newStart)
	it.setCellOffset(n, newStart)
	it.setCellCount(n + 1)
}

// findChild は key を含みうる子のページ番号を二分探索で求める。
// 「セルのキー ≥ key」となる最初のセルの子を返す。
// どのセルのキーも key 未満なら、右端の子を返す。
func (it interior) findChild(key uint64) pager.PageID {
	n := it.cellCount()
	i := sort.Search(n, func(i int) bool {
		return it.keyAt(i) >= key
	})
	if i == n {
		return it.rightmost()
	}
	return it.childAt(i)
}

// interiorCell は内部ノードのセル 1 つぶんの論理表現。
type interiorCell struct {
	key   uint64
	child pager.PageID
}

// readCells はページ上のセルをすべて Go のスライスへ読み出す。
func (it interior) readCells() []interiorCell {
	n := it.cellCount()
	cells := make([]interiorCell, n)
	for i := 0; i < n; i++ {
		cells[i] = interiorCell{key: it.keyAt(i), child: it.childAt(i)}
	}
	return cells
}

// writeCells はページをゼロクリアして initInterior からセル列を詰め直す。
// cells はキー昇順であること。
func (it interior) writeCells(cells []interiorCell, rightmost pager.PageID) {
	it.page.Data = [pager.PageSize]byte{}
	initInterior(it.page, rightmost)
	for _, c := range cells {
		it.appendCell(c.key, c.child)
	}
}

// splitInterior は readCells + 追加セルであふれた内部ページ it を 2 つに分割する。
// セルは固定長なので、この分割は容量不足で失敗することがない。
//
// mid 番目のセルのキーを昇格させる。リーフの分割と異なり、内部ノードの
// キーは道標に過ぎないのでコピーではなく移動でよい(左右どちらのページにも
// 残す必要がない)。
//
// 左(元のページ it)にはセル 0..mid-1 を、右端の子には mid 番目のセルの子を置く。
// 右(新しいページ)にはセル mid+1..末尾を、右端の子には元の右端の子(rightmost)を置く。
func splitInterior(pg *pager.Pager, it interior, cells []interiorCell, rightmost pager.PageID) (uint64, pager.PageID, error) {
	mid := len(cells) / 2
	sep := cells[mid].key
	promotedChild := cells[mid].child

	rightPage, err := pg.Allocate()
	if err != nil {
		return 0, 0, err
	}
	right := interior{page: rightPage}
	right.writeCells(cells[mid+1:], rightmost)

	it.writeCells(cells[:mid], promotedChild)

	pg.MarkDirty(it.page.ID)
	pg.MarkDirty(rightPage.ID)
	return sep, rightPage.ID, nil
}
