package exec

import (
	"reflect"
	"strings"
	"testing"

	"minidb/sql"
)

// EncodeRecord で作ったバイト列を DecodeRecord に戻すと元の値の並びに一致することを確認する。
func TestEncodeDecodeRecordRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		values []sql.Value
	}{
		{
			name:   "INTEGER のみ",
			values: []sql.Value{{Int: 1}, {Int: 2}, {Int: 3}},
		},
		{
			name:   "TEXT のみ",
			values: []sql.Value{{IsText: true, Text: "Alice"}, {IsText: true, Text: "Bob"}},
		},
		{
			name:   "INTEGER と TEXT の混在",
			values: []sql.Value{{Int: 1}, {IsText: true, Text: "Alice"}, {Int: 30}},
		},
		{
			name:   "負の整数",
			values: []sql.Value{{Int: -1}, {Int: -9223372036854775808}},
		},
		{
			name:   "空文字列",
			values: []sql.Value{{IsText: true, Text: ""}},
		},
		{
			name:   "列数 0",
			values: []sql.Value{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := EncodeRecord(tc.values)
			if err != nil {
				t.Fatalf("EncodeRecord failed: %v", err)
			}
			got, err := DecodeRecord(data)
			if err != nil {
				t.Fatalf("DecodeRecord failed: %v", err)
			}
			if len(got) == 0 && len(tc.values) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.values) {
				t.Fatalf("DecodeRecord(EncodeRecord(%v)) = %v, want %v", tc.values, got, tc.values)
			}
		})
	}
}

// TEXT が 65535 バイトを超える場合、EncodeRecord はエラーを返す。
func TestEncodeRecordTextTooLarge(t *testing.T) {
	values := []sql.Value{{IsText: true, Text: strings.Repeat("x", 0x10000)}}
	if _, err := EncodeRecord(values); err == nil {
		t.Fatal("EncodeRecord error = nil, want エラー")
	}
}

// 不正な形式のバイト列を渡したとき、DecodeRecord がエラーを返すことを確認する。
func TestDecodeRecordErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "空入力",
			data: []byte{},
		},
		{
			name: "型タグ途中で切れている",
			// 列数 2 だが型タグは 1 バイトしかない。
			data: []byte{0x00, 0x02, tagInteger},
		},
		{
			name: "INTEGER の値本体が足りない",
			// 列数 1, 型タグ INTEGER だが値本体が 8 バイトに満たない。
			data: []byte{0x00, 0x01, tagInteger, 0x00, 0x00, 0x00},
		},
		{
			name: "TEXT の長さフィールドが足りない",
			data: []byte{0x00, 0x01, tagText, 0x00},
		},
		{
			name: "TEXT の値本体が足りない",
			// 長さ 5 バイトを宣言しているが実際には 2 バイトしかない。
			data: []byte{0x00, 0x01, tagText, 0x00, 0x05, 'a', 'b'},
		},
		{
			name: "未知の型タグ",
			data: []byte{0x00, 0x01, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeRecord(tc.data); err == nil {
				t.Fatal("DecodeRecord error = nil, want エラー")
			}
		})
	}
}
