package monthlyusage

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestMonthDiscoversOnlyPlatformsWithRequests(t *testing.T) {
	// Same site and registered departments each time; each workbook discovers its own platforms.
	for _, tc := range []struct {
		day       string
		ids, want []string
	}{
		{"2026-09-01", nil, []string{"明细", "用户月汇总"}},
		{"2026-10-01", []string{"gemini"}, []string{"明细", "部门Gemini平台月汇总", "用户月汇总"}},
		{"2026-11-01", []string{"future/?", `a&"b`, "future:*"}, []string{"明细", `部门A&"b平台月汇总`, "部门Future__平台月汇总", "部门Future__~2平台月汇总", "用户月汇总"}},
	} {
		t.Run(tc.day, func(t *testing.T) {
			metrics := map[string][]string{}
			for _, id := range tc.ids {
				metrics[id] = []string{"1", "0", "0", "0", "0", "0"}
			}
			raw, err := json.Marshal(metrics)
			if err != nil {
				t.Fatal(err)
			}
			row := Row{Day: tc.day, UserID: "1", Department: "used", Requests: fmt.Sprint(1 + len(tc.ids)), InputTokens: "0", OutputTokens: "0", CacheReadTokens: "0", TotalTokens: "0", ActualCost: "0", PlatformUsage: string(raw)}
			var out bytes.Buffer
			if err := Build(&out, "site", map[string]int64{"unused": 8}, walkRows(row)); err != nil {
				t.Fatal(err)
			}
			parts := zipParts(t, out.Bytes())
			var catalog struct {
				Sheets []struct {
					Name string `xml:"name,attr"`
				} `xml:"sheets>sheet"`
			}
			if err := xml.Unmarshal(parts["xl/workbook.xml"], &catalog); err != nil {
				t.Fatal(err)
			}
			var names []string
			for i, s := range catalog.Sheets {
				names = append(names, s.Name)
				data := parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)]
				if len(data) == 0 {
					t.Fatalf("missing worksheet %q", s.Name)
				}
				if i > 0 && i < len(tc.want)-1 && !bytes.Contains(data, []byte("unused")) {
					t.Fatalf("registered department missing from %q", s.Name)
				}
			}
			if !slices.Equal(names, tc.want) || len(parts) != 5+len(tc.want) {
				t.Fatalf("worksheets %v, want %v", names, tc.want)
			}
		})
	}
}

func TestLongPlatformNamesRemainDistinct(t *testing.T) {
	prefix := strings.Repeat("😀", 20)
	used := make(map[string]bool)
	for _, tc := range []struct{ id, want string }{
		{prefix + "a", "部门" + strings.Repeat("😀", 12) + "平台月汇总"},
		{prefix + "b", "部门" + strings.Repeat("😀", 11) + "~2平台月汇总"},
	} {
		if got := departmentSheetName(tc.id, used); got != tc.want {
			t.Errorf("sheet %q, want %q", got, tc.want)
		}
	}
}

func TestPlatformSummariesKeepMixedDetailAndDeduplicateActiveUsers(t *testing.T) {
	// One daily user/model row includes three OpenAI requests, one DeepSeek request
	// and two requests outside either platform. The latter still count in user totals.
	mixed := Row{Day: "2026-11-01", UserID: "7", Department: "研发", Model: "shared",
		Requests: "6", InputTokens: "9000000000", OutputTokens: "0", CacheReadTokens: "0", TotalTokens: "9000000000", ActualCost: "5.75",
		PlatformUsage: `{"openai":["3","2000000000","0","0","2000000000","0.75"],"deepseek":["1","1000000000","0","0","1000000000","0"]}`}
	again := Row{Day: "2026-11-02", UserID: "7", Department: "研发", Model: "shared",
		Requests: "1", InputTokens: "1000000000", OutputTokens: "0", CacheReadTokens: "0", TotalTokens: "1000000000", ActualCost: "0.75",
		PlatformUsage: `{"openai":["1","1000000000","0","0","1000000000","0.75"]}`}
	free := Row{Day: again.Day, UserID: "8", Department: "研发", Model: "shared",
		Requests: "1", InputTokens: "0", OutputTokens: "0", CacheReadTokens: "0", TotalTokens: "0", ActualCost: "0",
		PlatformUsage: `{"deepseek":["1","0","0","0","0","0"]}`}
	var out bytes.Buffer
	if err := Build(&out, "site", map[string]int64{"研发": 9}, walkRows(mixed, again, free)); err != nil {
		t.Fatal(err)
	}
	parts := zipParts(t, out.Bytes())
	for i, tc := range []struct {
		rows  int
		cells map[string]string
	}{
		{4, map[string]string{"I2": "6", "M2": "9000000000", "N2": "5.75"}},
		{2, map[string]string{"C2": "9", "D2": "2", "E2": "2", "I2": "1.000000000", "J2": "0"}},
		{2, map[string]string{"C2": "9", "D2": "1", "E2": "4", "I2": "3.000000000", "J2": "1"}},
		{3, map[string]string{"F2": "7", "J2": "10000.000000", "K2": "6", "F3": "1"}},
	} {
		var data struct {
			Rows []struct {
				Cells []struct {
					Ref   string `xml:"r,attr"`
					Value string `xml:"v"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err := xml.Unmarshal(parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)], &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Rows) != tc.rows {
			t.Errorf("sheet %d: got %d rows, want %d", i+1, len(data.Rows), tc.rows)
		}
		got := map[string]string{}
		for _, row := range data.Rows {
			for _, c := range row.Cells {
				got[c.Ref] = c.Value
			}
		}
		for ref, want := range tc.cells {
			if got[ref] != want {
				t.Errorf("sheet %d %s = %q, want %q", i+1, ref, got[ref], want)
			}
		}
	}
}

func TestWorkbookRepairsDisplayTextAndPreservesTotals(t *testing.T) {
	const note = "历史覆盖无法确认：仅统计现存明细。"
	// Both department keys render identically. They must retain their own counts.
	model := strings.Repeat("😀", 16383) + "中😀tail"
	a := Row{Day: "2024-02-10", UserID: "1", Department: "A\x00", Username: "x\xff\uffff<&", Model: model,
		Requests: "11", InputTokens: "2", OutputTokens: "3", CacheReadTokens: "4", TotalTokens: "9", ActualCost: "1"}
	b := a
	b.UserID, b.Department, b.Requests = "2", "A\x01", "13"
	a.PlatformUsage = `{"other":["11","2","3","4","9","1"]}`
	b.PlatformUsage = `{"other":["13","2","3","4","9","1"]}`
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
