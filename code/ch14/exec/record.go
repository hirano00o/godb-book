// Package exec は minidb の実行エンジン(レコード形式・バイトコード VM)を担当する。
package exec

import (
	"encoding/binary"
	"fmt"

	"minidb/sql"
)

// レコードの型タグ。0 は NULL 用に予約してあるが、この章ではまだ使わない。
const (
	tagInteger = 1
	tagText    = 2
)

// EncodeRecord は値の並びを B-Tree のセルに格納するバイト列へ変換する。
//
// レイアウト:
//
//	オフセット 0-1 : 列数(ビッグエンディアン uint16)
//	続く 1 バイトずつ: 列ごとの型タグ(1=INTEGER, 2=TEXT)
//	続くバイト列    : 値本体。INTEGER は int64 を uint64 として 8 バイト
//	                  ビッグエンディアン、TEXT は長さ(uint16 BE)+ バイト列。
//
// TEXT の長さが uint16 に収まらない(65535 バイト超)場合はエラーを返す。
func EncodeRecord(values []sql.Value) ([]byte, error) {
	buf := make([]byte, 2, 2+len(values))
	binary.BigEndian.PutUint16(buf, uint16(len(values)))

	for _, v := range values {
		if v.IsText {
			buf = append(buf, tagText)
		} else {
			buf = append(buf, tagInteger)
		}
	}

	for _, v := range values {
		if v.IsText {
			if len(v.Text) > 0xffff {
				return nil, fmt.Errorf("TEXT が長すぎます: %d バイト(上限 65535)", len(v.Text))
			}
			lenBuf := make([]byte, 2)
			binary.BigEndian.PutUint16(lenBuf, uint16(len(v.Text)))
			buf = append(buf, lenBuf...)
			buf = append(buf, v.Text...)
		} else {
			intBuf := make([]byte, 8)
			binary.BigEndian.PutUint64(intBuf, uint64(v.Int))
			buf = append(buf, intBuf...)
		}
	}
	return buf, nil
}

// DecodeRecord は EncodeRecord が生成したバイト列を値の並びに戻す。
// 途中で切れているなど不正な形式の場合はエラーを返す。
func DecodeRecord(data []byte) ([]sql.Value, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("レコードが短すぎます: %d バイト(列数ヘッダに 2 バイト必要)", len(data))
	}
	n := int(binary.BigEndian.Uint16(data[0:2]))

	tagsEnd := 2 + n
	if len(data) < tagsEnd {
		return nil, fmt.Errorf("型タグが途中で切れています: 列数=%d, 残り %d バイト", n, len(data)-2)
	}
	tags := data[2:tagsEnd]

	values := make([]sql.Value, n)
	pos := tagsEnd
	for i, tag := range tags {
		switch tag {
		case tagInteger:
			if len(data)-pos < 8 {
				return nil, fmt.Errorf("列 %d(INTEGER)の値本体が不足しています: 残り %d バイト", i, len(data)-pos)
			}
			values[i] = sql.Value{Int: int64(binary.BigEndian.Uint64(data[pos : pos+8]))}
			pos += 8
		case tagText:
			if len(data)-pos < 2 {
				return nil, fmt.Errorf("列 %d(TEXT)の長さフィールドが不足しています: 残り %d バイト", i, len(data)-pos)
			}
			textLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			if len(data)-pos < textLen {
				return nil, fmt.Errorf("列 %d(TEXT)の値本体が不足しています: 必要 %d バイト, 残り %d バイト", i, textLen, len(data)-pos)
			}
			values[i] = sql.Value{IsText: true, Text: string(data[pos : pos+textLen])}
			pos += textLen
		default:
			return nil, fmt.Errorf("列 %d: 未知の型タグ 0x%02x", i, tag)
		}
	}
	return values, nil
}
