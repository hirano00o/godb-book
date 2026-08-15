package pager

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// 新規作成 → ページ確保 → 書き込み → 閉じる → 開き直して読める、
// という一連の流れを検証する。
func TestPagerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// --- 書き込み側 ---
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.NumPages(); got != 1 {
		t.Fatalf("新規ファイルの総ページ数 = %d, want 1", got)
	}

	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "hello, minidb")
	p.MarkDirty(page.ID)

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// --- 読み込み側 ---
	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if got := p2.NumPages(); got != 2 {
		t.Fatalf("再オープン後の総ページ数 = %d, want 2", got)
	}

	loaded, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(loaded.Data[:], []byte("hello, minidb")) {
		t.Fatalf("ページ内容が一致しません: %q", loaded.Data[:16])
	}
}

// 存在しないページの取得はエラーになる。
func TestGetOutOfRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Get(99); err == nil {
		t.Fatal("存在しないページの取得がエラーになりませんでした")
	}
}

// minidb ファイルでないものを開くとエラーになる。
func TestOpenInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-db")
	if err := writeJunk(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("不正なファイルのオープンがエラーになりませんでした")
	}
}

// ルートページ番号がヘッダページに正しく記録され、
// 再オープン後も読み戻せることを検証する。
func TestRootPageRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("新規ファイルの RootPage = %d, want 0", got)
	}

	if err := p.SetRootPage(1); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	got, err = p2.RootPage()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("再オープン後の RootPage = %d, want 1", got)
	}
}

// Free したページが Allocate で再利用され、その間 NumPages が
// 増えないことを確認する。フリーリストが枯渇すれば再び増える。
func TestFreeAndReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ids := make([]PageID, 3)
	for i := range ids {
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = page.ID
	}
	numAfterAlloc := p.NumPages()

	if err := p.Free(ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := p.Free(ids[2]); err != nil {
		t.Fatal(err)
	}
	if got := p.FreelistCount(); got != 2 {
		t.Fatalf("Free 後の FreelistCount() = %d, want 2", got)
	}

	reused := make(map[PageID]bool)
	for i := 0; i < 2; i++ {
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		reused[page.ID] = true
	}
	if got := p.NumPages(); got != numAfterAlloc {
		t.Fatalf("フリーリスト再利用後の NumPages = %d, want %d(増えないはず)", got, numAfterAlloc)
	}
	if !reused[ids[1]] || !reused[ids[2]] {
		t.Fatalf("解放したページが再利用されていません: reused=%v", reused)
	}
	if got := p.FreelistCount(); got != 0 {
		t.Fatalf("再利用後の FreelistCount() = %d, want 0", got)
	}

	// フリーリストが空になったので、次の Allocate はファイルを伸ばす。
	if _, err := p.Allocate(); err != nil {
		t.Fatal(err)
	}
	if got := p.NumPages(); got != numAfterAlloc+1 {
		t.Fatalf("フリーリスト枯渇後の NumPages = %d, want %d", got, numAfterAlloc+1)
	}
}

// フリーリストの状態が Close/Open をまたいで保持され、
// 再オープン後の Allocate でも再利用が働くことを確認する。
func TestFreelistPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	freedID := page.ID
	if err := p.Free(freedID); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if got := p2.FreelistCount(); got != 1 {
		t.Fatalf("再オープン後の FreelistCount() = %d, want 1", got)
	}
	numBefore := p2.NumPages()

	reused, err := p2.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID != freedID {
		t.Fatalf("Allocate() = ページ %d, want 解放済みページ %d の再利用", reused.ID, freedID)
	}
	if got := p2.NumPages(); got != numBefore {
		t.Fatalf("再利用後の NumPages = %d, want %d(増えないはず)", got, numBefore)
	}
}

// キャッシュ上限を超えるページ数を扱っても、追い出し → 再読み込みが
// 透過的に行われ、常に正しい内容が読めることを確認する。
func TestLRUEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.SetCacheLimit(4)

	const n = 10
	ids := make([]PageID, n)
	for i := 0; i < n; i++ {
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		copy(page.Data[:], fmt.Sprintf("page-%d", i))
		p.MarkDirty(page.ID)
		ids[i] = page.ID
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}

	// Flush 後は全ページが clean になり、追い出し対象になり得る。
	// 追い出しが起きても Get で正しい内容へ透過的に読み戻せることを確認する。
	for i := 0; i < n; i++ {
		page, err := p.Get(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("page-%d", i)
		if !bytes.HasPrefix(page.Data[:], []byte(want)) {
			t.Fatalf("ページ %d の内容 = %q, want prefix %q", ids[i], page.Data[:len(want)], want)
		}
	}

	// 白箱検査: 同じページを繰り返し参照して他のページを追い出させ続けると、
	// キャッシュ収容数が上限付近まで縮むことを確認する。
	for i := 0; i < 3*n; i++ {
		if _, err := p.Get(ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.cache) > p.maxCached+1 {
		t.Fatalf("キャッシュサイズ = %d, 上限 %d 付近に収まっていません", len(p.cache), p.maxCached)
	}
}

// dirty なページは、キャッシュ上限を超えていても絶対に追い出されない
// ことを白箱で確認する(すべて dirty ならソフトな上限として何もしない)。
func TestDirtyPageNeverEvicted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.SetCacheLimit(2)

	const n = 3
	ids := make([]PageID, n)
	for i := 0; i < n; i++ {
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		copy(page.Data[:], fmt.Sprintf("dirty-%d", i))
		p.MarkDirty(page.ID)
		ids[i] = page.ID
	}

	// 追い出しを誘発するため Get を繰り返す。すべて dirty のままなので、
	// 追い出せるページが 1 枚もなく何も起こらないはず。
	for round := 0; round < 5; round++ {
		for i := 0; i < n; i++ {
			if _, err := p.Get(ids[i]); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 白箱検査: Flush 前でも、dirty な 3 ページはすべてキャッシュに残っている。
	for i := 0; i < n; i++ {
		page, ok := p.cache[ids[i]]
		if !ok {
			t.Fatalf("dirty なページ %d がキャッシュから消えています", ids[i])
		}
		if !page.dirty {
			t.Fatalf("ページ %d が dirty ではありません", ids[i])
		}
		want := fmt.Sprintf("dirty-%d", i)
		if !bytes.HasPrefix(page.Data[:], []byte(want)) {
			t.Fatalf("ページ %d の内容 = %q, want prefix %q", ids[i], page.Data[:len(want)], want)
		}
	}
}
