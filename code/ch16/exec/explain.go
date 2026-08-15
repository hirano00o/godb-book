package exec

import (
	"fmt"
	"strconv"
	"strings"
)

// Explain はプログラムを人間が読める一覧表として書式化する。
// 第 1 章で見た SQLite の EXPLAIN と同じ、addr / opcode / p1 p2 p3 / p4 / comment の
// 列幅を揃えた等幅の表(ヘッダ行 + 罫線)で返す。
func Explain(program []Instr) string {
	headers := []string{"addr", "opcode", "p1", "p2", "p3", "p4", "comment"}
	rows := make([][]string, len(program))
	for i, instr := range program {
		rows[i] = []string{
			strconv.Itoa(i),
			instr.Op.String(),
			strconv.Itoa(instr.P1),
			strconv.Itoa(instr.P2),
			strconv.Itoa(instr.P3),
			instr.P4,
			instr.Comment,
		}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len([]rune(h))
	}
	for _, row := range rows {
		for i, cell := range row {
			if n := len([]rune(cell)); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	writeRow(&b, headers, widths)
	writeSeparator(&b, widths)
	for _, row := range rows {
		writeRow(&b, row, widths)
	}
	return b.String()
}

// writeRow は 1 行ぶんのセルを、列幅に合わせて左詰めで空白パディングしながら書く。
// 最終列(comment)は末尾の空白パディングを省く。
func writeRow(b *strings.Builder, cells []string, widths []int) {
	for i, cell := range cells {
		if i > 0 {
			b.WriteString("  ")
		}
		if i == len(cells)-1 {
			b.WriteString(cell)
			continue
		}
		fmt.Fprintf(b, "%-*s", widths[i], cell)
	}
	b.WriteByte('\n')
}

// writeSeparator はヘッダ行の下に引く罫線を書く。
func writeSeparator(b *strings.Builder, widths []int) {
	cells := make([]string, len(widths))
	for i, w := range widths {
		cells[i] = strings.Repeat("-", w)
	}
	writeRow(b, cells, widths)
}
