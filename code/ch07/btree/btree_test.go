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
