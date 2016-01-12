// Implements a basic HTML parser that just checks <title>
// It also annotates mime Type if possible

package unfurlist

import (
	"bytes"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

func BasicParseParseHTML(h *unfurlHandler, result *unfurlResult, htmlBody []byte) bool {
	result.Type = http.DetectContentType(htmlBody)
	switch {
	case strings.HasPrefix(result.Type, "image/"):
		result.Type = "image"
		result.Image = result.URL
	case strings.HasPrefix(result.Type, "text/"):
		result.Type = "website"
		if title, err := findTitle(htmlBody); err == nil {
			result.Title = title
		}
	case strings.HasPrefix(result.Type, "video/"):
		result.Type = "video"
	}
	return true
}

func findTitle(htmlBody []byte) (title string, err error) {
	bodyReader, err := charset.NewReader(bytes.NewReader(htmlBody), "text/html")
	if err != nil {
		return "", err
	}
	node, err := html.Parse(bodyReader)
	if err != nil {
		return "", err
	}
	if t, ok := getFirstElement(node, "title"); ok {
		return t, nil
	}
	return "", errNoTitleTag
}

// getFirstElement returns flattened content of first found element of given
// type
func getFirstElement(n *html.Node, element string) (t string, found bool) {
	if n.Type == html.ElementNode && n.Data == element {
		return flatten(n), true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		t, found = getFirstElement(c, element)
		if found {
			return
		}
	}
	return
}

// flatten returns flattened text content of html node
func flatten(n *html.Node) (res string) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		res += flatten(c)
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	return
}

var (
	errNoTitleTag = errors.New("no title tag found")
)
