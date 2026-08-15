package btree

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"minidb/pager"
)

// openTestTree はテスト用の一時ディレクトリに db を作り、
// pager と btree を開いた状態にする。
func openTestTree(t *testing.T) (*pager.Pager, *BTree) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })

	tree, err := Open(pg)
	if err != nil {
		t.Fatal(err)
	}
	return pg, tree
}

// treeHeight は白箱検査用に、ページ id を根とする部分木の高さを
// 実際にページを辿って数える(リーフを 1 とする)。
func treeHeight(t *testing.T, pg *pager.Pager, id pager.PageID) int {
	t.Helper()
	page, err := pg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !isInterior(page) {
		return 1
	}

	it := interior{page: page}
	height := treeHeight(t, pg, it.rightmost())
	for i := 0; i < it.cellCount(); i++ {
		if h := treeHeight(t, pg, it.childAt(i)); h > height {
			height = h
		}
	}
	return height + 1
}

// 挿入した値がそのままのキーで検索できること、
// 存在しないキーの検索が ErrKeyNotFound になることを確認する。
func TestInsertAndSearch(t *testing.T) {
	_, tree := openTestTree(t)

	records := map[uint64]string{
		1: "Alice,30",
		2: "Bob,25",
		3: "Carol,41",
	}
	for key, value := range records {
		if err := tree.Insert(key, []byte(value)); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	for key, want := range records {
		got, err := tree.Search(key)
		if err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
		if string(got) != want {
			t.Fatalf("Search(%d) = %q, want %q", key, got, want)
		}
	}

	if _, err := tree.Search(99); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Search(99) error = %v, want ErrKeyNotFound", err)
	}
}

// キーを昇順以外の順序で挿入しても、全キーが検索でき、
// リーフ内部ではセルがキー昇順に並んでいることを確認する。
func TestOutOfOrderInsert(t *testing.T) {
	pg, tree := openTestTree(t)

	for _, key := range []uint64{5, 1, 3} {
		if err := tree.Insert(key, []byte("value")); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	for _, key := range []uint64{5, 1, 3} {
		if _, err := tree.Search(key); err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
	}

	// 白箱検査: リーフページのセルポインタ配列を先頭から読み、
	// キーが昇順に並んでいることを直接確認する。
	root, err := pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	page, err := pg.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	l := leaf{page: page}
	var prev uint64
	for i := 0; i < l.cellCount(); i++ {
		key := l.keyAt(i)
		if i > 0 && key <= prev {
			t.Fatalf("セルが昇順に並んでいません: keyAt(%d)=%d <= keyAt(%d)=%d", i, key, i-1, prev)
		}
		prev = key
	}
}

// 既存キーへの再挿入は ErrDuplicateKey になる。
func TestDuplicateKey(t *testing.T) {
	_, tree := openTestTree(t)

	if err := tree.Insert(1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert(1, []byte("second")); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("2 回目の Insert(1) error = %v, want ErrDuplicateKey", err)
	}
}

// MaxValueSize を超える値の挿入は ErrValueTooLarge になる。
func TestValueTooLarge(t *testing.T) {
	_, tree := openTestTree(t)

	value := make([]byte, MaxValueSize+1)
	if err := tree.Insert(1, value); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("Insert error = %v, want ErrValueTooLarge", err)
	}
}

// 大きめの値を複数件挿入してリーフページをあふれさせ、分割が起きることを確認する。
// 分割後も全キーが検索でき、ルートは内部ノード(0x05)へ成長しているはず。
func TestSplitLeaf(t *testing.T) {
	pg, tree := openTestTree(t)

	value := bytes.Repeat([]byte("x"), 400)
	const n = 20
	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	for key := uint64(1); key <= n; key++ {
		got, err := tree.Search(key)
		if err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
		if !bytes.Equal(got, value) {
			t.Fatalf("Search(%d) = %q, want %q", key, got, value)
		}
	}

	// 白箱検査: リーフ分割が起きていれば、ルートは内部ノードになっているはず。
	root, err := pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	page, err := pg.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if page.Data[0] != interiorPageType {
		t.Fatalf("ルートページの種別 = 0x%02x, want 0x%02x(内部ノード)", page.Data[0], interiorPageType)
	}
}

// キー 1..1000 を決定的な擬似ランダム順((i*7)%1000+1、gcd(7,1000)=1 なので
// 全キーをちょうど一巡する)で挿入し、Scan で回収したキー列が
// 昇順に 1..1000 と一致することを確認する。
func TestManyInsertsAndScan(t *testing.T) {
	_, tree := openTestTree(t)

	const n = 1000
	for i := 0; i < n; i++ {
		key := uint64((i*7)%n) + 1
		value := fmt.Sprintf("value-%d", key)
		if err := tree.Insert(key, []byte(value)); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	var got []uint64
	err := tree.Scan(func(key uint64, value []byte) error {
		got = append(got, key)
		if want := fmt.Sprintf("value-%d", key); string(value) != want {
			t.Fatalf("Scan: key=%d value=%q, want %q", key, value, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	if len(got) != n {
		t.Fatalf("Scan で得られた件数 = %d, want %d", len(got), n)
	}
	for i, key := range got {
		if want := uint64(i + 1); key != want {
			t.Fatalf("got[%d] = %d, want %d", i, key, want)
		}
	}
}

// 小さい値で 60000 件挿入し、木の高さが 3 以上に育つことを白箱検査で確認する。
// 全件は検索せず、代表的なキーだけを確認する。
func TestGrowToHeightThree(t *testing.T) {
	pg, tree := openTestTree(t)

	const n = 60000
	value := bytes.Repeat([]byte("v"), 8)
	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	root, err := pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	if h := treeHeight(t, pg, root); h < 3 {
		t.Fatalf("木の高さ = %d, want 3 以上", h)
	}

	for _, key := range []uint64{1, 30000, 60000} {
		if _, err := tree.Search(key); err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
	}
}

// pager を Close して開き直しても、挿入済みのデータが検索できることを確認する。
func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := Open(pg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert(1, []byte("Alice,30")); err != nil {
		t.Fatal(err)
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg2.Close()

	tree2, err := Open(pg2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tree2.Search(1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "Alice,30" {
		t.Fatalf("Search(1) = %q, want %q", got, "Alice,30")
	}
}

// 分割が起きるほど大量に挿入したあと Close して再 Open しても、
// Scan の件数と代表的なキーの検索が正しいことを確認する。
func TestPersistenceAfterSplit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	pg, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := Open(pg)
	if err != nil {
		t.Fatal(err)
	}

	const n = 1000
	for key := uint64(1); key <= n; key++ {
		value := fmt.Sprintf("value-%d", key)
		if err := tree.Insert(key, []byte(value)); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	pg2, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pg2.Close()

	tree2, err := Open(pg2)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	if err := tree2.Scan(func(key uint64, value []byte) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if count != n {
		t.Fatalf("Scan で得られた件数 = %d, want %d", count, n)
	}

	for _, key := range []uint64{1, 500, 1000} {
		if _, err := tree2.Search(key); err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
	}
}

// キーを挿入してから Delete すると、そのキーは検索できなくなり、
// 他のキーは残ることを確認する。
func TestDeleteAndSearch(t *testing.T) {
	_, tree := openTestTree(t)

	for key := uint64(1); key <= 5; key++ {
		if err := tree.Insert(key, []byte(fmt.Sprintf("value-%d", key))); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	if err := tree.Delete(3); err != nil {
		t.Fatalf("Delete(3) failed: %v", err)
	}

	if _, err := tree.Search(3); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Search(3) after Delete error = %v, want ErrKeyNotFound", err)
	}

	for _, key := range []uint64{1, 2, 4, 5} {
		got, err := tree.Search(key)
		if err != nil {
			t.Fatalf("Search(%d) failed: %v", key, err)
		}
		if want := fmt.Sprintf("value-%d", key); string(got) != want {
			t.Fatalf("Search(%d) = %q, want %q", key, got, want)
		}
	}
}

// 存在しないキーの Delete は ErrKeyNotFound になる。
func TestDeleteNotFound(t *testing.T) {
	_, tree := openTestTree(t)

	if err := tree.Insert(1, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := tree.Delete(99); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Delete(99) error = %v, want ErrKeyNotFound", err)
	}
}

// Delete したキーは、同じキーで再び Insert できる。
func TestDeleteAndReinsert(t *testing.T) {
	_, tree := openTestTree(t)

	if err := tree.Insert(1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := tree.Delete(1); err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert(1, []byte("second")); err != nil {
		t.Fatalf("再挿入に失敗: %v", err)
	}

	got, err := tree.Search(1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("Search(1) = %q, want %q", got, "second")
	}
}

// 1000 件挿入して木を高さ 2 以上に育てたあと全件削除すると、
// 木の高さがリーフ 1 枚(高さ 1)まで縮み、Scan が 0 件になり、
// 空いたページがフリーリストに回収されていることを確認する。
func TestDeleteAllShrinksTree(t *testing.T) {
	pg, tree := openTestTree(t)

	const n = 1000
	value := bytes.Repeat([]byte("v"), 8)
	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	root, err := pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	if h := treeHeight(t, pg, root); h < 2 {
		t.Fatalf("削除前の木の高さ = %d, want 2 以上", h)
	}

	for key := uint64(1); key <= n; key++ {
		if err := tree.Delete(key); err != nil {
			t.Fatalf("Delete(%d) failed: %v", key, err)
		}
	}

	root, err = pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	if h := treeHeight(t, pg, root); h != 1 {
		t.Fatalf("全件削除後の木の高さ = %d, want 1", h)
	}

	count := 0
	if err := tree.Scan(func(key uint64, value []byte) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("全件削除後の Scan 件数 = %d, want 0", count)
	}

	if got := pg.FreelistCount(); got == 0 {
		t.Fatal("全件削除後もフリーリストが空です")
	}
}

// 全件削除後に再び同じ件数を挿入すると、フリーリストのページが
// 再利用されて総ページ数が増えないことを確認する。
func TestReuseAfterDeleteAll(t *testing.T) {
	pg, tree := openTestTree(t)

	const n = 1000
	value := bytes.Repeat([]byte("v"), 8)
	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}
	numPagesAfterFirstInsert := pg.NumPages()

	for key := uint64(1); key <= n; key++ {
		if err := tree.Delete(key); err != nil {
			t.Fatalf("Delete(%d) failed: %v", key, err)
		}
	}

	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("再挿入 Insert(%d) failed: %v", key, err)
		}
	}

	if got := pg.NumPages(); got > numPagesAfterFirstInsert {
		t.Fatalf("再挿入後の NumPages = %d, 最初の挿入後の %d を超えています(フリーリストが再利用されていない)",
			got, numPagesAfterFirstInsert)
	}
}

// 値サイズが極端に偏り、二等分してもページに収まらない場合は、
// panic せず ErrPageFull が返ること、そして分割前の事前検査のおかげで
// 木が一切書き換えられていない(全キーが読める)ことを確認する。
func TestSplitUnbalancedValues(t *testing.T) {
	_, tree := openTestTree(t)

	small := bytes.Repeat([]byte("s"), 8)
	for key := uint64(1); key <= 100; key++ {
		if err := tree.Insert(key, small); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	huge := bytes.Repeat([]byte("h"), 4000)
	if err := tree.Insert(200, huge); !errors.Is(err, ErrPageFull) {
		t.Fatalf("巨大な値の Insert error = %v, want ErrPageFull", err)
	}

	// 失敗した挿入は木に何の変更も残していない。
	for key := uint64(1); key <= 100; key++ {
		if _, err := tree.Search(key); err != nil {
			t.Fatalf("ErrPageFull 後の Search(%d) failed: %v", key, err)
		}
	}
	if err := tree.Insert(101, small); err != nil {
		t.Fatalf("ErrPageFull 後の通常の Insert failed: %v", err)
	}
}
