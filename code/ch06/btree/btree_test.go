package btree

import (
	"bytes"
	"errors"
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

// リーフページが満杯になると ErrPageFull を返し、
// それまでに挿入したキーは引き続き検索できることを確認する。
func TestPageFull(t *testing.T) {
	_, tree := openTestTree(t)

	value := bytes.Repeat([]byte("x"), 400)

	var inserted []uint64
	var key uint64
	for {
		err := tree.Insert(key, value)
		if err != nil {
			if !errors.Is(err, ErrPageFull) {
				t.Fatalf("Insert(%d) error = %v, want ErrPageFull", key, err)
			}
			break
		}
		inserted = append(inserted, key)
		key++
	}

	if len(inserted) == 0 {
		t.Fatal("ErrPageFull になる前に 1 件も挿入できませんでした")
	}

	for _, k := range inserted {
		if _, err := tree.Search(k); err != nil {
			t.Fatalf("ErrPageFull 後の Search(%d) failed: %v", k, err)
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
