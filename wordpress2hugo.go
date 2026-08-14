package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
	"gopkg.in/yaml.v3"
)

// RSS structs with namespace support
// Note: encoding/xml matches namespaced elements when using the expanded form {namespace}local

type RSS struct {
	Channel Channel `xml:"channel"`
}

type Channel struct {
	Title string `xml:"title"`
	Items []Item `xml:"item"`
}

type Item struct {
	Title           string     `xml:"title"`
	Link            string     `xml:"link"`
	PubDate         string     `xml:"pubDate"`
	GUID            string     `xml:"guid"`
	Creator         string     `xml:"{http://purl.org/dc/elements/1.1/}creator"`
	Description     string     `xml:"description"`
	ContentEncoded  string     `xml:"{http://purl.org/rss/1.0/modules/content/}encoded"`
	Categories      []Category `xml:"category"`
	CommentsFeedURL string     `xml:"{http://wellformedweb.org/CommentAPI/}commentRss"`
}

type Category struct {
	Domain string `xml:"domain,attr"`
	Value  string `xml:",chardata"`
}

// Front matter structure for YAML

type FrontMatter struct {
	Title      string    `yaml:"title"`
	Date       time.Time `yaml:"date"`
	Draft      bool      `yaml:"draft"`
	Tags       []string  `yaml:"tags"`
	Aliases    []string  `yaml:"aliases"`
	Categories []string  `yaml:"categories"`
}

var (
	feedURL     = flag.String("feed", "https://blog.breyer.berlin/feed/", "RSS feed URL or file path")
	outDir      = flag.String("out", "posts", "Output directory for Hugo page bundles")
	timezone    = flag.String("tz", "Europe/Berlin", "IANA timezone for front matter dates, e.g. Europe/Berlin")
	limitItems  = flag.Int("limit", 1, "Process only the first N items (0 = all)")
	concurrency = flag.Int("concurrency", 6, "Concurrent image download workers")
	timeoutSec  = flag.Int("timeout", 120, "Per-request download timeout in seconds")
	retries     = flag.Int("retries", 3, "Number of download retries on failure")
	perHost     = flag.Int("perhost", 4, "Max concurrent downloads per host")
	verbose     = flag.Bool("v", true, "Verbose output")
	clean       = flag.Bool("clean", true, "Delete the output folder before run")
)

func main() {
	flag.Parse()

	if *clean {
		if err := cleanOutput(*outDir); err != nil {
			log.Fatalf("clean output: %v", err)
		}
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}
	rss, err := loadRSS(*feedURL)
	if err != nil {
		log.Fatalf("load RSS: %v", err)
	}

	loc, err := time.LoadLocation(*timezone)
	if err != nil {
		log.Printf("warn: could not load tz %q, using Local: %v", *timezone, err)
		loc = time.Local
	}

	// Image downloader with deduplication and per-host concurrency
	dl := newDownloader(*concurrency, *perHost)

	n := len(rss.Channel.Items)
	if *limitItems > 0 && *limitItems < n {
		n = *limitItems
	}

	for i := 0; i < n; i++ {
		item := rss.Channel.Items[i]
		if err := processItem(item, loc, dl); err != nil {
			log.Printf("error processing item %d: %v", i, err)
		}
	}

	dl.Wait()
}

func cleanOutput(contentOut string) error {
	if err := removeAndRecreate(contentOut); err != nil {
		return fmt.Errorf("reset out dir: %w", err)
	}
	return nil
}

func removeAndRecreate(p string) error {
	if err := os.RemoveAll(p); err != nil {
		return err
	}
	return os.MkdirAll(p, 0o755)
}

func loadRSS(src string) (*RSS, error) {
	var r io.ReadCloser
	var err error

	src = strings.TrimSpace(src)
	src = strings.TrimPrefix(src, "view-source:") // allow pasted view-source: URLs

	if fileExists(src) {
		r, err = os.Open(src)
		if err != nil {
			return nil, err
		}
	} else {
		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest("GET", src, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		r = resp.Body
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Try robust feed parsing with gofeed (handles many malformed feeds)
	fp := gofeed.NewParser()
	feed, err := fp.ParseString(string(data))
	if err != nil {
		// As a fallback, try sanitizing obvious issues and reparse
		safe := sanitizeXML(data)
		feed, err = fp.ParseString(string(safe))
		if err != nil {
			return nil, fmt.Errorf("failed to parse feed: %w", err)
		}
	}

	out := &RSS{Channel: Channel{Title: feed.Title}}
	for _, it := range feed.Items {
		pub := it.Published
		if pub == "" && it.PublishedParsed != nil {
			pub = it.PublishedParsed.Format(time.RFC1123Z)
		}
		creator := ""
		if it.Author != nil {
			creator = strings.TrimSpace(it.Author.Name)
		}
		// Prefer full HTML content; fall back to description
		html := it.Content
		if strings.TrimSpace(html) == "" {
			html = it.Description
		}

		// Categories: gofeed gives plain strings (domain attr from WP isn't preserved)
		cats := make([]Category, 0, len(it.Categories))
		for _, c := range it.Categories {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			cats = append(cats, Category{Value: c})
		}

		// Comments feed (best-effort via extensions)
		commentsURL := ""
		if extNS, ok := it.Extensions["wfw"]; ok {
			if nodes, ok := extNS["commentRss"]; ok && len(nodes) > 0 {
				commentsURL = strings.TrimSpace(nodes[0].Value)
			}
		}

		out.Channel.Items = append(out.Channel.Items, Item{
			Title:           it.Title,
			Link:            it.Link,
			PubDate:         pub,
			GUID:            it.GUID,
			Creator:         creator,
			Description:     it.Description,
			ContentEncoded:  html,
			Categories:      cats,
			CommentsFeedURL: commentsURL,
		})
	}
	return out, nil
}

func sanitizeXML(b []byte) []byte {
	s := string(b)
	s = removeInvalidXMLChars(s)
	s = encodeAmpersands(s)
	return []byte(s)
}

func encodeAmpersands(s string) string {
	var out strings.Builder
	out.Grow(len(s) + len(s)/100)
	for i := 0; i < len(s); {
		c := s[i]
		if c != '&' {
			out.WriteByte(c)
			i++
			continue
		}
		j := i + 1
		if j < len(s) {
			if s[j] == '#' {
				j++
				if j < len(s) && (s[j] == 'x' || s[j] == 'X') {
					j++
					k := j
					for k < len(s) && isHex(s[k]) {
						k++
					}
					if k < len(s) && k > j && s[k] == ';' {
						out.WriteString(s[i : k+1])
						i = k + 1
						continue
					}
				} else {
					k := j
					for k < len(s) && isDigit(s[k]) {
						k++
					}
					if k < len(s) && k > j && s[k] == ';' {
						out.WriteString(s[i : k+1])
						i = k + 1
						continue
					}
				}
			} else if isAlpha(s[j]) {
				k := j + 1
				for k < len(s) && isAlphaNum(s[k]) {
					k++
				}
				if k < len(s) && k > j && s[k] == ';' {
					out.WriteString(s[i : k+1])
					i = k + 1
					continue
				}
			}
		}
		// Not a valid entity → escape
		out.WriteString("&amp;")
		i++
	}
	return out.String()
}

func isAlpha(b byte) bool    { return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') }
func isDigit(b byte) bool    { return b >= '0' && b <= '9' }
func isAlphaNum(b byte) bool { return isAlpha(b) || isDigit(b) }
func isHex(b byte) bool      { return isDigit(b) || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f') }

func removeInvalidXMLChars(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if r == 0x9 || r == 0xA || r == 0xD ||
			(r >= 0x20 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) ||
			(r >= 0x10000 && r <= 0x10FFFF) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func processItem(item Item, loc *time.Location, dl *downloader) error {
	u, err := url.Parse(strings.TrimSpace(item.Link))
	if err != nil {
		return fmt.Errorf("parse link: %w", err)
	}
	aliasPath := ensureTrailingSlash(u.Path)
	year, month, slugTail := extractPathParts(u.Path)
	if year == "" || month == "" || slugTail == "" {
		// fallback to date + normalized title
		if *verbose {
			log.Printf("fallback slug logic for link=%s", item.Link)
		}
		year, month = pubDateYearMonth(item.PubDate, loc)
		slugTail = slugify(path.Base(strings.Trim(u.Path, "/")))
	} else {
		// sanitize slug from URL (remove emojis, spaces, etc.)
		slugTail = slugify(slugTail)
	}
	slug := fmt.Sprintf("%s-%s-%s", year, month, slugTail)

	contentHTML := strings.TrimSpace(item.ContentEncoded)
	if contentHTML == "" {
		contentHTML = strings.TrimSpace(item.Description)
	}

	processedHTML, err := rewriteAndDownloadImages(contentHTML, slug, dl)
	if err != nil {
		return fmt.Errorf("rewrite images: %w", err)
	}

	bodyMD, err := toMarkdownPreserveOrder(processedHTML, slug)
	if err != nil {
		return fmt.Errorf("html->md: %w", err)
	}

	postTime, err := parsePubDate(item.PubDate, loc)
	if err != nil {
		if *verbose {
			log.Printf("warn: pubDate parse failed, using now: %v", err)
		}
		postTime = time.Now().In(loc)
	}

	tags, cats := splitTagsAndCategories(item.Categories)
	aliases := []string{aliasPath}

	fm := FrontMatter{
		Title:      strings.TrimSpace(item.Title),
		Date:       postTime,
		Draft:      false,
		Tags:       tags,
		Aliases:    aliases,
		Categories: cats,
	}

	if err := writeMarkdownFile(slug, fm, bodyMD); err != nil {
		return err
	}

	if *verbose {
		log.Printf("✓ %s -> %s/index.md (%d chars)", item.Title, slug, len(bodyMD))
	}
	return nil
}

func writeMarkdownFile(slug string, fm FrontMatter, body string) error {
	data, err := yaml.Marshal(&fm)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(data)
	buf.WriteString("---\n")
	buf.WriteString(strings.TrimSpace(body))
	buf.WriteString("\n")

	outPath := filepath.Join(*outDir, slug, "index.md")
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, buf.Bytes(), 0o644)
}

func splitTagsAndCategories(cats []Category) (tags []string, categories []string) {
	mTags := map[string]struct{}{}
	mCats := map[string]struct{}{}
	for _, c := range cats {
		name := strings.TrimSpace(htmlUnescape(c.Value))
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "Allgemein") {
			continue
		}
		if strings.EqualFold(c.Domain, "post_tag") {
			mTags[name] = struct{}{}
		} else {
			mCats[name] = struct{}{}
		}
	}
	tags = setToSortedSlice(mTags)
	categories = setToSortedSlice(mCats)
	return
}

func setToSortedSlice(m map[string]struct{}) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// Convert HTML to Markdown, preserving paragraph order and text.
func toMarkdownPreserveOrder(html string, slug string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	conv := md.NewConverter("", false, nil)
	// Paragraphs → keep as paragraphs with blank line
	conv.AddRules(md.Rule{
		Filter: []string{"p"},
		Replacement: func(content string, selec *goquery.Selection, opt *md.Options) *string {
			content = strings.TrimSpace(content)
			if content == "" {
				return nil
			}
			return md.String(content + "\n\n")
		},
	})
	// Line breaks
	conv.AddRules(md.Rule{
		Filter: []string{"br"},
		Replacement: func(content string, selec *goquery.Selection, opt *md.Options) *string {
			return md.String("\n")
		},
	})
	// Images → emit with trailing blank line so adjacent images don't glue together
	conv.AddRules(md.Rule{
		Filter: []string{"img"},
		Replacement: func(content string, selec *goquery.Selection, opt *md.Options) *string {
			src, _ := selec.Attr("src")
			alt, _ := selec.Attr("alt")
			alt = strings.TrimSpace(alt)
			if alt == "" {
				alt = path.Base(src)
			}
			if src == "" {
				return nil
			}
			return md.String(fmt.Sprintf("![%s](%s)\n\n", alt, src))
		},
	})

	var b strings.Builder
	var roots *goquery.Selection
	if doc.Find("body").Length() > 0 {
		roots = doc.Find("body").Contents()
	} else {
		roots = doc.Selection.Contents()
	}

	roots.Each(func(i int, s *goquery.Selection) {
		// Skip pure-whitespace text nodes
		if goquery.NodeName(s) == "#text" {
			if strings.TrimSpace(s.Text()) == "" {
				return
			}
			// Emit text as a paragraph
			b.WriteString(strings.TrimSpace(s.Text()))
			b.WriteString("\n\n")
			return
		}

		// Special handling: Gutenberg gallery block → do not emit inline markup; handled by Hugo convention externally
		if s.Is(".wp-block-gallery, figure.wp-block-gallery") {
			return
		}
		// Special handling: Gutenberg video block or plain <video>
		if s.Is(".wp-block-video, figure.wp-block-video, video") {
			var vs *goquery.Selection
			if s.Is("video") {
				vs = s
			} else {
				vs = s.Find("video").First()
			}
			if vs.Length() > 0 {
				src, _ := vs.Attr("src")
				if strings.TrimSpace(src) == "" {
					if vv := vs.Find("source").First(); vv.Length() > 0 {
						src, _ = vv.Attr("src")
					}
				}
				if strings.TrimSpace(src) != "" {
					name := path.Base(src)
					// Output a plain Markdown link to the local video path
					b.WriteString(fmt.Sprintf("[Video: %s](%s)\n\n", name, src))
				}
			}
			return
		}
		// Default: convert this fragment as-is to preserve order
		h, err := goquery.OuterHtml(s)
		if err != nil {
			return
		}
		frag, err := conv.ConvertString(h)
		if err != nil {
			return
		}
		if strings.TrimSpace(frag) == "" {
			// Fallback: if conversion yields empty (e.g., container-only nodes), use visible text
			if txt := strings.TrimSpace(s.Text()); txt != "" {
				b.WriteString(txt)
				b.WriteString("\n\n")
			}
			return
		}
		b.WriteString(frag)
		// Ensure a trailing newline if the fragment didn't add one
		if !strings.HasSuffix(frag, "\n") {
			b.WriteString("\n")
		}
	})

	return strings.TrimSpace(b.String()), nil
}

func parsePubDate(p string, loc *time.Location) (time.Time, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return time.Time{}, errors.New("empty pubDate")
	}
	// Try common RSS formats
	formats := []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339}
	var t time.Time
	var err error
	for _, f := range formats {
		t, err = time.Parse(f, p)
		if err == nil {
			return t.In(loc), nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown date format: %q", p)
}

func pubDateYearMonth(p string, loc *time.Location) (string, string) {
	t, err := parsePubDate(p, loc)
	if err != nil {
		now := time.Now().In(loc)
		return fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", int(now.Month()))
	}
	return fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month()))
}

func extractPathParts(p string) (year, month, tail string) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) >= 4 {
		year = segs[0]
		month = segs[1]
		tail = segs[3]
		return
	}
	return "", "", ""
}

func ensureTrailingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

var slugRe = regexp.MustCompile(`[^a-z0-9\-]+`)

func replaceEmojisWithCode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isEmojiRune(r) {
			b.WriteString("u")
			b.WriteString(strings.ToUpper(fmt.Sprintf("%X", r)))
		} else if r == '\u200D' || r == '\uFE0F' { // ZWJ / variation selector – drop
			continue
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isEmojiRune(r rune) bool {
	// Common emoji blocks (not exhaustive, but good coverage)
	if (r >= 0x1F300 && r <= 0x1F5FF) || // Misc Symbols & Pictographs
		(r >= 0x1F600 && r <= 0x1F64F) || // Emoticons
		(r >= 0x1F680 && r <= 0x1F6FF) || // Transport & Map
		(r >= 0x1F700 && r <= 0x1F77F) || // Alchemical Symbols
		(r >= 0x1F900 && r <= 0x1F9FF) || // Supplemental Symbols & Pictographs
		(r >= 0x1FA70 && r <= 0x1FAFF) || // Symbols & Pictographs Extended-A
		(r >= 0x2600 && r <= 0x26FF) || // Misc Symbols
		(r >= 0x2700 && r <= 0x27BF) || // Dingbats
		(r >= 0x1F1E6 && r <= 0x1F1FF) { // Regional Indicator Symbols (flags)
		return true
	}
	return false
}

func slugify(s string) string {
	s = replaceEmojisWithCode(s)
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func htmlUnescape(s string) string {
	// Minimal replacement; XML decoder already unescapes most values
	return strings.ReplaceAll(s, "\u00a0", " ")
}

func rewriteAndDownloadImages(html string, slug string, dl *downloader) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	// Per-post image numbering (001_, 002_, ...), based on first mention order
	imageIndex := 1
	assigned := make(map[string]int) // original URL -> assigned index

	doc.Find("img").Each(func(i int, s *goquery.Selection) {
		// 1) Emojis aus s.w.org / wp-smiley direkt als Unicode einsetzen
		cls, _ := s.Attr("class")
		src, _ := s.Attr("src")
		if strings.Contains(cls, "wp-smiley") || strings.Contains(src, "/s.w.org/images/core/emoji/") {
			if alt, ok := s.Attr("alt"); ok && strings.TrimSpace(alt) != "" {
				_ = s.ReplaceWithHtml(alt) // Emoji als Text
			} else {
				_ = s.ReplaceWithHtml("") // sicherheitshalber entfernen
			}
			return
		}

		srcset, _ := s.Attr("srcset")
		best := pickBestSrc(src, srcset)
		if best == "" {
			return
		}

		// 2) Auf Originaldatei ohne -WxH / -scaled verweisen
		origURL := toOriginalURL(best)

		base := filepath.Join(*outDir, slug, "gallery")
		relBase := "gallery"
		_ = os.MkdirAll(base, 0o755)

		// Assign stable, per-post index for this original URL based on first mention
		num, ok := assigned[origURL]
		if !ok {
			num = imageIndex
			assigned[origURL] = num
			imageIndex++
		}
		prefix := fmt.Sprintf("%03d_", num)

		filename := prefix + filenameFromURL(origURL)
		dest := filepath.Join(base, filename)
		rel := path.Join(relBase, filename)

		// 3) Download und Umschreiben der Attribute (src, evtl. a[href])
		dl.Schedule(origURL, dest)

		s.RemoveAttr("srcset")
		s.RemoveAttr("sizes")
		s.SetAttr("src", rel)

		// Falls das Bild von einem Link umschlossen ist, den Link ebenfalls lokal machen
		if a := s.ParentsFiltered("a").First(); a.Length() > 0 {
			a.SetAttr("href", rel)
		}
	})
	// Store HTML5 videos in the page bundle gallery and rewrite src to a relative path.
	doc.Find("video").Each(func(i int, v *goquery.Selection) {
		src, _ := v.Attr("src")
		// Some WP videos use <source src> children instead of video@src
		if strings.TrimSpace(src) == "" {
			if vv := v.Find("source").First(); vv.Length() > 0 {
				src, _ = vv.Attr("src")
			}
		}
		if strings.TrimSpace(src) == "" {
			return
		}

		base := filepath.Join(*outDir, slug, "gallery")
		relBase := "gallery"
		_ = os.MkdirAll(base, 0o755)

		filename := filenameFromURL(src)
		dest := filepath.Join(base, filename)
		rel := path.Join(relBase, filename)

		// schedule download of the original video URL (no WP size suffix stripping for videos)
		dl.Schedule(src, dest)

		// rewrite video@src and any <source src> children to the local relative path
		v.SetAttr("src", rel)
		v.Find("source").Each(func(_ int, s *goquery.Selection) {
			s.SetAttr("src", rel)
		})
	})
	// Serialize modified HTML back to string (inner contents)
	var outParts []string
	root := doc.Selection
	// Prefer body contents if a body exists
	if doc.Find("body").Length() > 0 {
		doc.Find("body").Contents().Each(func(i int, s *goquery.Selection) {
			h, err := goquery.OuterHtml(s)
			if err == nil {
				outParts = append(outParts, h)
			}
		})
	} else {
		root.Contents().Each(func(i int, s *goquery.Selection) {
			h, err := goquery.OuterHtml(s)
			if err == nil {
				outParts = append(outParts, h)
			}
		})
	}
	return strings.TrimSpace(strings.Join(outParts, "")), nil
}

var srcsetRe = regexp.MustCompile(`,?\s*([^\s,]+)\s+(\d+)w`)
var wpSizeSuffixRe = regexp.MustCompile(`-(?:\d+)x(?:\d+)(?:-[0-9]+)?$`)
var wpScaledSuffixRe = regexp.MustCompile(`-scaled(?:-[0-9]+)?$`)

func toOriginalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	base := path.Base(u.Path)
	dir := path.Dir(u.Path)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	name = stripWPSuffixes(name)
	u.Path = path.Join(dir, name+ext)
	return u.String()
}

func stripWPSuffixes(name string) string {
	name = wpSizeSuffixRe.ReplaceAllString(name, "")
	name = wpScaledSuffixRe.ReplaceAllString(name, "")
	return name
}

func pickBestSrc(src string, srcset string) string {
	src = strings.TrimSpace(src)
	srcset = strings.TrimSpace(srcset)
	best := src
	maxW := -1
	for _, m := range srcsetRe.FindAllStringSubmatch(srcset, -1) {
		u := m[1]
		wStr := m[2]
		var w int
		fmt.Sscanf(wStr, "%d", &w)
		if w > maxW {
			maxW = w
			best = u
		}
	}
	return best
}

func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return path.Base(raw)
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" {
		name = "image"
	}
	return name
}

// Downloader implements deduplicated concurrent downloads

type downloader struct {
	wg      sync.WaitGroup
	sem     chan struct{}
	seen    sync.Map // url -> struct{}
	hostSem map[string]chan struct{}
	mu      sync.Mutex
	perHost int
}

func newDownloader(concurrency int, perhost int) *downloader {
	if concurrency < 1 {
		concurrency = 1
	}
	if perhost < 1 {
		perhost = 1
	}
	return &downloader{
		sem:     make(chan struct{}, concurrency),
		hostSem: make(map[string]chan struct{}),
		perHost: perhost,
	}
}

func (d *downloader) getHostSem(host string) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.hostSem[host]; ok {
		return ch
	}
	ch := make(chan struct{}, d.perHost)
	d.hostSem[host] = ch
	return ch
}

func (d *downloader) Schedule(rawURL string, dest string) {
	if _, exists := d.seen.LoadOrStore(rawURL, struct{}{}); exists {
		return
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	var hsem chan struct{}
	if host != "" {
		hsem = d.getHostSem(host)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.sem <- struct{}{}
		defer func() { <-d.sem }()
		if hsem != nil {
			hsem <- struct{}{}
			defer func() { <-hsem }()
		}
		if err := downloadFile(rawURL, dest); err != nil {
			log.Printf("download failed %s -> %s: %v", rawURL, dest, err)
		} else if *verbose {
			log.Printf("downloaded %s", dest)
		}
	}()
}

func (d *downloader) Wait() { d.wg.Wait() }

func downloadFile(rawURL, dest string) error {
	// Skip if file already exists and is non-empty
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return nil
	}

	attempts := *retries
	if attempts < 1 {
		attempts = 1
	}
	t := time.Duration(*timeoutSec) * time.Second
	if t < 10*time.Second {
		t = 10 * time.Second
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: *perHost,
			MaxConnsPerHost:     *perHost,
		}
		client := &http.Client{Timeout: t, Transport: transport}

		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "wordpress2hugo/1.0 (+https://example.com)")

		resp, err := client.Do(req)
		if err != nil {
			if attempt == attempts {
				return err
			}
			// backoff with jitter
			time.Sleep(time.Duration(attempt*2)*time.Second + time.Duration(rand.Intn(500))*time.Millisecond)
			continue
		}

		var copyErr error
		func() {
			defer resp.Body.Close()
			if resp.StatusCode >= 500 {
				copyErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				return
			}
			if resp.StatusCode >= 400 {
				// client errors → don’t retry
				copyErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				attempt = attempts
				return
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				copyErr = err
				return
			}
			f, err := os.Create(dest)
			if err != nil {
				copyErr = err
				return
			}
			defer func() {
				f.Close()
				if copyErr != nil {
					_ = os.Remove(dest)
				}
			}()
			if _, err = io.Copy(f, resp.Body); err != nil {
				copyErr = err
				return
			}
		}()

		if copyErr == nil {
			return nil
		}
		if attempt == attempts {
			return copyErr
		}
		time.Sleep(time.Duration(attempt*2)*time.Second + time.Duration(rand.Intn(500))*time.Millisecond)
	}
	return fmt.Errorf("unreachable")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
