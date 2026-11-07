// Package jsonview provides borrowed, read-only views of immutable JSON bodies.
package jsonview

import (
	"unsafe"

	"github.com/tidwall/gjson"
)

// Parse avoids copying the whole body to a string. The caller MUST NOT modify
// body while the result or any derived string is in use. Clone extracted strings
// before keeping them independently of the request (including in small caches).
// This is a view, not a JSON validity check.
func Parse(body []byte) gjson.Result {
	if len(body) == 0 {
		return gjson.Result{}
	}
	return gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body)))
}

// Get has gjson.GetBytes lookup semantics without copying the selected subtree.
// The same immutable-body lifetime rules as Parse apply to its result.
func Get(body []byte, path string) gjson.Result {
	if len(body) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(unsafe.String(unsafe.SliceData(body), len(body)), path)
}
