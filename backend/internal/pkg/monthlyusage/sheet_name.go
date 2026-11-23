package monthlyusage

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/cases"
)

// Keep distinct platforms separate even if Excel's character or length limits
// turn their display labels into the same worksheet name.
func departmentSheetName(id string, used map[string]bool) string {
	const prefix, suffix = "部门", "平台月汇总"
	label := id
	if id == "openai" {
		label = "OpenAI"
	} else if id[0] >= 'a' && id[0] <= 'z' {
		label = strings.ToUpper(id[:1]) + id[1:]
	}
	label = strings.Map(func(r rune) rune {
		if r < 32 || r == 0xfffe || r == 0xffff || strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, label)
	for n := 1; ; n++ {
		disambiguator := ""
		if n > 1 {
			disambiguator = "~" + strconv.Itoa(n)
		}
		// The prefix and suffix contain seven BMP characters in total.
		room := 31 - len([]rune(prefix+suffix)) - len(disambiguator)
		part := label
		for i, r := range label {
			room -= utf16.RuneLen(r)
			if room < 0 {
				part = label[:i]
				break
			}
		}
		name := prefix + part + disambiguator + suffix
		key := cases.Fold().String(name)
		if !used[key] {
			used[key] = true
			return name
		}
	}
}
