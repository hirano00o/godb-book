// journal.go は「コミット時ジャーナリング」によるトランザクションの
// 耐久性・原子性を担う、ロールバックジャーナルの読み書きを扱う。
//
// トランザクション中はディスクに一切書かない(dirty ページはキャッシュに
// 滞留する)。そのためディスク上のページは常に「最後にコミットされた
// 内容」であり、コミットの直前に「これから上書きするページのディスク上の
// 内容」をまとめてジャーナルへ退避しておける。クラッシュが起きても、
// 次に Open したときにジャーナルを見れば DB 本体を元に戻せる。
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	// journalMagic はジャーナルファイルの先頭 8 バイト。
	journalMagic = "minidbjn"

	// journalHeaderSize はジャーナルのヘッダサイズ。
	// マジック(8B)+ エントリ数(uint32 BE、4B)+ 予約(4B)。
	journalHeaderSize = 16

	// journalEntrySize は 1 エントリのサイズ。ページ番号(uint32 BE、4B)+
	// ページ内容(PageSize バイト)。
	journalEntrySize = 4 + PageSize
)

// journalPath はこの DB のジャーナルファイルのパスを返す。
func (p *Pager) journalPath() string {
	return p.path + "-journal"
}

// journalEntry はジャーナル 1 エントリ(退避されたページ 1 枚)を表す。
type journalEntry struct {
	id   PageID
	data [PageSize]byte
}

// writeJournal は commit 対象の dirty ページについて、ディスク上の現在の
// 内容(まだ上書きしていない、直前にコミットされた内容)を読み取って
// ジャーナルファイルへ退避する。
//
// dirty ページの中には、このトランザクション中に Allocate で新規に
// 確保され、まだディスク上に存在しないページも含まれうる。そのような
// ページには退避すべき「前の内容」がない(ロールバック時は numPages を
// 元に戻すだけで、その領域は論理的に見えなくなる)ので、ジャーナルには
// 書かない。
//
// 書き込み手順(コミットプロトコルの中核):
//  1. エントリ列をヘッダより後ろへ書く(この時点でヘッダのエントリ数は
//     まだ 0 のまま)
//  2. fsync してエントリが確実にディスクへ落ちたことを保証する
//  3. マジックとエントリ数を持つヘッダを書き、再度 fsync する
//
// エントリ数の入ったヘッダこそが「ジャーナル有効」の印。ここまでの
// どの時点でクラッシュしても、DB 本体は無傷であり、ジャーナルは
// ヘッダが欠けている(無効)ため安全に無視できる。
func (p *Pager) writeJournal(dirty []*Page) error {
	jf, err := os.OpenFile(p.journalPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer jf.Close()

	info, err := p.file.Stat()
	if err != nil {
		return err
	}
	existingSize := info.Size()

	entry := make([]byte, journalEntrySize)
	offset := int64(journalHeaderSize)
	count := 0
	for _, page := range dirty {
		pageOffset := int64(page.ID) * PageSize
		if pageOffset+PageSize > existingSize {
			// このトランザクション中に新規確保されたページ。ディスクに
			// まだ存在しないので退避不要(上のコメント参照)。
			continue
		}
		if _, err := p.file.ReadAt(entry[4:], pageOffset); err != nil {
			return fmt.Errorf("ページ %d の退避に失敗: %w", page.ID, err)
		}
		binary.BigEndian.PutUint32(entry[0:4], uint32(page.ID))
		if _, err := jf.WriteAt(entry, offset); err != nil {
			return fmt.Errorf("ジャーナルへの書き込みに失敗: %w", err)
		}
		offset += journalEntrySize
		count++
	}
	if err := jf.Sync(); err != nil {
		return err
	}

	header := make([]byte, journalHeaderSize)
	copy(header[0:8], journalMagic)
	binary.BigEndian.PutUint32(header[8:12], uint32(count))
	if _, err := jf.WriteAt(header, 0); err != nil {
		return fmt.Errorf("ジャーナルヘッダの書き込みに失敗: %w", err)
	}
	return jf.Sync()
}

// recoverFromJournal は有効なジャーナルが残っていれば(= コミットの
// 途中でクラッシュしていれば)、退避されていた変更前ページを DB へ
// 書き戻して未完のトランザクションを巻き戻す。
//
// ジャーナルが存在しない場合は何もしない。存在してもヘッダが不正
// (マジック不一致・エントリ数 0・ファイルサイズ不足)なら、コミットの
// ヘッダを書き切る前にクラッシュしたということなので、DB 本体は無傷
// なままジャーナルだけを削除して終える。
func (p *Pager) recoverFromJournal(path string) error {
	entries, valid, err := readJournal(path)
	if err != nil {
		return err
	}
	if !valid {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	for _, e := range entries {
		if _, err := p.file.WriteAt(e.data[:], int64(e.id)*PageSize); err != nil {
			return fmt.Errorf("ページ %d の復元に失敗: %w", e.id, err)
		}
	}
	if err := p.file.Sync(); err != nil {
		return err
	}
	return os.Remove(path)
}

// readJournal はジャーナルファイルを読み取り、有効なジャーナルかどうかと
// そのエントリの一覧を返す。ファイルが存在しない場合は
// valid = false, err = nil を返す(「ジャーナルなし」も「無効」も
// 呼び出し側の扱いは同じなので区別しない)。
func readJournal(path string) (entries []journalEntry, valid bool, err error) {
	jf, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer jf.Close()

	info, err := jf.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() < journalHeaderSize {
		return nil, false, nil
	}

	header := make([]byte, journalHeaderSize)
	if _, err := jf.ReadAt(header, 0); err != nil {
		return nil, false, err
	}
	if string(header[0:8]) != journalMagic {
		return nil, false, nil
	}
	count := binary.BigEndian.Uint32(header[8:12])
	if count == 0 {
		return nil, false, nil
	}
	wantSize := int64(journalHeaderSize) + int64(count)*int64(journalEntrySize)
	if info.Size() < wantSize {
		return nil, false, nil
	}

	buf := make([]byte, journalEntrySize)
	offset := int64(journalHeaderSize)
	result := make([]journalEntry, count)
	for i := uint32(0); i < count; i++ {
		if _, err := jf.ReadAt(buf, offset); err != nil {
			return nil, false, err
		}
		e := journalEntry{id: PageID(binary.BigEndian.Uint32(buf[0:4]))}
		copy(e.data[:], buf[4:])
		result[i] = e
		offset += journalEntrySize
	}
	return result, true, nil
}
