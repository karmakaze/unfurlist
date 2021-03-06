// Package unfurlist implements a service that unfurls URLs and provides more information about them.
//
// The current version supports Open Graph and oEmbed formats, Twitter card format is also planned.
// If the URL does not support common formats, unfurlist falls back to looking at common HTML tags
// such as <title> and <meta name="description">.
//
// The endpoint accepts GET and POST requests with `content` as the main argument.
// It then returns a JSON encoded list of URLs that were parsed.
//
// If an URL lacks an attribute (e.g. `image`) then this attribute will be omitted from the result.
//
// Example:
//
//     ?content=Check+this+out+https://www.youtube.com/watch?v=dQw4w9WgXcQ
//
// Will return:
//
//     Type: "application/json"
//
// 	[
// 		{
// 			"title": "Rick Astley - Never Gonna Give You Up",
// 			"url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
// 			"url_type": "video",
// 			"site_name": "YouTube",
// 			"image": "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg"
// 		}
// 	]
//
// If handler was configured with FetchImageSize=true in its config, each hash
// may have additional fields `image_width` and `image_height` specifying
// dimensions of image provided by `image` attribute.
//
// Additionally you can supply `callback` to wrap the result in a JavaScript callback (JSONP),
// the type of this response would be "application/x-javascript"
//
// Security
//
// Care should be taken when running this inside internal network since it may
// disclose internal endpoints. It is a good idea to run the service on
// a separate host in an isolated subnet.
//
// Alternatively access to internal resources may be limited with firewall
// rules, i.e. if service is running as 'unfurlist' user on linux box, the
// following iptables rules can reduce chances of it connecting to internal
// endpoints (note this example is for ipv4 only!):
//
//	iptables -A OUTPUT -m owner --uid-owner unfurlist -p tcp --syn \
//		-d 127/8,10/8,169.254/16,172.16/12,192.168/16 \
//		-j REJECT --reject-with icmp-net-prohibited
//	ip6tables -A OUTPUT -m owner --uid-owner unfurlist -p tcp --syn \
//		-d ::1/128,fe80::/10 \
//		-j REJECT --reject-with adm-prohibited
package unfurlist

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/html/charset"

	"github.com/artyom/oembed"
	"github.com/bradfitz/gomemcache/memcache"
)

const defaultMaxBodyChunkSize = 1024 * 64 //64KB

type unfurlHandler struct {
	HTTPClient       *http.Client
	Log              Logger
	oembedLookupFunc oembed.LookupFunc
	Cache            *memcache.Client
	MaxBodyChunkSize int64
	FetchImageSize   bool

	// Headers specify key-value pairs of extra headers to add to each
	// outgoing request made by Handler. Headers length must be even,
	// otherwise Headers are ignored.
	Headers []string

	titleBlacklist []string

	pmap *prefixMap // built from BlacklistPrefix

	fetchers []FetchFunc
	mu       sync.Mutex
	inFlight map[string]chan struct{} // in-flight urls processed
}

// Result that's returned back to the client
type unfurlResult struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Type        string `json:"url_type,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	Image       string `json:"image,omitempty"`
	ImageWidth  int    `json:"image_width,omitempty"`
	ImageHeight int    `json:"image_height,omitempty"`
	IconUrl     string `json:"icon"`
	IconType    string `json:"icon_type"`

	idx int
}

func (u *unfurlResult) Empty() bool {
	return u.URL == "" && u.Title == "" && u.Type == "" &&
		u.Description == "" && u.Image == ""
}

func (u *unfurlResult) normalize() {
	b := bytes.Join(bytes.Fields([]byte(u.Title)), []byte{' '})
	u.Title = string(b)
}

func (u *unfurlResult) Merge(u2 *unfurlResult) {
	if u2 == nil {
		return
	}
	if u.URL == "" {
		u.URL = u2.URL
	}
	if u.Title == "" {
		u.Title = u2.Title
	}
	if u.Type == "" {
		u.Type = u2.Type
	}
	if u.Description == "" {
		u.Description = u2.Description
	}
	if u.SiteName == "" {
		u.SiteName = u2.SiteName
	}
	if u.Image == "" {
		u.Image = u2.Image
	}
	if u.ImageWidth == 0 {
		u.ImageWidth = u2.ImageWidth
	}
	if u.ImageHeight == 0 {
		u.ImageHeight = u2.ImageHeight
	}
	if u.IconUrl == "" {
		if u2.IconUrl != "" {
			u.IconType = u2.IconType
			u.IconUrl = u2.IconUrl
		}
	}
}

type unfurlResults []*unfurlResult

func (rs unfurlResults) Len() int           { return len(rs) }
func (rs unfurlResults) Less(i, j int) bool { return rs[i].idx < rs[j].idx }
func (rs unfurlResults) Swap(i, j int)      { rs[i], rs[j] = rs[j], rs[i] }

// ConfFunc is used to configure new unfurl handler; such functions should be
// used as arguments to New function
type ConfFunc func(*unfurlHandler) *unfurlHandler

// New returns new initialized unfurl handler. If no configuration functions
// provided, sane defaults would be used.
func New(conf ...ConfFunc) http.Handler {
	h := &unfurlHandler{
		inFlight: make(map[string]chan struct{}),
	}
	for _, f := range conf {
		h = f(h)
	}
	if h.HTTPClient == nil {
		h.HTTPClient = http.DefaultClient
	}
	if len(h.Headers)%2 != 0 {
		h.Headers = nil
	}
	if h.MaxBodyChunkSize == 0 {
		h.MaxBodyChunkSize = defaultMaxBodyChunkSize
	}
	if h.Log == nil {
		h.Log = log.New(ioutil.Discard, "", 0)
	}
	fn, err := oembed.Providers(strings.NewReader(providersData))
	if err != nil {
		panic(err)
	}
	h.oembedLookupFunc = fn
	return h
}

func (h *unfurlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	content := r.Form.Get("content")
	callback := r.Form.Get("callback")

	if content == "" {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	urls := parseURLsMax(content, 20)

	jobResults := make(chan *unfurlResult, 1)
	results := make(unfurlResults, 0, len(urls))
	ctx := r.Context()

	for i, r := range urls {
		go func(ctx context.Context, i int, link string, jobResults chan *unfurlResult) {
			select {
			case jobResults <- h.processURL(ctx, i, link):
			case <-ctx.Done():
			}
		}(ctx, i, r, jobResults)
	}
	for i := 0; i < len(urls); i++ {
		select {
		case <-ctx.Done():
			return
		case res := <-jobResults:
			results = append(results, res)
		}
	}

	sort.Sort(results)
	for _, r := range results {
		r.normalize()
	}

	if callback != "" {
		w.Header().Set("Content-Type", "application/x-javascript")
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	if callback != "" {
		io.WriteString(w, callback+"(")
		json.NewEncoder(w).Encode(results)
		w.Write([]byte(")"))
		return
	}
	json.NewEncoder(w).Encode(results)
}

// Processes the URL by first looking in cache, then trying oEmbed, OpenGraph
// If no match is found the result will be an object that just contains the URL
func (h *unfurlHandler) processURL(ctx context.Context, i int, link string) *unfurlResult {
	result := &unfurlResult{idx: i, URL: link}
	waitLogged := false
	for {
		// spinlock-like loop to ensure we don't have two in-flight
		// outgoing requests for the same link
		h.mu.Lock()
		if ch, ok := h.inFlight[link]; ok {
			h.mu.Unlock()
			if !waitLogged {
				h.Log.Printf("Wait for in-flight request to complete %q", link)
				waitLogged = true
			}
			select {
			case <-ch: // block until another goroutine processes the same url
			case <-ctx.Done():
				return result
			}
		} else {
			ch = make(chan struct{})
			h.inFlight[link] = ch
			h.mu.Unlock()
			defer func() {
				h.mu.Lock()
				delete(h.inFlight, link)
				h.mu.Unlock()
				close(ch)
			}()
			break
		}
	}

	if h.pmap != nil && h.pmap.Match(link) { // blacklisted
		h.Log.Printf("Blacklisted %q", link)
		return result
	}

	if mc := h.Cache; mc != nil {
		if it, err := mc.Get(mcKey(link)); err == nil {
			var cached unfurlResult
			if err = json.Unmarshal(it.Value, &cached); err == nil {
				h.Log.Printf("Cache hit for %q", link)
				cached.idx = i
				return &cached
			}
		}
	}
	chunk, err := h.fetchData(ctx, result.URL)
	if err != nil {
		return result
	}
	for _, f := range h.fetchers {
		meta, ok := f(chunk.url)
		if !ok || !meta.Valid() {
			continue
		}
		result.Title = meta.Title
		result.Type = meta.Type
		result.Description = meta.Description
		result.Image = meta.Image
		result.ImageWidth = meta.ImageWidth
		result.ImageHeight = meta.ImageHeight
		result.IconUrl = meta.IconUrl
		result.IconType = meta.IconType
		goto hasMatch
	}

	if res := openGraphParseHTML(chunk); res != nil {
		if !blacklisted(h.titleBlacklist, res.Title) {
			result.Merge(res)
			goto hasMatch
		}
	}
	if endpoint, found := chunk.oembedEndpoint(h.oembedLookupFunc); found {
		if res, err := fetchOembed(ctx, endpoint, h.httpGet); err == nil {
			result.Merge(res)
			goto hasMatch
		}
	}

hasMatch:
	if res := basicParseHTML(chunk); res != nil {
		if !blacklisted(h.titleBlacklist, res.Title) {
			result.Merge(res)
		}
	}

	if absURL, err := absoluteImageURL(result.URL, result.IconUrl); err == nil {
		result.IconUrl = absURL
	}
	switch absURL, err := absoluteImageURL(result.URL, result.Image); err {
	case errEmptyImageURL:
	case nil:
		switch {
		case validURL(absURL):
			result.Image = absURL
		default:
			result.Image = ""
		}
		if result.Image != "" && h.FetchImageSize && (result.ImageWidth == 0 || result.ImageHeight == 0) {
			if width, height, err := imageDimensions(ctx, h.HTTPClient, result.Image); err != nil {
				h.Log.Printf("dimensions detect for image %q: %v", result.Image, err)
			} else {
				result.ImageWidth, result.ImageHeight = width, height
			}
		}
	default:
		h.Log.Printf("cannot get absolute image url for %q: %v", result.Image, err)
		result.Image, result.ImageWidth, result.ImageHeight = "", 0, 0
	}

	if mc := h.Cache; mc != nil && !result.Empty() {
		if cdata, err := json.Marshal(result); err == nil {
			h.Log.Printf("Cache update for %q", link)
			mc.Set(&memcache.Item{Key: mcKey(link), Value: cdata})
		}
	}
	return result
}

// pageChunk describes first chunk of resource
type pageChunk struct {
	data []byte   // first chunk of resource data
	url  *url.URL // final url resource was fetched from (after all redirects)
	ct   string   // Content-Type as reported by server
}

func (p *pageChunk) oembedEndpoint(fn oembed.LookupFunc) (url string, found bool) {
	if p == nil || fn == nil {
		return "", false
	}
	if u, ok := fn(p.url.String()); ok {
		return u, true
	}
	r, err := charset.NewReader(bytes.NewReader(p.data), p.ct)
	if err != nil {
		return "", false
	}
	if u, ok, err := oembed.Discover(r); err == nil && ok {
		return u, true
	}
	return "", false
}

func (h *unfurlHandler) httpGet(ctx context.Context, URL string) (*http.Response, error) {
	client := h.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodGet, URL, nil)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(h.Headers); i += 2 {
		req.Header.Set(h.Headers[i], h.Headers[i+1])
	}
	req = req.WithContext(ctx)
	return client.Do(req)
}

// fetchData fetches the first chunk of the resource. The chunk size is
// determined by h.MaxBodyChunkSize.
func (h *unfurlHandler) fetchData(ctx context.Context, URL string) (*pageChunk, error) {
	resp, err := h.httpGet(ctx, URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, errors.New("bad status: " + resp.Status)
	}
	if resp.Header.Get("Content-Encoding") == "deflate" &&
		strings.HasSuffix(resp.Request.Host, "twitter.com") {
		// twitter sends unsolicited deflate-encoded responses
		// violating RFC; workaround this.
		// See https://golang.org/issues/18779 for background
		var err error
		if resp.Body, err = zlib.NewReader(resp.Body); err != nil {
			return nil, err
		}
	}
	head, err := ioutil.ReadAll(io.LimitReader(resp.Body, h.MaxBodyChunkSize))
	if err != nil {
		return nil, err
	}
	return &pageChunk{
		data: head,
		url:  resp.Request.URL,
		ct:   resp.Header.Get("Content-Type"),
	}, nil
}

// mcKey returns string of hex representation of sha1 sum of string provided.
// Used to get safe keys to use with memcached
func mcKey(s string) string {
	h := sha1.New()
	io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func blacklisted(blacklist []string, title string) bool {
	if title == "" || len(blacklist) == 0 {
		return false
	}
	lt := strings.ToLower(title)
	for _, s := range blacklist {
		if strings.Contains(lt, s) {
			return true
		}
	}
	return false
}

//go:generate go run assets-update.go
