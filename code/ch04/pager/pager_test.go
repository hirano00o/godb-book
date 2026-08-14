package pager

import (
	"bytes"
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
