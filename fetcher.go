package unfurlist

import "net/url"

// FetchFunc defines custom metadata fetchers that can be attached to unfurl
// handler
type FetchFunc func(*url.URL) (*Metadata, bool)

// Metadata represents metadata retrieved by FetchFunc. At least one of Title,
// Description or Image attributes are expected to be non-empty.
type Metadata struct {
	Title       string
	Type        string // TODO: make this int8 w/enum constants
	Description string
	Image       string // image/thumbnail url
	ImageWidth  int
	ImageHeight int
	IconType    string // link rel=icon type (e.g. image/png)
	IconUrl     string // link rel=icon href (URL)
}

// Valid check that at least one of the mandatory attributes is non-empty
func (m *Metadata) Valid() bool {
	return m != nil || m.Title != "" || m.Description != "" || m.Image != ""
}
