package monthlyusage

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxRows = 1048576 // includes the header
	maxText = 32767   // UTF-16 code units
)

var textBounds = map[string][2]int{
	"站点": {10, 18}, "用量日期": {13, 13}, "用户 ID": {9, 24}, "用户名": {10, 22},
	"当前真实姓名": {11, 22}, "当前所属部门": {12, 42}, "使用分组": {13, 26}, "模型": {18, 36},
}

type column struct {
	hist           []int // text widths, clipped to the upper bound
	floor, numeric int
	token          bool
}

func (c column) width(rows int) int {
	if c.hist == nil {
		return max(c.floor, c.numeric)
	}
	// 95th percentile, with five additional characters of outlier headroom.
	target := (rows*95 + 99) / 100
	p95 := 0
	for target > c.hist[p95] {
		target -= c.hist[p95]
		p95++
	}
	longest := len(c.hist) - 1
	for longest > 0 && c.hist[longest] == 0 {
		longest--
	}
	return max(c.floor, min(len(c.hist)-1, longest+2, p95+7))
}

type sheet struct {
	body    *bufio.Writer
	columns []column
	rows    int
	size    int64 // length of this sheet's row XML in the shared spool
}

func newSheet(body *bufio.Writer, headers []string) (*sheet, error) {
	s := &sheet{body: body, columns: make([]column, len(headers))}
	for i, label := range headers {
		c := &s.columns[i]
		if bounds, ok := textBounds[label]; ok {
			c.floor = bounds[0]
			c.hist = make([]int, bounds[1]+1)
			continue
		}
		c.token = strings.Contains(label, "Token")
		switch label {
		case "注册且未被封禁人数":
			c.floor = 15
		case "活跃用户数":
			c.floor = 11
		case "请求数":
			c.floor = 10
		default:
			c.floor = 14
		}
	}
	return s, s.row(headers, nil)
}

func runeWidth(r rune) int {
	if r < 128 {
		return 1
	}
	i := sort.Search(len(widthRanges), func(i int) bool { return widthRanges[i].last >= r })
	if i < len(widthRanges) && widthRanges[i].first <= r {
		return widthRanges[i].width
	}
	return 1
}

func textSpan(value string) (int, error) {
	if !utf8.ValidString(value) {
		return 0, fmt.Errorf("invalid UTF-8 in cell")
	}
	var units, line, widest int
	for _, r := range value {
		units++
		if r > 0xffff {
			units++
		}
		switch {
		case r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029:
			widest, line = max(widest, line), 0
		case r < 32 && r != '\t' || r == 0xfffe || r == 0xffff:
			return 0, fmt.Errorf("invalid XML character in cell")
		default:
			line += runeWidth(r)
		}
	}
	if units > maxText {
		return 0, fmt.Errorf("text exceeds Excel cell length limit")
	}
	return max(widest, line), nil
}

// Match the leading underscore only: consecutive Excel escapes can share an underscore.
func excelEscape(s string) bool {
	if len(s) < 7 || s[1] != 'x' || s[6] != '_' {
		return false
	}
	for _, r := range s[2:6] {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func writeEscaped(b *strings.Builder, s string) {
	start := 0
	for i := 0; i < len(s); i++ {
		var entity string
		switch s[i] {
		case '&':
			entity = "&amp;"
		case '<':
			entity = "&lt;"
		case '>':
			entity = "&gt;"
		case '\r':
			entity = "&#13;"
		case '_':
			if !excelEscape(s[i:]) {
				continue
			}
			entity = "_x005F_"
		default:
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(entity)
		start = i + 1
	}
	b.WriteString(s[start:])
}

func (s *sheet) textCell(b *strings.Builder, col int, value string) error {
	span, err := textSpan(value)
	if err != nil {
		return err
	}
	if s.rows == 1 {
		b.WriteString(` s="1"`)
	} else {
		c := &s.columns[col]
		limit := 28 // precision-preserving text in a numeric column
		if c.hist != nil {
			limit = len(c.hist) - 1
			c.hist[min(span, limit)]++
		} else {
			c.numeric = max(c.numeric, min(limit, span+2))
		}
		if span*5 <= limit*7 {
			b.WriteString(` s="6"`)
		}
	}
	b.WriteString(` t="inlineStr"><is><t xml:space="preserve">`)
	writeEscaped(b, value)
	b.WriteString("</t></is></c>")
	return nil
}

func (s *sheet) row(labels []string, numbers []number) error {
	if s.rows >= maxRows {
		return fmt.Errorf("worksheet exceeds Excel limit of %d rows including header", maxRows)
	}
	s.rows++
	rowID := strconv.Itoa(s.rows)
	height := 20
	if s.rows == 1 {
		height = 32
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<row r="%s" ht="%d" customHeight="1">`, rowID, height)
	for i := range s.columns {
		fmt.Fprintf(&b, `<c r="%c%s"`, 'A'+i, rowID)
		if i < len(labels) {
			if err := s.textCell(&b, i, labels[i]); err != nil {
				return err
			}
			continue
		}
		n, err := numbers[i-len(labels)].render()
		if err != nil {
			return err
		}
		if n.style == 0 {
			if err := s.textCell(&b, i, n.value); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(&b, ` s="%d" t="n"><v>%s</v></c>`, n.style, n.value)
			c := &s.columns[i]
			c.numeric = max(c.numeric, min(255, n.width+2))
		}
	}
	b.WriteString("</row>")
	n, err := s.body.WriteString(b.String())
	s.size += int64(n)
	return err
}

func (s *sheet) writeTo(w io.Writer, rows io.Reader) error {
	var prefix strings.Builder
	prefix.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><sheetFormatPr defaultRowHeight="20"/><cols>`)
	widths := make([]int, len(s.columns))
	common := 0
	for i, c := range s.columns {
		widths[i] = c.width(s.rows - 1)
		if c.token {
			common = max(common, widths[i])
		}
	}
	for i, width := range widths {
		if s.columns[i].token {
			width = common
		}
		fmt.Fprintf(&prefix, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, width)
	}
	prefix.WriteString("</cols><sheetData>")
	if _, err := io.WriteString(w, prefix.String()); err != nil {
		return err
	}
	if _, err := io.Copy(w, rows); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, `</sheetData><autoFilter ref="A1:%c%d"/></worksheet>`, 'A'+len(widths)-1, s.rows)
	return err
}
