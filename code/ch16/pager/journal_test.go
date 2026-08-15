package pager

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// Begin → 変更 → Commit で確定した内容は、再オープンしても残っており、
// ジャーナルファイルは残っていない(コミットの最後にジャーナルを消すため)。
func TestCommitPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "hello, journal")
	p.MarkDirty(page.ID)

	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(journalPathFor(path)); !os.IsNotExist(err) {
		t.Fatalf("Commit 後にジャーナルファイルが残っています: err=%v", err)
	}

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
	if !bytes.HasPrefix(loaded.Data[:], []byte("hello, journal")) {
		t.Fatalf("ページ内容が一致しません: %q", loaded.Data[:20])
	}
}

// Begin → 変更(既存ページの書き換え + Allocate)→ Rollback で、
// キャッシュ上の内容もページ確保も、Begin 前の状態へ巻き戻ることを確認する。
func TestRollbackDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// 巻き戻される既存ページを 1 枚、コミット済みの状態として用意する。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	target, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(target.Data[:], "original")
	p.MarkDirty(target.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	numPagesBefore := p.NumPages()

	// ここからロールバック対象のトランザクション。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.MarkDirty(page.ID)
	copy(page.Data[:], "changed!")

	// Tx 中に新しくページも確保する(numPages が巻き戻ることを確認するため)。
	newPage, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(newPage.Data[:], "new page")
	p.MarkDirty(newPage.ID)

	if err := p.Rollback(); err != nil {
		t.Fatal(err)
	}

	if got := p.NumPages(); got != numPagesBefore {
		t.Fatalf("Rollback 後の NumPages = %d, want %d(Allocate が巻き戻っていない)", got, numPagesBefore)
	}

	reloaded, err := p.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(reloaded.Data[:], []byte("original")) {
		t.Fatalf("Rollback 後のページ内容 = %q, want prefix %q(キャッシュから変更前の内容へ戻っていない)", reloaded.Data[:8], "original")
	}

	if p.InTx() {
		t.Fatal("Rollback 後も InTx() が true のままです")
	}
}

// トランザクション中の Flush はエラーになる(Commit を使うよう促す)。
func TestTxBlocksFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(); err == nil {
		t.Fatal("トランザクション中の Flush がエラーになりませんでした")
	}
}

// ジャーナルにエントリだけ書かれてヘッダ(エントリ数)が書かれる前に
// クラッシュした状況を手で再現する。DB 本体は無傷なはずなので、
// Open はこの無効なジャーナルを無視して削除し、DB は変更前のまま。
func TestCrashBeforeJournalHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	p.MarkDirty(page.ID)
	if err := p.Flush(); err != nil { // Tx の外で確定させておく(コミット済みの初期状態)
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// エントリだけ書き、ヘッダ(マジック + エントリ数)を書かずに放置する
	// = writeJournal の手順 1(fsync 前)でクラッシュした状態の再現。
	jPath := journalPathFor(path)
	entry := make([]byte, journalEntrySize)
	binary.BigEndian.PutUint32(entry[0:4], uint32(page.ID))
	copy(entry[4:], []byte("this should never make it back"))
	if err := os.WriteFile(jPath, append(make([]byte, journalHeaderSize), entry...), 0o644); err != nil {
		t.Fatal(err)
	}
	// ヘッダ部分は全部ゼロ(マジックも入っていない) → 無効なジャーナル。

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if _, err := os.Stat(jPath); !os.IsNotExist(err) {
		t.Fatalf("無効なジャーナルが削除されていません: err=%v", err)
	}
	loaded, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(loaded.Data[:], []byte("this should never make it back")) {
		t.Fatal("無効なジャーナルの内容が DB に反映されてしまっています")
	}
}

// 有効なジャーナル(ヘッダも書き終わっている)を作った上で、DB 本体には
// Flush 途中で落ちたかのような中途半端な内容を直接書き込んでおく。
// Open はジャーナルから変更前の内容を復元し、DB を元の状態に戻すはず。
func TestCrashAfterJournalBeforeFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "committed content")
	p.MarkDirty(page.ID)
	if err := p.Flush(); err != nil { // コミット済みの初期状態を用意する
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// 有効なジャーナル: ページ page.ID の「変更前(= コミット済み)の内容」を退避する。
	jPath := journalPathFor(path)
	header := make([]byte, journalHeaderSize)
	copy(header[0:8], journalMagic)
	binary.BigEndian.PutUint32(header[8:12], 1)

	entry := make([]byte, journalEntrySize)
	binary.BigEndian.PutUint32(entry[0:4], uint32(page.ID))
	var original [PageSize]byte
	copy(original[:], "committed content")
	copy(entry[4:], original[:])

	journalBytes := append(header, entry...)
	if err := os.WriteFile(jPath, journalBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// DB 本体を Flush 途中で落ちたかのように、中途半端な内容で直接上書きする。
	dbFile, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var half [PageSize]byte
	copy(half[:], "HALF-WRITTEN-GARBAGE")
	if _, err := dbFile.WriteAt(half[:], int64(page.ID)*PageSize); err != nil {
		t.Fatal(err)
	}
	if err := dbFile.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if _, err := os.Stat(jPath); !os.IsNotExist(err) {
		t.Fatalf("復元後にジャーナルが削除されていません: err=%v", err)
	}
	loaded, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(loaded.Data[:], []byte("committed content")) {
		t.Fatalf("復元後のページ内容 = %q, want prefix %q", loaded.Data[:20], "committed content")
	}
}

// Begin → 変更 → Close(Rollback せずに閉じる)で、再オープンすると
// 変更が消えている(Close が自動的に Rollback する)ことを確認する。
func TestCloseInTxRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "should vanish")
	p.MarkDirty(page.ID)
	allocatedID := page.ID

	// Rollback も Commit もせず、そのまま Close する。
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if got := p2.NumPages(); got != 1 {
		t.Fatalf("再オープン後の NumPages = %d, want 1(Allocate が巻き戻っていない)", got)
	}
	if _, err := p2.Get(allocatedID); err == nil {
		t.Fatalf("Close 時に Rollback されなかったページ %d が読めてしまいます", allocatedID)
	}
}

// journalPathFor はテストから見えるジャーナルパスの組み立て(非公開の
// journalPath メソッドと同じ規則)。
func journalPathFor(dbPath string) string {
	return dbPath + "-journal"
}
