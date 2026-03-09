package platforms

import "strings"

func buildShortFormText(title, content string, limit int) string {
	cleanTitle := strings.TrimSpace(title)
	cleanContent := strings.TrimSpace(content)
	
	if cleanTitle != "" && !strings.HasPrefix(cleanContent, cleanTitle) {
		cleanContent = cleanTitle + "\n\n" + cleanContent
	}
	
	runes := []rune(cleanContent)
	if len(runes) <= limit {
		return cleanContent
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func BuildShortForm(title, content string) string {
	return buildShortFormText(title, content, 280)
}
