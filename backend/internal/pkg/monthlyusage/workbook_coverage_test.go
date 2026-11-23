package monthlyusage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestWorkbookRepairsDisplayTextAndPreservesTotals(t *testing.T) {
	const note = "历史覆盖无法确认：仅统计现存明细。"
	// Both department keys render identically. They must retain their own counts.
	model := strings.Repeat("😀", 16383) + "中😀tail"
	a := Row{Day: "2024-02-10", UserID: "1", Department: "A\x00", Username: "x\xff\uffff<&", Model: model,
		Requests: "11", InputTokens: "2", OutputTokens: "3", CacheReadTokens: "4", TotalTokens: "9", ActualCost: "1"}
	b := a
	b.UserID, b.Department, b.Requests = "2", "A\x01", "13"
	var out bytes.Buffer
	err := BuildContext(context.Background(), &out, "site", map[string]int64{a.Department: 4, b.Department: 7}, note, walkRows(a, b))
	if err != nil {
		t.Fatal(err)
	}
	parts := zipParts(t, out.Bytes())
	for i, last := range []string{"N", "J", "K"} {
		var sheet struct {
			Rows []struct {
				Height int `xml:"ht,attr"`
				Cells  []struct {
					Text  string `xml:"is>t"`
					Value string `xml:"v"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
			Filter struct {
				Ref string `xml:"ref,attr"`
			} `xml:"autoFilter"`
			Merge struct {
				Ref string `xml:"ref,attr"`
			} `xml:"mergeCells>mergeCell"`
			Pane struct {
				TopLeft string `xml:"topLeftCell,attr"`
			} `xml:"sheetViews>sheetView>pane"`
		}
		if err := xml.Unmarshal(parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)], &sheet); err != nil {
			t.Fatal(err)
		}
		if len(sheet.Rows) != 4 || sheet.Rows[0].Cells[0].Text != note || sheet.Filter.Ref != "A2:"+last+"4" || sheet.Merge.Ref != "A1:"+last+"1" || sheet.Pane.TopLeft != "A3" {
			t.Fatalf("sheet %d: unexpected rows, notice, filter, merge or pane", i+1)
		}
		for j, row := range sheet.Rows {
			height := 20
			if j == 1 {
				height = 32
			}
			if row.Height != height || (j >= 1 && len(row.Cells) != int(last[0]-'A'+1)) {
				t.Fatalf("sheet %d row %d layout", i+1, j+1)
			}
		}
		first, second := sheet.Rows[2].Cells, sheet.Rows[3].Cells
		switch i {
		case 0:
			if first[3].Text != "x��<&" || first[5].Text != "A�" || first[7].Text != strings.TrimSuffix(model, "😀tail") || first[8].Value != "11" || second[8].Value != "13" {
				t.Fatal("detail text or counts changed")
			}
		case 1:
			if first[1].Text != "A�" || second[1].Text != "A�" || first[2].Value != "4" || second[2].Value != "7" || first[4].Value != "11" || second[4].Value != "13" {
				t.Fatal("department keys merged or counts changed")
			}
		case 2:
			if first[2].Text != "x��<&" || first[5].Value != "11" || second[5].Value != "13" {
				t.Fatal("user labels or counts changed")
			}
		}
	}
}

type cancelSpool struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancelSpool) ReadAt(p []byte, _ int64) (int, error) {
	r.calls++
	r.cancel()
	clear(p)
	return len(p), nil
}

func TestCancelWhilePackingHighlyCompressibleSheet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelSpool{cancel: cancel}
	err := writeWorkbook(ctx, io.Discard, r, []*sheet{{size: 1 << 20, rows: 1, headerRow: 1, columns: []column{{}}}})
	if !errors.Is(err, context.Canceled) || r.calls != 1 {
		t.Fatalf("after first read: err=%v, spool reads=%d", err, r.calls)
	}
}

func TestLabelUTF16Boundary(t *testing.T) {
	prefix := strings.Repeat("a", maxText-1)
	if got := cellLabel(prefix + "😀"); got != prefix {
		t.Fatal("split non-BMP character at boundary")
	}
	if got := cellLabel("\x0b" + prefix + "!"); got != "�"+prefix {
		t.Fatal("repair and truncation produced an incorrect label")
	}
}
