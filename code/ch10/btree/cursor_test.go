package btree

import (
	"bytes"
	"fmt"
	"testing"
)

// 空の木に対する NewCursor は、いきなり Valid() が false になる。
func TestCursorEmptyTree(t *testing.T) {
	_, tree := openTestTree(t)

	c, err := tree.NewCursor()
	if err != nil {
		t.Fatal(err)
	}
	if c.Valid() {
		t.Fatal("空の木の Cursor.Valid() = true, want false")
	}
}

// 3 件を挿入した木を先頭から Next で読み進めると、キー昇順に全件回収できる。
func TestCursorReadThree(t *testing.T) {
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

	c, err := tree.NewCursor()
	if err != nil {
		t.Fatal(err)
	}

	var gotKeys []uint64
	for c.Valid() {
		key := c.Key()
		value := c.Value()
		if want := records[key]; string(value) != want {
			t.Fatalf("key=%d value=%q, want %q", key, value, want)
		}
		gotKeys = append(gotKeys, key)
		if err := c.Next(); err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}

	want := []uint64{1, 2, 3}
	if len(gotKeys) != len(want) {
		t.Fatalf("読み取ったキー数 = %d, want %d", len(gotKeys), len(want))
	}
	for i, k := range want {
		if gotKeys[i] != k {
			t.Fatalf("gotKeys[%d] = %d, want %d", i, gotKeys[i], k)
		}
	}
}

// リーフ分割が起きる規模(1000 件)で Next により全件を昇順に回収し、
// BTree.Scan の結果と一致することを確認する。
func TestCursorMatchesScan(t *testing.T) {
	_, tree := openTestTree(t)

	const n = 1000
	for i := 0; i < n; i++ {
		key := uint64((i*7)%n) + 1
		value := fmt.Sprintf("value-%d", key)
		if err := tree.Insert(key, []byte(value)); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	var wantKeys []uint64
	wantValues := make(map[uint64][]byte)
	if err := tree.Scan(func(key uint64, value []byte) error {
		wantKeys = append(wantKeys, key)
		wantValues[key] = append([]byte(nil), value...)
		return nil
	}); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	c, err := tree.NewCursor()
	if err != nil {
		t.Fatal(err)
	}
	var gotKeys []uint64
	for c.Valid() {
		key := c.Key()
		if !bytes.Equal(c.Value(), wantValues[key]) {
			t.Fatalf("key=%d value=%q, want %q", key, c.Value(), wantValues[key])
		}
		gotKeys = append(gotKeys, key)
		if err := c.Next(); err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}

	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("読み取ったキー数 = %d, want %d", len(gotKeys), len(wantKeys))
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("gotKeys[%d] = %d, want %d", i, gotKeys[i], wantKeys[i])
		}
	}
}

// Seek の 3 パターン: 完全一致・存在しないキー(次のキーに着地)・全キーより大きいキー。
func TestCursorSeek(t *testing.T) {
	_, tree := openTestTree(t)

	for _, key := range []uint64{10, 20, 30, 40, 50} {
		if err := tree.Insert(key, []byte(fmt.Sprintf("value-%d", key))); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	t.Run("完全一致", func(t *testing.T) {
		c, err := tree.NewCursor()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Seek(30); err != nil {
			t.Fatalf("Seek(30) failed: %v", err)
		}
		if !c.Valid() {
			t.Fatal("Seek(30) 後の Valid() = false, want true")
		}
		if c.Key() != 30 {
			t.Fatalf("Seek(30) 後の Key() = %d, want 30", c.Key())
		}
	})

	t.Run("存在しないキーは次のキーに着地", func(t *testing.T) {
		c, err := tree.NewCursor()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Seek(25); err != nil {
			t.Fatalf("Seek(25) failed: %v", err)
		}
		if !c.Valid() {
			t.Fatal("Seek(25) 後の Valid() = false, want true")
		}
		if c.Key() != 30 {
			t.Fatalf("Seek(25) 後の Key() = %d, want 30", c.Key())
		}
	})

	t.Run("全キーより大きいキーは Valid が false", func(t *testing.T) {
		c, err := tree.NewCursor()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Seek(100); err != nil {
			t.Fatalf("Seek(100) failed: %v", err)
		}
		if c.Valid() {
			t.Fatal("Seek(100) 後の Valid() = true, want false")
		}
	})
}

// 高さ 3 に育つ規模(60000 件)でも、Next で全件を正しくたどれることを確認する。
func TestCursorHeightThree(t *testing.T) {
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

	c, err := tree.NewCursor()
	if err != nil {
		t.Fatal(err)
	}
	count := uint64(0)
	for c.Valid() {
		count++
		if c.Key() != count {
			t.Fatalf("count=%d 番目のキー = %d, want %d", count, c.Key(), count)
		}
		if err := c.Next(); err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}
	if count != n {
		t.Fatalf("Cursor で読み取った件数 = %d, want %d", count, n)
	}
}

// 削除によって「リーフの実際の最大キーが親のセパレータより小さい」状態を
// 作ったとき、Seek がそのリーフで止まらず次のリーフへ進めることを確認する
// (セパレータは削除済みキーの亡霊として残るため)。
func TestCursorSeekAfterDelete(t *testing.T) {
	pg, tree := openTestTree(t)

	const n = 1000
	value := bytes.Repeat([]byte("v"), 8)
	for key := uint64(1); key <= n; key++ {
		if err := tree.Insert(key, value); err != nil {
			t.Fatalf("Insert(%d) failed: %v", key, err)
		}
	}

	// 白箱: ルート(内部ノード)の最初のセルのキー = 最初のリーフの最大キー。
	root, err := pg.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	page, err := pg.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if !isInterior(page) {
		t.Fatal("前提が崩れています: ルートが内部ノードではありません")
	}
	sep := interior{page: page}.keyAt(0)

	// セパレータと同じキーを削除する。親のセパレータは残ったままになる。
	if err := tree.Delete(sep); err != nil {
		t.Fatalf("Delete(%d) failed: %v", sep, err)
	}

	// sep 以上の最小キーは sep+1 のはず(連番なので)。
	c, err := tree.NewCursor()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Seek(sep); err != nil {
		t.Fatalf("Seek(%d) failed: %v", sep, err)
	}
	if !c.Valid() {
		t.Fatalf("Seek(%d) 後の Valid() = false, want true(次のリーフへ進むべき)", sep)
	}
	if got := c.Key(); got != sep+1 {
		t.Fatalf("Seek(%d) 後の Key() = %d, want %d", sep, got, sep+1)
	}
}
