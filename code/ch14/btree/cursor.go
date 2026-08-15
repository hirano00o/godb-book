package btree

import (
	"sort"

	"minidb/pager"
)

// cursorFrame はカーソルがルートからリーフへ降りる経路上の 1 段を表す。
//
// pageID はページ番号だけを持ち、ページオブジェクト自体は保持しない。
// pager の LRU キャッシュはいつでもページを追い出しうるため、btree.go の
// insertIntoInterior / deleteFromInterior と同じ規約に従い、セルへ
// アクセスするたびに BTree.pg.Get(pageID) で取得し直す。
//
// index はこのページで「どの子へ降りたか」を表す(0..cellCount)。
// 0..cellCount-1 はセル index の子、cellCount は右端の子を表す。
type cursorFrame struct {
	pageID pager.PageID
	index  int
}

// Cursor は B-Tree のリーフセルを昇順に 1 つずつたどる読み取り専用カーソル。
// 内部ノードからリーフへ降りる経路を stack として保持する。
//
// このカーソルは読み取り専用の走査だけを目的とする。カーソルが生きている
// 間に対象の BTree へ Insert / Delete を行うことは想定していない
// (木の構造が変わると stack が指す位置の意味が崩れるため)。
type Cursor struct {
	t     *BTree
	stack []cursorFrame

	valid      bool
	leafPageID pager.PageID
	leafIndex  int
}

// NewCursor は t の先頭(最小キー)に位置づけたカーソルを返す。
// 木が空であれば、返されたカーソルは Valid() が false になる。
func (t *BTree) NewCursor() (*Cursor, error) {
	c := &Cursor{t: t}
	if err := c.descendLeftmost(t.root); err != nil {
		return nil, err
	}
	return c, nil
}

// Valid はカーソルが有効なセルを指しているかを返す(木が空、または末尾を超えたら false)。
func (c *Cursor) Valid() bool {
	return c.valid
}

// Key は現在のセルのキーを返す。Valid() が false のときの戻り値は未定義。
func (c *Cursor) Key() uint64 {
	page, err := c.t.pg.Get(c.leafPageID)
	if err != nil {
		return 0
	}
	return leaf{page: page}.keyAt(c.leafIndex)
}

// Value は現在のセルの値を返す(ページ内部のバイト列のコピー)。
// Valid() が false のときの戻り値は未定義。
func (c *Cursor) Value() []byte {
	page, err := c.t.pg.Get(c.leafPageID)
	if err != nil {
		return nil
	}
	src := leaf{page: page}.valueAt(c.leafIndex)
	value := make([]byte, len(src))
	copy(value, src)
	return value
}

// Next は次のセルへ進む。末尾を超えたら Valid() が false になる。
// 既に Valid() が false のカーソルに対しては何もしない(no-op)。
func (c *Cursor) Next() error {
	if !c.valid {
		return nil
	}

	page, err := c.t.pg.Get(c.leafPageID)
	if err != nil {
		return err
	}
	l := leaf{page: page}
	if c.leafIndex+1 < l.cellCount() {
		c.leafIndex++
		return nil
	}

	// リーフを読み切った: stack を 1 段ずつ遡り、まだ降りていない
	// 次の子を探す。
	return c.advanceFromStack()
}

// Seek はキー key 以上の最小のセルへ位置づける。完全一致検索を行いたい
// 場合は、呼び出し側が Valid() と Key() == key を確認すること。
// key 以上のキーが存在しなければ Valid() が false になる。
func (c *Cursor) Seek(key uint64) error {
	c.stack = c.stack[:0]

	pageID := c.t.root
	for {
		page, err := c.t.pg.Get(pageID)
		if err != nil {
			return err
		}
		if !isInterior(page) {
			l := leaf{page: page}
			idx, _ := l.findSlot(key)
			c.leafPageID = pageID
			c.leafIndex = idx
			if idx < l.cellCount() {
				c.valid = true
				return nil
			}
			// このリーフに key 以上のセルがない。削除によってリーフの
			// 実際の最大キーが親のセパレータより小さくなっている場合に
			// 起こる。key 以上のキーは次のリーフ以降にあるかもしれない
			// ので、stack を使って次のリーフの先頭へ進む
			// (advanceFromStack が valid を適切に設定し直す)。
			return c.advanceFromStack()
		}

		it := interior{page: page}
		idx := seekChildIndex(it, key)
		c.stack = append(c.stack, cursorFrame{pageID: pageID, index: idx})
		pageID = childAtIndex(it, idx)
	}
}

// descendLeftmost はページ pageID を根とする部分木の最左のリーフまで
// 常に子 0 を選んで降り、通過した内部ノードを stack に積む。
// 到達したリーフにセルが 1 つもなければ(空の木)カーソルを無効にする。
func (c *Cursor) descendLeftmost(pageID pager.PageID) error {
	for {
		page, err := c.t.pg.Get(pageID)
		if err != nil {
			return err
		}
		if !isInterior(page) {
			l := leaf{page: page}
			c.leafPageID = pageID
			c.leafIndex = 0
			c.valid = l.cellCount() > 0
			return nil
		}

		c.stack = append(c.stack, cursorFrame{pageID: pageID, index: 0})
		pageID = childAtIndex(interior{page: page}, 0)
	}
}

// advanceFromStack はリーフを読み終えたときに呼ばれる。stack を末尾から
// 遡り、まだ降りていない次の子が見つかったページからその子の最左の
// リーフへ再び降りる。どの段でも次の子が見つからなければ(木全体を
// 読み終えた)stack が空になり、カーソルを無効にする。
func (c *Cursor) advanceFromStack() error {
	for len(c.stack) > 0 {
		top := &c.stack[len(c.stack)-1]
		page, err := c.t.pg.Get(top.pageID)
		if err != nil {
			return err
		}
		it := interior{page: page}

		top.index++
		if top.index > it.cellCount() {
			// このページの子はすべて辿り終えた: 1 段上へ。
			c.stack = c.stack[:len(c.stack)-1]
			continue
		}
		return c.descendLeftmost(childAtIndex(it, top.index))
	}

	c.valid = false
	return nil
}

// seekChildIndex は key 以上の最小のセルの位置を二分探索で返す。
// 該当するセルがなければ cellCount(右端の子を表す)を返す。
// interior.findChild と同じ探索だが、子ページ番号ではなく
// カーソルの stack フレームに積むための位置(インデックス)を返す。
func seekChildIndex(it interior, key uint64) int {
	n := it.cellCount()
	return sort.Search(n, func(i int) bool {
		return it.keyAt(i) >= key
	})
}

// childAtIndex はカーソルのフレームが持つインデックス(0..cellCount)に
// 対応する子ページ番号を返す。index == cellCount は右端の子を表す。
func childAtIndex(it interior, index int) pager.PageID {
	if index == it.cellCount() {
		return it.rightmost()
	}
	return it.childAt(index)
}
