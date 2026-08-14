// Package pager は minidb のストレージ層の最下層、
// 「ページ単位のファイル入出力とキャッシュ」を担当する。
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	// PageSize は 1 ページのバイト数。SQLite のデフォルトに合わせる。
	PageSize = 4096

	// Magic は minidb ファイルの先頭 8 バイト。
	Magic = "minidb01"

	// headerPageID はメタ情報を置くページ。常にファイルの先頭。
	headerPageID PageID = 0
)

// PageID はページ番号。ページ n はファイルの n*PageSize バイト目から始まる。
type PageID uint32

// Page はメモリ上に読み込まれた 1 ページ。
type Page struct {
	ID    PageID
	Data  [PageSize]byte
	dirty bool // ディスクに書き戻す必要があるか
}

// Pager はページの読み書きとキャッシュを担当する。
// 上位層(B-Tree)はファイルオフセットを一切意識せず、
// PageID だけでページを取得・更新する。
type Pager struct {
	file     *os.File
	numPages uint32           // ヘッダページ含む総ページ数
	cache    map[PageID]*Page // 読み込み済みページのキャッシュ
}

// Open は minidb ファイルを開く。存在しなければ新規作成し、
// ヘッダページ(ページ 0)を初期化する。
func Open(path string) (*Pager, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	p := &Pager{
		file:  file,
		cache: make(map[PageID]*Page),
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	if info.Size() == 0 {
		// 新規ファイル: ヘッダページを作って書き込む
		if err := p.initHeader(); err != nil {
			file.Close()
			return nil, err
		}
		return p, nil
	}

	// 既存ファイル: ヘッダページを検証して総ページ数を読み取る
	if err := p.loadHeader(); err != nil {
		file.Close()
		return nil, err
	}
	return p, nil
}

// initHeader はヘッダページを初期化してディスクに書き込む。
// レイアウト:
//
//	オフセット 0-7 : マジック文字列 "minidb01"
//	オフセット 8-11: ページサイズ(ビッグエンディアン uint32)
//	オフセット 12-15: 総ページ数(ビッグエンディアン uint32)
func (p *Pager) initHeader() error {
	page := &Page{ID: headerPageID}
	copy(page.Data[0:8], Magic)
	binary.BigEndian.PutUint32(page.Data[8:12], PageSize)

	p.numPages = 1 // ヘッダページ自身
	binary.BigEndian.PutUint32(page.Data[12:16], p.numPages)

	p.cache[headerPageID] = page
	page.dirty = true
	return p.Flush()
}

// loadHeader は既存ファイルのヘッダページを読み込んで検証する。
func (p *Pager) loadHeader() error {
	page, err := p.readPage(headerPageID)
	if err != nil {
		return err
	}
	if string(page.Data[0:8]) != Magic {
		return errors.New("minidb ファイルではありません")
	}
	if got := binary.BigEndian.Uint32(page.Data[8:12]); got != PageSize {
		return fmt.Errorf("ページサイズが不一致: file=%d, build=%d", got, PageSize)
	}
	p.numPages = binary.BigEndian.Uint32(page.Data[12:16])
	p.cache[headerPageID] = page
	return nil
}

// NumPages はヘッダページを含む総ページ数を返す。
func (p *Pager) NumPages() uint32 { return p.numPages }

// Get はページを取得する。キャッシュにあればそれを、
// なければディスクから読み込んで返す。
func (p *Pager) Get(id PageID) (*Page, error) {
	if uint32(id) >= p.numPages {
		return nil, fmt.Errorf("ページ %d は存在しません(総ページ数 %d)", id, p.numPages)
	}
	if page, ok := p.cache[id]; ok {
		return page, nil
	}
	page, err := p.readPage(id)
	if err != nil {
		return nil, err
	}
	p.cache[id] = page
	return page, nil
}

// Allocate はファイル末尾に新しいページを確保して返す。
// ディスクへの書き込みは Flush まで遅延される。
func (p *Pager) Allocate() (*Page, error) {
	page := &Page{ID: PageID(p.numPages), dirty: true}
	p.numPages++
	p.cache[page.ID] = page

	// ヘッダページの総ページ数も更新する
	header, err := p.Get(headerPageID)
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(header.Data[12:16], p.numPages)
	header.dirty = true
	return page, nil
}

// MarkDirty はページの内容を書き換えたことをページャに伝える。
// 上位層は Data を書き換えたら必ずこれを呼ぶ。
func (p *Pager) MarkDirty(id PageID) {
	if page, ok := p.cache[id]; ok {
		page.dirty = true
	}
}

// Flush はダーティなページをすべてディスクに書き戻し、
// fsync でストレージへの到達を保証する。
func (p *Pager) Flush() error {
	for _, page := range p.cache {
		if !page.dirty {
			continue
		}
		if err := p.writePage(page); err != nil {
			return err
		}
		page.dirty = false
	}
	// OS のバッファに残っているデータを物理ディスクまで書かせる。
	// これを省くと、電源断でファイルが壊れる可能性がある。
	return p.file.Sync()
}

// Close は Flush してからファイルを閉じる。
func (p *Pager) Close() error {
	if err := p.Flush(); err != nil {
		p.file.Close()
		return err
	}
	return p.file.Close()
}

// readPage はディスクからページを 1 枚読み込む(キャッシュを介さない)。
func (p *Pager) readPage(id PageID) (*Page, error) {
	page := &Page{ID: id}
	_, err := p.file.ReadAt(page.Data[:], int64(id)*PageSize)
	if err != nil {
		return nil, fmt.Errorf("ページ %d の読み取りに失敗: %w", id, err)
	}
	return page, nil
}

// writePage はページをディスク上の正しい位置に書き込む。
func (p *Pager) writePage(page *Page) error {
	_, err := p.file.WriteAt(page.Data[:], int64(page.ID)*PageSize)
	if err != nil {
		return fmt.Errorf("ページ %d の書き込みに失敗: %w", page.ID, err)
	}
	return nil
}
