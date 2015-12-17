// Implements a basic HTML parser that just checks <title>
// It also annotates mime Type if possible

package unfurlist

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

var ReTitle = regexp.MustCompile("<title[^>]*>(.+)</title>")

func BasicParseParseHTML(h *unfurlHandler, result *unfurlResult, htmlBody string, resp chan<- serviceResult) {
	serviceResult := serviceResult{Result: result, HasMatch: false}

	title, err := findTitle(htmlBody)
	if err == nil {
		result.Title = title
		serviceResult.HasMatch = true
	}

	result.Type = http.DetectContentType([]byte(htmlBody))

	if strings.Index(result.Type, "image/") != -1 {
		result.Type = "image"
		result.Image = result.URL
	} else if strings.Index(result.Type, "text/") != -1 {
		result.Type = "website"
	} else if strings.Index(result.Type, "video/") != -1 {
		result.Type = "video"
	}

	resp <- serviceResult
}

func findTitle(htmlBody string) (title string, err error) {
	match := ReTitle.FindStringSubmatch(htmlBody)
	if len(match) == 2 {
		return match[1], nil
	} else {
		return title, errors.New("no title tag found")
	}
}
