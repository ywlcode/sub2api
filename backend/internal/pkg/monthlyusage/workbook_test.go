package monthlyusage

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func walkRows(rows ...Row) func(func(Row) error) error {
	return func(visit func(Row) error) error {
		for _, r := range rows {
			if err := visit(r); err != nil {
				return err
			}
		}
		return nil
	}
}

func zipParts(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	parts := make(map[string][]byte)
	for _, file := range z.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := parts[file.Name]; ok {
			t.Fatalf("duplicate ZIP member %s", file.Name)
		}
		parts[file.Name] = body
	}
	return parts
}

func TestPythonWorkbookParity(t *testing.T) {
	var input struct {
		Site       string
		Registered map[string]int64
		Rows       [][]string
	}
	raw, err := os.ReadFile("testdata/case.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	var rows []Row
	for _, v := range input.Rows {
		if len(v) != 13 {
			t.Fatalf("fixture row has %d columns", len(v))
		}
		rows = append(rows, Row{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8], v[9], v[10], v[11], v[12]})
	}
	var output bytes.Buffer
	if err := Build(&output, input.Site, input.Registered, walkRows(rows...)); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile("testdata/reference.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	got, want := zipParts(t, output.Bytes()), zipParts(t, ref)
	if len(got) != len(want) {
		t.Fatalf("ZIP member count = %d, want %d", len(got), len(want))
	}
	for name, expected := range want {
		actual := got[name]
		if bytes.Equal(actual, expected) {
			continue
		}
		i := 0
		for i < min(len(actual), len(expected)) && actual[i] == expected[i] {
			i++
		}
		t.Errorf("%s differs at byte %d\n got ...%s\nwant ...%s", name, i,
			actual[max(0, i-30):min(len(actual), i+100)], expected[max(0, i-30):min(len(expected), i+100)])
	}
}

func TestEmptyMonth(t *testing.T) {
	var out bytes.Buffer
	if err := Build(&out, "site", nil, walkRows()); err != nil {
		t.Fatal(err)
	}
	parts := zipParts(t, out.Bytes())
	for file, end := range map[string]string{"sheet1": "N1", "sheet2": "J1", "sheet3": "K1"} {
		xml := string(parts["xl/worksheets/"+file+".xml"])
		if strings.Count(xml, "<row ") != 1 || !strings.Contains(xml, `<autoFilter ref="A1:`+end+`"/>`) {
			t.Errorf("%s: expected header only with filter through %s", file, end)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	base := Row{Day: "2024-02-29", UserID: "0", Requests: "0", InputTokens: "1", OutputTokens: "2", CacheReadTokens: "3", TotalTokens: "9", ActualCost: "-1.01"}
	for label, mutate := range map[string]func(*Row){
		"invalid day":           func(r *Row) { r.Day = "2024-02-30" },
		"mixed months":          func(r *Row) { r.Day = "2024-03-01" },
		"year zero":             func(r *Row) { r.Day = "0000-02-01" },
		"missing user":          func(r *Row) { r.UserID = "" },
		"inconsistent profile":  func(r *Row) { r.Department = "other" },
		"fractional count":      func(r *Row) { r.Requests = "1.5" },
		"signed count":          func(r *Row) { r.TotalTokens = "-1" },
		"long count":            func(r *Row) { r.InputTokens = strings.Repeat("0", 309) },
		"nonfinite":             func(r *Row) { r.ActualCost = "NaN" },
		"invalid decimal scale": func(r *Row) { r.ActualCost = "0e-32768" },
		"below minimum":         func(r *Row) { r.ActualCost = "2.22507385850720139e-308" },
		"above maximum":         func(r *Row) { r.ActualCost = "-9.999999999999991e307" },
	} {
		t.Run(label, func(t *testing.T) {
			r := base
			mutate(&r)
			var out bytes.Buffer
			if err := Build(&out, "site", nil, walkRows(base, r)); err == nil || out.Len() != 0 {
				t.Fatalf("invalid rows: err=%v, %d bytes written", err, out.Len())
			}
		})
	}

	// Each detail value fits; the aggregated requests exceed Excel's numeric range.
	base.Requests = "5" + strings.Repeat("0", 307)
	if err := Build(io.Discard, "site", nil, walkRows(base, base)); err == nil {
		t.Fatal("expected aggregate overflow")
	}
	if err := Build(io.Discard, "site", map[string]int64{"研发": -1}, walkRows()); err == nil {
		t.Fatal("accepted negative registered headcount")
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestSpoolUnlinkedBeforeReadingRows(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("checks Linux open-file unlink semantics")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	err := Build(io.Discard, "site", nil, func(func(Row) error) error {
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(files) != 0 {
			t.Fatalf("found %d named temporary files while rendering", len(files))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildErrorsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	failure := errors.New("injected failure")
	var out bytes.Buffer
	err := Build(&out, "site", nil, func(func(Row) error) error { return failure })
	if !errors.Is(err, failure) || out.Len() != 0 {
		t.Fatalf("source failure: %v (%d bytes)", err, out.Len())
	}
	if err := Build(failingWriter{failure}, "site", nil, walkRows()); !errors.Is(err, failure) {
		t.Fatalf("writer failure: %v", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Fatalf("temporary files after failure: %v, %v", left, err)
	}
}

func TestLayoutWithRareWideDepartments(t *testing.T) {
	// Across all sheets there are 19 distinct short names and one outlier.
	// P95 is 12 display columns: four Chinese characters plus four ASCII digits.
	rows := make([]Row, 20)
	for i := range rows {
		id := fmt.Sprintf("%04d", i)
		rows[i] = Row{Day: "2024-02-01", UserID: id, Department: "测试部门" + id,
			Requests: "1", InputTokens: "2", OutputTokens: "3", CacheReadTokens: "4", TotalTokens: "10", ActualCost: "0.97"}
	}
	rows[0].Department = strings.Repeat("宽", 100)
	var out bytes.Buffer
	if err := Build(&out, "site", nil, walkRows(rows...)); err != nil {
		t.Fatal(err)
	}
	parts := zipParts(t, out.Bytes())
	for i, col := range []int{6, 2, 5} {
		xml := parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)]
		if !bytes.Contains(xml, []byte(fmt.Sprintf(`<col min="%d" max="%d" width="19" customWidth="1"/>`, col, col))) {
			t.Fatalf("sheet %d: unexpected department width", i+1)
		}
	}
}

func TestRowLimitIncludesHeader(t *testing.T) {
	s := sheet{rows: maxRows}
	if err := s.row(nil, nil); err == nil {
		t.Fatal("accepted worksheet row beyond Excel limit")
	}
}
