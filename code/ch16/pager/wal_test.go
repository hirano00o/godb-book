package pager

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// readDBBody は DB 本体ファイルをキャッシュや WAL を介さず直接読み取る
// (WAL がまだ DB 本体へ反映していないことを確かめるための白箱ヘルパー)。
func readDBBody(t *testing.T, path string, id PageID) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, PageSize)
	if _, err := f.ReadAt(buf, int64(id)*PageSize); err != nil {
		t.Fatal(err)
	}
	return buf
}

// Begin → 変更 → Commit を WAL モードで行うと、Get では新しい値が読める
// 一方、DB 本体ファイルは旧値のままで、WAL ファイルが作られていることを
// 確認する(コミット時追記が「DB 本体には一切書かない」ことの直接的な証拠)。
func TestWALCommitAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// ロールバックジャーナルモードのまま、コミット済みの初期値を作る。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "old value")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.MarkDirty(got.ID)
	copy(got.Data[:], "new value via wal")
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	// 変更カウンタのインクリメント(bumpChangeCounter)によりヘッダページも
	// dirty になるため、対象ページ本体と合わせて 2 フレームになる。
	if p.walFrames != 2 {
		t.Fatalf("walFrames = %d, want 2", p.walFrames)
	}

	// キャッシュ経由(まだ載っている)でも新値が読める。
	reread, err := p.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(reread.Data[:], []byte("new value via wal")) {
		t.Fatalf("Get() の内容 = %q, want prefix %q", reread.Data[:20], "new value via wal")
	}

	// キャッシュから追い出し、readPage の WAL 経由読み取りも確認する。
	delete(p.cache, page.ID)
	if elem, ok := p.lruIndex[page.ID]; ok {
		p.lru.Remove(elem)
		delete(p.lruIndex, page.ID)
	}
	rereadFromWAL, err := p.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(rereadFromWAL.Data[:], []byte("new value via wal")) {
		t.Fatalf("WAL 経由の Get() の内容 = %q, want prefix %q", rereadFromWAL.Data[:20], "new value via wal")
	}

	// DB 本体ファイルは旧値のまま(WAL コミットは DB 本体に一切書かない)。
	body := readDBBody(t, path, page.ID)
	if !bytes.HasPrefix(body, []byte("old value")) {
		t.Fatalf("DB 本体の内容 = %q, want prefix %q(WAL コミットで DB 本体が書き換わってしまっている)", body[:20], "old value")
	}

	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("WAL ファイルが存在しません: %v", err)
	}
}

// Commit → Close(チェックポイント)→ 再 Open で、新しい値が DB 本体に
// 反映されており、WAL はヘッダだけの空の状態になっていることを確認する。
func TestWALPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "persisted via wal")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := p.Close(); err != nil { // Close がチェックポイントする
		t.Fatal(err)
	}

	walInfo, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if walInfo.Size() != walHeaderSize {
		t.Fatalf("Close 後の WAL サイズ = %d, want %d(ヘッダのみ)", walInfo.Size(), walHeaderSize)
	}

	body := readDBBody(t, path, page.ID)
	if !bytes.HasPrefix(body, []byte("persisted via wal")) {
		t.Fatalf("Close 後の DB 本体の内容 = %q, want prefix %q", body[:20], "persisted via wal")
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	if p2.JournalMode() != JournalModeWAL {
		t.Fatalf("再オープン後のモード = %v, want %v", p2.JournalMode(), JournalModeWAL)
	}
	got, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got.Data[:], []byte("persisted via wal")) {
		t.Fatalf("再オープン後の内容 = %q, want prefix %q", got.Data[:20], "persisted via wal")
	}
}

// コミット済みトランザクションの後に、コミット印のないフレームを手で
// 追記してクラッシュを再現する。再 Open は尻切れ分を無視し、
// コミット済みの内容までは正しく見える。さらにその状態で新しい Commit を
// しても整合が保たれることを確認する。
func TestWALTornTailRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "committed")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	walPath := path + "-wal"
	committedInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	committedSize := committedInfo.Size()

	// クラッシュ再現: コミット印のないフレームを直接追記する。
	// p は意図的に Close しない — Close はチェックポイントで WAL を
	// 切り詰めてしまい、これから作る尻切れを消してしまうため
	// (このテストが検証したいのは「Open 時の復旧」であって、
	// 正常なグレースフルシャットダウンではない)。
	wf, err := os.OpenFile(walPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, walFrameSize)
	binary.BigEndian.PutUint32(frame[0:4], uint32(page.ID))
	binary.BigEndian.PutUint32(frame[4:8], 0) // コミット印なし = 尻切れ
	copy(frame[8:], []byte("torn write, should vanish"))
	if _, err := wf.WriteAt(frame, committedSize); err != nil {
		t.Fatal(err)
	}
	if err := wf.Close(); err != nil {
		t.Fatal(err)
	}

	// p はまだ WAL モードの接続単位ロック(排他)を保持したままなので、
	// このまま Open(path) すると ErrBusy になってしまう。実際のクラッシュ
	// では OS がプロセスの終了時に flock を自動的に解放するので、それを
	// 模して p.unlock() だけを呼ぶ(p.Close() はチェックポイントで WAL を
	// 切り詰めてしまい、上で作った尻切れを消してしまうため使えない)。
	if err := p.unlock(); err != nil {
		t.Fatal(err)
	}

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	got, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got.Data[:], []byte("committed")) {
		t.Fatalf("復旧後の内容 = %q, want prefix %q", got.Data[:20], "committed")
	}
	if bytes.Contains(got.Data[:], []byte("torn write")) {
		t.Fatal("尻切れフレームの内容が反映されてしまっています")
	}

	walInfoAfter, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if walInfoAfter.Size() != committedSize {
		t.Fatalf("再オープン後の WAL サイズ = %d, want %d(尻切れが切り詰められていない)", walInfoAfter.Size(), committedSize)
	}

	// 尻切れ復旧後、新しい Commit をしても整合が保たれる。
	if err := p2.Begin(); err != nil {
		t.Fatal(err)
	}
	got2, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	p2.MarkDirty(got2.ID)
	copy(got2.Data[:], "after recovery")
	if err := p2.Commit(); err != nil {
		t.Fatal(err)
	}

	got3, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got3.Data[:], []byte("after recovery")) {
		t.Fatalf("復旧後の Commit の内容 = %q, want prefix %q", got3.Data[:20], "after recovery")
	}
}

// チェックポイント相当の書き戻し(writeBackWALPages)を 2 回連続で
// 実行しても、DB 本体の内容が変わらないことを確認する。フレームは常に
// ページの完全な内容なので、同じ書き戻しを繰り返しても安全なはず
// (チェックポイントが処理の途中で中断されても、最初からやり直せる)。
func TestWALCheckpointIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "checkpoint me")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := p.writeBackWALPages(); err != nil {
		t.Fatal(err)
	}
	first := append([]byte(nil), readDBBody(t, path, page.ID)...)
	if !bytes.HasPrefix(first, []byte("checkpoint me")) {
		t.Fatalf("1 回目の書き戻し後の内容 = %q, want prefix %q", first[:20], "checkpoint me")
	}

	// walIndex をクリアせず(= checkpoint() のようにフレームを消さず)に
	// もう一度実行しても、結果が変わらないことを確認する。
	if err := p.writeBackWALPages(); err != nil {
		t.Fatal(err)
	}
	second := readDBBody(t, path, page.ID)
	if !bytes.Equal(first, second) {
		t.Fatalf("2 回目の書き戻しで結果が変わりました: 1 回目=%q, 2 回目=%q", first[:20], second[:20])
	}
}

// コミット済みフレーム数が walCheckpointThreshold を超えたら、Commit の
// たびに自動でチェックポイントされ、WAL が溜まり続けないことを確認する。
func TestWALAutoCheckpoint(t *testing.T) {
	original := walCheckpointThreshold
	SetWALCheckpointThreshold(3)
	defer SetWALCheckpointThreshold(original)

	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	var firstPageID PageID
	const n = 10
	for i := 0; i < n; i++ {
		if err := p.Begin(); err != nil {
			t.Fatal(err)
		}
		page, err := p.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstPageID = page.ID
		}
		copy(page.Data[:], fmt.Sprintf("row-%d", i))
		p.MarkDirty(page.ID)
		if err := p.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	if p.walFrames > walCheckpointThreshold {
		t.Fatalf("自動チェックポイントが機能していません: walFrames=%d, threshold=%d", p.walFrames, walCheckpointThreshold)
	}

	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	maxExpected := int64(walHeaderSize) + int64(walCheckpointThreshold)*int64(walFrameSize)
	if info.Size() > maxExpected {
		t.Fatalf("WAL ファイルが自動で切り詰められていません: size=%d, want <= %d", info.Size(), maxExpected)
	}

	// 最初の方の書き込みは、途中の自動チェックポイントで DB 本体へ
	// 反映されているはず。
	body := readDBBody(t, path, firstPageID)
	if !bytes.HasPrefix(body, []byte("row-0")) {
		t.Fatalf("自動チェックポイント後の DB 本体の内容 = %q, want prefix %q", body[:10], "row-0")
	}
}

// rollback → wal → rollback とモードを往復できること、切替後の再オープンで
// モードが持続すること、WAL → rollback の切替でデータが DB 本体へ
// 反映されることを確認する。
func TestJournalModeSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if p.JournalMode() != JournalModeRollback {
		t.Fatalf("新規 DB の既定モード = %v, want %v", p.JournalMode(), JournalModeRollback)
	}

	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}
	if p.JournalMode() != JournalModeWAL {
		t.Fatalf("切替後のモード = %v, want %v", p.JournalMode(), JournalModeWAL)
	}

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "wal data")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// 再オープンでモードが持続する。
	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if p2.JournalMode() != JournalModeWAL {
		t.Fatalf("再オープン後のモード = %v, want %v(持続していない)", p2.JournalMode(), JournalModeWAL)
	}
	got, err := p2.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got.Data[:], []byte("wal data")) {
		t.Fatalf("再オープン後の内容 = %q, want prefix %q", got.Data[:20], "wal data")
	}

	// WAL → rollback。チェックポイントされ、WAL ファイルが削除され、
	// DB 本体にデータが反映されているはず。
	if err := p2.SetJournalMode(JournalModeRollback); err != nil {
		t.Fatal(err)
	}
	if p2.JournalMode() != JournalModeRollback {
		t.Fatalf("切替後のモード = %v, want %v", p2.JournalMode(), JournalModeRollback)
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("rollback へ切替後も WAL ファイルが残っています: err=%v", err)
	}

	body := readDBBody(t, path, page.ID)
	if !bytes.HasPrefix(body, []byte("wal data")) {
		t.Fatalf("rollback へ切替後の DB 本体の内容 = %q, want prefix %q", body[:20], "wal data")
	}

	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}

	// 再オープンしても rollback のまま持続する。
	p3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p3.Close()
	if p3.JournalMode() != JournalModeRollback {
		t.Fatalf("再々オープン後のモード = %v, want %v", p3.JournalMode(), JournalModeRollback)
	}
}

// WAL モードでの Rollback は変更を捨てる(ロールバックジャーナル方式と
// 同じ「dirty ページを捨てて読み直すだけ」の挙動になる)。WAL には
// コミット時にしか追記しないので、Rollback したトランザクションの内容は
// WAL に一切書かれない。
func TestWALRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "original")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}
	numPagesBefore := p.NumPages()

	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.MarkDirty(got.ID)
	copy(got.Data[:], "changed!")

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
	if p.InTx() {
		t.Fatal("Rollback 後も InTx() が true のままです")
	}

	reloaded, err := p.Get(page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(reloaded.Data[:], []byte("original")) {
		t.Fatalf("Rollback 後のページ内容 = %q, want prefix %q", reloaded.Data[:8], "original")
	}

	if len(p.walIndex) != 0 {
		t.Fatalf("Rollback したはずの変更が WAL に書かれています: walIndex=%v", p.walIndex)
	}
}

// 書き込みエラーで失敗したコミットが残す「コミット印のない孤児フレーム」が、
// 後続の正常なコミットと再オープンを経ても蘇生しないことを確認する。
// walCommit がファイルサイズではなく walEnd から書き始めることの検証。
func TestWALOrphanFramesNotResurrected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetJournalMode(JournalModeWAL); err != nil {
		t.Fatal(err)
	}

	// 正常なコミットを 1 つ作る。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page.Data[:], "committed")
	p.MarkDirty(page.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	orphanTarget := page.ID

	// 失敗したコミットを再現する: コミット印のない孤児フレームを
	// WAL ファイル末尾へ直接書き込む(committed ページを別内容で汚す)。
	frame := make([]byte, walFrameSize)
	binary.BigEndian.PutUint32(frame[0:4], uint32(orphanTarget))
	binary.BigEndian.PutUint32(frame[4:8], 0) // コミット印なし
	copy(frame[8:], []byte("orphan"))
	info, err := p.walFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.walFile.WriteAt(frame, info.Size()); err != nil {
		t.Fatal(err)
	}

	// 孤児のあとに正常なコミットを重ねる(walEnd から書かれ、孤児を上書きする)。
	if err := p.Begin(); err != nil {
		t.Fatal(err)
	}
	page2, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	copy(page2.Data[:], "second")
	p.MarkDirty(page2.ID)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// 再オープンして、汚された側のページが "committed" のままであることを確認。
	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	got, err := p2.Get(orphanTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got.Data[:], []byte("committed")) {
		t.Fatalf("孤児フレームが蘇生しています: %q", got.Data[:16])
	}
}
