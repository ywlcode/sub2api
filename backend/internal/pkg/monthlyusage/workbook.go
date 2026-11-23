// Package monthlyusage renders the three monthly usage worksheets from one site's SQL snapshot.
package monthlyusage

import (
	"archive/zip"
	"bufio"
	"embed"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Row contains one record from usage.sql, before any numeric conversion.
type Row struct {
	Day, UserID, Username, RealName, Department, Groups, Model                    string
	Requests, InputTokens, OutputTokens, CacheReadTokens, TotalTokens, ActualCost string
}

var metricHeaders = [...]string{"请求数", "输入 Token", "输出 Token", "缓存读取 Token", "总 Token", "用户扣费（美元）"}

func headers(labels []string, unit string) []string {
	for i, label := range metricHeaders {
		if unit != "" && i >= 1 && i <= 4 {
			label += " (" + unit + ")"
		}
		labels = append(labels, label)
	}
	return labels
}

var (
	detailHeaders     = headers([]string{"站点", "用量日期", "用户 ID", "用户名", "当前真实姓名", "当前所属部门", "使用分组", "模型"}, "")
	departmentHeaders = headers([]string{"站点", "当前所属部门", "注册且未被封禁人数", "活跃用户数"}, "B")
	userHeaders       = headers([]string{"站点", "用户 ID", "用户名", "当前真实姓名", "当前所属部门"}, "M")
)

type profile struct {
	username, name, department string
}

type user struct {
	profile
	totals
}

type department struct {
	active int64
	totals
}

// Both summaries sort by descending total tokens, breaking ties by the original string key.
func rankedKeys[T any](groups map[string]*T, usage func(*T) *totals) []string {
	keys := slices.Collect(maps.Keys(groups))
	slices.SortFunc(keys, func(a, b string) int {
		cmp := usage(groups[b]).counts[4].Cmp(usage(groups[a]).counts[4])
		if cmp != 0 {
			return cmp
		}
		return strings.Compare(a, b)
	})
	return keys
}

type snapshot struct {
	site, month string
	users       map[string]*user
	details     *sheet
}

func (s *snapshot) appendDetail(r Row) error {
	day, err := time.Parse(time.DateOnly, r.Day)
	if err != nil || day.Year() == 0 {
		return fmt.Errorf("invalid usage day %q", r.Day)
	}
	if s.month == "" {
		s.month = r.Day[:7]
	} else if s.month != r.Day[:7] {
		return fmt.Errorf("usage day %q outside month %s", r.Day, s.month)
	}
	if r.UserID == "" {
		return fmt.Errorf("missing user ID")
	}
	metrics, err := parseTotals(r)
	if err != nil {
		return err
	}
	p := profile{r.Username, r.RealName, r.Department}
	u := s.users[r.UserID]
	if u == nil {
		u = &user{profile: p}
		s.users[r.UserID] = u
	} else if u.profile != p {
		return fmt.Errorf("inconsistent user profile in monthly snapshot")
	}
	labels := []string{s.site, r.Day, r.UserID, r.Username, r.RealName, r.Department, r.Groups, r.Model}
	negativeZero := metrics.cost.IsZero() && strings.HasPrefix(r.ActualCost, "-")
	if err := s.details.row(labels, metrics.numbers(nil, 0, negativeZero)); err != nil {
		return err
	}
	u.add(metrics)
	return nil
}

// Build writes one XLSX to w. walk must call its visitor serially, in detail-sheet order,
// and return the first error. The first row establishes the month. An empty walk is valid.
// Build spools private row XML to the system temp directory to measure columns in one pass.
// It begins writing w only after all input and summaries have been validated.
func Build(w io.Writer, site string, registered map[string]int64, walk func(func(Row) error) error) error {
	if w == nil || walk == nil {
		return fmt.Errorf("monthlyusage: missing writer or row source")
	}
	if strings.TrimFunc(site, func(r rune) bool { return unicode.IsSpace(r) || (0x1c <= r && r <= 0x1f) }) == "" {
		return fmt.Errorf("monthlyusage: site must be a nonempty name")
	}
	depts := make(map[string]*department, len(registered))
	for dept, count := range registered {
		if count < 0 {
			return fmt.Errorf("monthlyusage: negative registered headcount")
		}
		depts[dept] = &department{}
	}

	f, err := os.CreateTemp("", "monthly-usage-*.xml")
	if err != nil {
		return err
	}
	// Unlink before writing any rows so a killed process leaves no named XML.
	if err := os.Remove(f.Name()); err != nil {
		// Platforms that cannot unlink an open file remove it after Close.
		defer os.Remove(f.Name())
	}
	defer f.Close()
	body := bufio.NewWriter(f)
	details, err := newSheet(body, detailHeaders)
	if err != nil {
		return err
	}
	users := make(map[string]*user)
	snapshot := snapshot{site: site, users: users, details: details}
	var rowErr error
	err = walk(func(r Row) error {
		if rowErr == nil {
			n := details.rows + 1
			if err := snapshot.appendDetail(r); err != nil {
				rowErr = fmt.Errorf("site %q, detail row %d: %w", site, n, err)
			}
		}
		return rowErr
	})
	if err != nil {
		return fmt.Errorf("monthlyusage: detail source: %w", err)
	}
	if rowErr != nil {
		return rowErr
	}

	for _, u := range users {
		d := depts[u.department]
		if d == nil {
			d = &department{}
			depts[u.department] = d
		}
		d.active++
		d.add(u.totals)
	}
	deptSheet, err := newSheet(body, departmentHeaders)
	if err != nil {
		return err
	}
	for _, key := range rankedKeys(depts, func(d *department) *totals { return &d.totals }) {
		d := depts[key]
		label := key
		if label == "" {
			label = "未填写部门"
		}
		counts := []number{integer(registered[key]), integer(d.active)}
		if err := deptSheet.row([]string{site, label}, d.numbers(counts, -9, false)); err != nil {
			return fmt.Errorf("部门月汇总: %w", err)
		}
	}
	userSheet, err := newSheet(body, userHeaders)
	if err != nil {
		return err
	}
	for _, uid := range rankedKeys(users, func(u *user) *totals { return &u.totals }) {
		u := users[uid]
		labels := []string{site, uid, u.username, u.name, u.department}
		if err := userSheet.row(labels, u.numbers(nil, -6, false)); err != nil {
			return fmt.Errorf("用户月汇总: %w", err)
		}
	}
	if err := body.Flush(); err != nil {
		return err
	}
	return writeWorkbook(w, f, []*sheet{details, deptSheet, userSheet})
}

//go:embed parts/*.xml
var parts embed.FS

func writeWorkbook(w io.Writer, spool io.ReaderAt, sheets []*sheet) error {
	z := zip.NewWriter(w)
	for _, part := range [][2]string{
		{"content-types.xml", "[Content_Types].xml"}, {"root-rels.xml", "_rels/.rels"},
		{"workbook.xml", "xl/workbook.xml"}, {"styles.xml", "xl/styles.xml"},
		{"workbook-rels.xml", "xl/_rels/workbook.xml.rels"},
	} {
		data, err := parts.ReadFile("parts/" + part[0])
		if err != nil {
			return err
		}
		entry, err := z.Create(part[1])
		if err != nil {
			return err
		}
		if _, err = entry.Write(data); err != nil {
			return err
		}
	}
	var offset int64
	for i, s := range sheets {
		entry, err := z.Create(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1))
		if err != nil {
			return err
		}
		if err := s.writeTo(entry, io.NewSectionReader(spool, offset, s.size)); err != nil {
			return err
		}
		offset += s.size
	}
	return z.Close()
}
