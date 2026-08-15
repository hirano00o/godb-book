// wal.go は Write-Ahead Logging(WAL)方式によるトランザクションの
// 耐久性・原子性を担う、WAL ファイルの読み書きを扱う。
//
// ロールバックジャーナル方式(journal.go)は「変更前の内容」をジャーナルへ
// 退避してから DB 本体を書き換える。WAL 方式はその逆で、「変更後の内容」を
// WAL ファイルへ追記するだけでコミットを終える。DB 本体は一切書き換えない。
// 読み取りは「WAL にあれば WAL、なければ DB 本体」という 2 層構造になり、
// WAL は「DB 本体の内容を最新のコミット結果で上書きするレイヤ」として働く
// (pager.go の readPage 参照)。
//
// この方式の核心は、コミットのたびに必要な fsync が 1 回で済むこと。
// ロールバックジャーナル方式は
//  1. ジャーナルへ退避 + fsync
//  2. DB 本体へ書き戻し + fsync(Flush)
//  3. ジャーナル削除 + fsync
//
// と 3 回の fsync を要するが、WAL 方式は「WAL への追記 + fsync」の 1 回
// だけで耐久性を確保できる(DB 本体への反映はチェックポイントまで遅延され、
// チェックポイントの頻度はコミットより低く保てる)。
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	// walMagic は WAL ファイルの先頭 8 バイト。
	walMagic = "minidbwl"

	// walHeaderSize は WAL のヘッダサイズ。マジック(8B)+ 予約(8B)。
	walHeaderSize = 16

	// walFrameHeaderSize は 1 フレームのヘッダサイズ。
	// ページ番号(uint32 BE、4B)+ コミット印(uint32 BE、4B)。
	walFrameHeaderSize = 8

	// walFrameSize は 1 フレームの総サイズ(ヘッダ + ページ本体)。
	walFrameSize = walFrameHeaderSize + PageSize
)

// walCheckpointThreshold はコミット済みフレーム数がこれを超えたら
// Commit のたびに自動でチェックポイントする閾値。テストから小さい値に
// 差し替えられるよう var にしている(package pager の白箱テストが直接
// 書き換える)。
var walCheckpointThreshold = 1000

// walPath はこの DB の WAL ファイルのパスを返す。
func (p *Pager) walPath() string {
	return p.path + "-wal"
}

// closeWALFile は Open の失敗経路で WAL ファイルを閉じ忘れないための
// 後始末用ヘルパー。walFile が nil でも安全に呼べる。
func (p *Pager) closeWALFile() {
	if p.walFile != nil {
		p.walFile.Close()
		p.walFile = nil
	}
}

// ensureWALFile は WAL ファイルが開かれていなければ開く(存在しなければ
// ヘッダだけの新規ファイルを作る)。通常は SetJournalMode(WAL) の時点で
// 既に開かれているはずだが、「ヘッダを WAL 用に書き換えた直後、WAL
// ファイルを作る前」にクラッシュした場合への保険として、walCommit からも
// 呼べるようにしてある。
func (p *Pager) ensureWALFile() error {
	if p.walFile != nil {
		return nil
	}
	f, err := os.OpenFile(p.walPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if info.Size() == 0 {
		header := make([]byte, walHeaderSize)
		copy(header[0:8], walMagic)
		if _, err := f.WriteAt(header, 0); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	p.walFile = f
	if p.walIndex == nil {
		p.walIndex = make(map[PageID]int64)
	}
	p.walEnd = walHeaderSize
	return nil
}

// openWALIfPresent は Open の際に呼ばれる。WAL ファイルが残っていれば
// 先頭から走査し、コミット印までのフレームだけを walIndex に反映する。
//
// 最後のコミット印より後ろのフレーム(コミットの途中でクラッシュした
// 尻切れ)は index に入れず、ファイル自体をそこまで切り詰める。これにより
// 次の walCommit の追記が尻切れ部分を上書きする形になり、「尻切れの無視」
// がそのままクラッシュリカバリとして働く。
func (p *Pager) openWALIfPresent() error {
	f, err := os.OpenFile(p.walPath(), os.O_RDWR, 0o644)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if info.Size() < walHeaderSize {
		// ヘッダすら書き切れていない壊れた WAL。無視して素の状態から始める。
		f.Close()
		return nil
	}
	header := make([]byte, walHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		f.Close()
		return err
	}
	if string(header[0:8]) != walMagic {
		f.Close()
		return fmt.Errorf("WAL ファイルではありません: %s", p.walPath())
	}

	index, totalFrames, lastCommitEnd, err := scanWALFrames(f, info.Size())
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Truncate(lastCommitEnd); err != nil {
		f.Close()
		return err
	}

	p.walFile = f
	p.walIndex = index
	p.walFrames = totalFrames
	p.walEnd = lastCommitEnd
	return nil
}

// scanWALFrames は WAL ファイル f をヘッダの直後からフレーム単位で走査し、
// コミット済みのページ → オフセット対応(index)、コミット済みフレームの
// 総数(totalFrames)、最後のコミット印の直後のオフセット(lastCommitEnd、
// 尻切れフレームを切り詰める境界)を返す。
//
// pending は「まだこのトランザクションのコミット印(commit=1)に
// 到達していないフレーム」を溜めておくバッファ。コミット印に到達したら
// まとめて index へ反映する。コミット印に到達しないまま走査が終われば
// (= クラッシュで途中まで書かれた尻切れ)、pending の中身は棄てられる。
func scanWALFrames(f *os.File, size int64) (index map[PageID]int64, totalFrames int, lastCommitEnd int64, err error) {
	index = make(map[PageID]int64)
	pending := make(map[PageID]int64)
	pendingCount := 0

	head := make([]byte, walFrameHeaderSize)
	offset := int64(walHeaderSize)
	lastCommitEnd = offset
	for offset+walFrameSize <= size {
		if _, err := f.ReadAt(head, offset); err != nil {
			return nil, 0, 0, fmt.Errorf("WAL フレームの読み取りに失敗: %w", err)
		}
		id := PageID(binary.BigEndian.Uint32(head[0:4]))
		commit := binary.BigEndian.Uint32(head[4:8])

		pending[id] = offset
		pendingCount++
		offset += walFrameSize

		if commit == 1 {
			for pid, poff := range pending {
				index[pid] = poff
			}
			totalFrames += pendingCount
			pending = make(map[PageID]int64)
			pendingCount = 0
			lastCommitEnd = offset
		}
	}
	return index, totalFrames, lastCommitEnd, nil
}

// walCommit は WAL モードでのコミット処理。dirty ページをすべて WAL
// ファイル末尾へフレームとして追記し、最後の 1 フレームだけコミット印
// (commit=1)を立てる。fsync は末尾への追記が終わった後の 1 回だけ行い、
// それが成功して初めて walIndex を更新する。DB 本体には一切書き込まない。
//
// 書き始めの位置はファイルサイズではなく walEnd(有効なフレーム列の末尾)。
// 以前のコミットが書き込みエラーで失敗していた場合、コミット印のない
// 孤児フレームがファイル末尾に残っていることがある。ファイルサイズから
// 書き始めると、孤児フレームの直後に次のコミットが続き、後の再オープン時に
// スキャナが孤児フレームを次のコミットの一部として拾ってしまう
// (ロールバック済みの変更が蘇生する)。walEnd から書けば孤児は上書きされる。
func (p *Pager) walCommit(dirty []*Page) error {
	if err := p.ensureWALFile(); err != nil {
		return err
	}

	startOffset := p.walEnd

	offset := startOffset
	frame := make([]byte, walFrameSize)
	for i, page := range dirty {
		binary.BigEndian.PutUint32(frame[0:4], uint32(page.ID))
		commit := uint32(0)
		if i == len(dirty)-1 {
			commit = 1
		}
		binary.BigEndian.PutUint32(frame[4:8], commit)
		copy(frame[8:], page.Data[:])
		if _, err := p.walFile.WriteAt(frame, offset); err != nil {
			return fmt.Errorf("WAL への書き込みに失敗: %w", err)
		}
		offset += walFrameSize
	}

	// fsync は 1 回だけ。ロールバックジャーナル方式が 3 回必要とするのに
	// 対し、この 1 回にコミットの fsync 回数を削減できることが WAL 方式の
	// 核心の利点(package コメント参照)。
	if err := p.walFile.Sync(); err != nil {
		return err
	}

	offset = startOffset
	for _, page := range dirty {
		p.walIndex[page.ID] = offset
		page.dirty = false
		offset += walFrameSize
	}
	p.walFrames += len(dirty)
	p.walEnd = offset
	return nil
}

// writeBackWALPages は walIndex が指す全ページを WAL から読み出して DB
// 本体へ書き戻し、fsync する。WAL のフレームは常にページの完全な内容
// (差分ではない)なので、同じフレームを重ねて書き戻しても結果は変わらない
// (冪等)。チェックポイントの途中でクラッシュしても、最初からやり直せば
// 必ず同じ DB 本体に行き着く。
func (p *Pager) writeBackWALPages() error {
	if len(p.walIndex) == 0 {
		return nil
	}
	buf := make([]byte, PageSize)
	for id, offset := range p.walIndex {
		if _, err := p.walFile.ReadAt(buf, offset+walFrameHeaderSize); err != nil {
			return fmt.Errorf("チェックポイント: ページ %d の読み取りに失敗: %w", id, err)
		}
		if _, err := p.file.WriteAt(buf, int64(id)*PageSize); err != nil {
			return fmt.Errorf("チェックポイント: ページ %d の書き戻しに失敗: %w", id, err)
		}
	}
	return p.file.Sync()
}

// checkpoint はチェックポイント本体。WAL 上のコミット済みページをすべて
// DB 本体へ反映し(writeBackWALPages)、WAL ファイルをヘッダだけに
// truncate して fsync し、walIndex をクリアする。
//
// 実行タイミングは 3 つ: (1) Close 時(Tx 自動 Rollback の後)、
// (2) Commit 直後にコミット済みフレーム数が walCheckpointThreshold を
// 超えたとき自動、(3) PRAGMA wal_checkpoint による明示要求。
func (p *Pager) checkpoint() error {
	if p.walFile == nil {
		return nil
	}
	if err := p.writeBackWALPages(); err != nil {
		return err
	}
	if err := p.walFile.Truncate(walHeaderSize); err != nil {
		return err
	}
	if err := p.walFile.Sync(); err != nil {
		return err
	}
	p.walIndex = make(map[PageID]int64)
	p.walFrames = 0
	p.walEnd = walHeaderSize
	return nil
}

// Checkpoint は WAL 上のコミット済みページを DB 本体へ反映し、WAL を
// 空にする(PRAGMA wal_checkpoint から呼ばれる)。ロールバックジャーナル
// モードやトランザクション中は呼び出せない。
func (p *Pager) Checkpoint() error {
	if p.journalMode != JournalModeWAL {
		return errors.New("チェックポイントは WAL モードでのみ実行できます")
	}
	if p.inTx {
		return errors.New("トランザクション中はチェックポイントを実行できません")
	}
	return p.checkpoint()
}

// switchToWAL はロールバックジャーナル方式から WAL 方式へ切り替える。
// ヘッダページの journalMode を書き換えて Flush で確定させてから、
// WAL ファイルを新規に作る。
func (p *Pager) switchToWAL() error {
	header, err := p.Get(headerPageID)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(header.Data[journalModeOffset:journalModeOffset+4], uint32(JournalModeWAL))
	header.dirty = true
	p.journalMode = JournalModeWAL
	if err := p.Flush(); err != nil {
		return err
	}
	return p.ensureWALFile()
}

// switchToRollback は WAL 方式からロールバックジャーナル方式へ切り替える。
// まずチェックポイントで WAL の内容を DB 本体へ反映してから WAL
// ファイルを削除し、最後にヘッダページの journalMode を書き換えて
// Flush で確定させる。
func (p *Pager) switchToRollback() error {
	if err := p.checkpoint(); err != nil {
		return err
	}
	walPath := p.walPath()
	p.closeWALFile()
	if err := os.Remove(walPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	header, err := p.Get(headerPageID)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(header.Data[journalModeOffset:journalModeOffset+4], uint32(JournalModeRollback))
	header.dirty = true
	p.journalMode = JournalModeRollback
	return p.Flush()
}

// SetWALCheckpointThreshold はテストから自動チェックポイントの閾値を
// 差し替えるためのヘルパー(SetCacheLimit と同じ立て付け)。
func SetWALCheckpointThreshold(n int) { walCheckpointThreshold = n }
