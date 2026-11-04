package middleware

import "strings"

func isTimedInferencePath(path string) bool {
	for _, suffix := range []string{
		"/responses", "/responses/compact", "/chat/completions", "/completions", "/messages",
		"/images/generations", "/images/edits", "/images/variations",
		"/audio/transcriptions", "/audio/translations", "/audio/speech",
		":generateContent", ":streamGenerateContent",
	} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
