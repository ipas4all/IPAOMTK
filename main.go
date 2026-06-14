package main

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

const defaultAPI = "https://ipaomtk.com/wp-json/wp/v2/posts"
const sourceIconURL = "https://ipaomtk.com/wp-content/uploads/2026/04/cropped-ipaomtk-icon.png"

type config struct {
	apiURL       string
	output       string
	perPage      int
	concurrency  int
	maxPages     int
	bundlePrefix string
	pretty       bool
	inspectIPA   bool
	inspectConc  int
	inspectMax   int
	timeout      time.Duration
	retries      int
}

type wpPost struct {
	ID            int          `json:"id"`
	DateGMT       string       `json:"date_gmt"`
	ModifiedGMT   string       `json:"modified_gmt"`
	Slug          string       `json:"slug"`
	Link          string       `json:"link"`
	Title         renderedText `json:"title"`
	Excerpt       renderedText `json:"excerpt"`
	Content       renderedText `json:"content"`
	Downloads     []download   `json:"downloads"`
	YoastHeadJSON yoastHead    `json:"yoast_head_json"`
	Embedded      embedded     `json:"_embedded"`
}

type renderedText struct {
	Rendered string `json:"rendered"`
}

type download struct {
	Name    string `json:"download_name"`
	Version string `json:"download_version"`
	ModInfo string `json:"download_mod_info"`
	Size    string `json:"download_size"`
	URL     string `json:"download_url"`
}

type yoastHead struct {
	Title       string      `json:"title"`
	Description string      `json:"description"`
	OGImage     []yoastImg  `json:"og_image"`
	Schema      yoastSchema `json:"schema"`
}

type yoastImg struct {
	URL string `json:"url"`
}

type yoastSchema struct {
	Graph []schemaNode `json:"@graph"`
}

type schemaNode struct {
	Type           any      `json:"@type"`
	ArticleSection []string `json:"articleSection"`
	ThumbnailURL   string   `json:"thumbnailUrl"`
	DatePublished  string   `json:"datePublished"`
}

type embedded struct {
	Terms [][]term  `json:"wp:term"`
	Media []wpMedia `json:"wp:featuredmedia"`
}

type term struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type wpMedia struct {
	SourceURL string `json:"source_url"`
}

type sourceJSON struct {
	FeaturedApps []string `json:"featuredApps,omitempty"`
	Description  string   `json:"description"`
	Apps         []app    `json:"apps"`
	IconURL      string   `json:"iconURL"`
	Identifier   string   `json:"identifier"`
	Website      string   `json:"website"`
	HeaderURL    string   `json:"headerURL"`
	Name         string   `json:"name"`
	Subtitle     string   `json:"subtitle"`
	TintColor    string   `json:"tintColor"`
}

type app struct {
	BundleIdentifier     string       `json:"bundleIdentifier"`
	LocalizedDescription string       `json:"localizedDescription"`
	Beta                 bool         `json:"beta,omitempty"`
	TintColor            string       `json:"tintColor,omitempty"`
	Category             string       `json:"category,omitempty"`
	IconURL              string       `json:"iconURL,omitempty"`
	DeveloperName        string       `json:"developerName,omitempty"`
	DownloadURL          string       `json:"downloadURL"`
	Versions             []appVersion `json:"versions"`
	Subtitle             string       `json:"subtitle,omitempty"`
	Name                 string       `json:"name"`
}

type appVersion struct {
	LocalizedDescription string `json:"localizedDescription,omitempty"`
	Size                 int64  `json:"size,omitempty"`
	Date                 string `json:"date,omitempty"`
	Version              string `json:"version"`
	DownloadURL          string `json:"downloadURL"`
}

type stats struct {
	posts         int
	apps          int
	skippedNoDL   int
	skippedBadURL int
}

type inspectStats struct {
	attempted int
	found     int
	failed    int
	duplicate int
	bytes     int64
}

func main() {
	cfg := parseFlags()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}
	totalPages, err := discoverTotalPages(ctx, client, cfg)
	if err != nil {
		exitf("discover pages: %v", err)
	}
	if cfg.maxPages > 0 && cfg.maxPages < totalPages {
		totalPages = cfg.maxPages
	}

	posts, err := fetchAllPosts(ctx, client, cfg, totalPages)
	if err != nil {
		exitf("fetch posts: %v", err)
	}

	src, st := buildSource(posts, cfg)
	var ist inspectStats
	if cfg.inspectIPA {
		ist = inspectBundleIDs(ctx, client, cfg, src.Apps)
	}
	if len(src.Apps) > 0 {
		src.FeaturedApps = []string{src.Apps[0].BundleIdentifier}
	}

	if err := writeJSON(cfg, src); err != nil {
		exitf("write JSON: %v", err)
	}

	fmt.Fprintf(os.Stderr, "pages=%d posts=%d apps=%d skipped_no_download=%d skipped_bad_url=%d output=%s\n",
		totalPages, st.posts, st.apps, st.skippedNoDL, st.skippedBadURL, cfg.output)
	if cfg.inspectIPA {
		fmt.Fprintf(os.Stderr, "ipa_inspect_attempted=%d ipa_inspect_found=%d ipa_inspect_failed=%d ipa_inspect_duplicate=%d ipa_inspect_bytes=%d\n",
			ist.attempted, ist.found, ist.failed, ist.duplicate, ist.bytes)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.apiURL, "api", defaultAPI, "WordPress posts API endpoint")
	flag.StringVar(&cfg.output, "out", "ipaomtk-source.json", "output JSON file")
	flag.IntVar(&cfg.perPage, "per-page", 100, "posts per page")
	flag.IntVar(&cfg.concurrency, "concurrency", 16, "concurrent page requests")
	flag.IntVar(&cfg.maxPages, "max-pages", 0, "only fetch the first N pages; 0 means all pages")
	flag.StringVar(&cfg.bundlePrefix, "bundle-prefix", "com.ipaomtk", "fallback bundle identifier prefix")
	flag.BoolVar(&cfg.pretty, "pretty", true, "pretty-print JSON")
	flag.BoolVar(&cfg.inspectIPA, "inspect-ipa", false, "range-read IPA Info.plist files to discover real bundle identifiers")
	flag.IntVar(&cfg.inspectConc, "inspect-concurrency", 8, "concurrent IPA bundle identifier inspections")
	flag.IntVar(&cfg.inspectMax, "inspect-max-apps", 0, "only inspect the first N apps; 0 means all apps")
	flag.DurationVar(&cfg.timeout, "timeout", 10*time.Minute, "overall timeout")
	flag.IntVar(&cfg.retries, "retries", 3, "HTTP retries per page")
	flag.Parse()

	if cfg.perPage < 1 {
		cfg.perPage = 1
	}
	if cfg.perPage > 100 {
		cfg.perPage = 100
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}
	if cfg.retries < 1 {
		cfg.retries = 1
	}
	if cfg.inspectConc < 1 {
		cfg.inspectConc = 1
	}
	cfg.bundlePrefix = strings.Trim(cfg.bundlePrefix, ".")
	return cfg
}

func discoverTotalPages(ctx context.Context, client *http.Client, cfg config) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, pageURL(cfg, 1), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ipaomtk-source-builder/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return discoverTotalPagesByGET(ctx, client, cfg)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return discoverTotalPagesByGET(ctx, client, cfg)
	}
	return totalPagesFromHeader(resp.Header, cfg)
}

func discoverTotalPagesByGET(ctx context.Context, client *http.Client, cfg config) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL(cfg, 1), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ipaomtk-source-builder/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("GET discovery %s", resp.Status)
	}
	return totalPagesFromHeader(resp.Header, cfg)
}

func totalPagesFromHeader(header http.Header, cfg config) (int, error) {
	if totalPages := header.Get("X-WP-TotalPages"); totalPages != "" {
		n, err := strconv.Atoi(totalPages)
		if err == nil && n > 0 {
			return n, nil
		}
	}
	if total := header.Get("X-WP-Total"); total != "" {
		n, err := strconv.Atoi(total)
		if err == nil && n > 0 {
			return int(math.Ceil(float64(n) / float64(cfg.perPage))), nil
		}
	}
	return 0, errors.New("missing X-WP-TotalPages/X-WP-Total headers")
}

func fetchAllPosts(ctx context.Context, client *http.Client, cfg config, totalPages int) ([]wpPost, error) {
	type pageResult struct {
		page  int
		posts []wpPost
		err   error
	}

	jobs := make(chan int)
	results := make(chan pageResult, totalPages)
	var wg sync.WaitGroup

	workers := cfg.concurrency
	if workers > totalPages {
		workers = totalPages
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for page := range jobs {
				posts, err := fetchPage(ctx, client, cfg, page)
				results <- pageResult{page: page, posts: posts, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for page := 1; page <= totalPages; page++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- page:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	byPage := make(map[int][]wpPost, totalPages)
	for res := range results {
		if res.err != nil {
			return nil, fmt.Errorf("page %d: %w", res.page, res.err)
		}
		byPage[res.page] = res.posts
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var out []wpPost
	for page := 1; page <= totalPages; page++ {
		out = append(out, byPage[page]...)
	}
	return out, nil
}

func fetchPage(ctx context.Context, client *http.Client, cfg config, page int) ([]wpPost, error) {
	var lastErr error
	for attempt := 1; attempt <= cfg.retries; attempt++ {
		posts, err := fetchPageOnce(ctx, client, cfg, page)
		if err == nil {
			return posts, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func fetchPageOnce(ctx context.Context, client *http.Client, cfg config, page int) ([]wpPost, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL(cfg, page), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ipaomtk-source-builder/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var posts []wpPost
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func pageURL(cfg config, page int) string {
	u, err := url.Parse(cfg.apiURL)
	if err != nil {
		return cfg.apiURL
	}
	q := u.Query()
	q.Set("per_page", strconv.Itoa(cfg.perPage))
	q.Set("page", strconv.Itoa(page))
	q.Set("_embed", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

func buildSource(posts []wpPost, cfg config) (sourceJSON, stats) {
	src := sourceJSON{
		Description: "IPAOMTK without the bullshit",
		Apps:        make([]app, 0, len(posts)),
		IconURL:     sourceIconURL,
		Identifier:  strings.Trim(cfg.bundlePrefix+".source", "."),
		Website:     "https://ipaomtk.com",
		HeaderURL:   sourceIconURL,
		Name:        "IPAOMTK Source",
		Subtitle:    "Download iOS IPA Files & Tweaked Apps",
		TintColor:   "#D03E43",
	}
	var st stats
	usedBundleIDs := make(map[string]int)

	for _, post := range posts {
		st.posts++
		if len(post.Downloads) == 0 {
			st.skippedNoDL++
			continue
		}

		dl := post.Downloads[0]
		if strings.TrimSpace(dl.URL) == "" {
			st.skippedBadURL++
			continue
		}

		name := cleanAppName(firstNonEmpty(post.Title.Rendered, post.YoastHeadJSON.Title, post.Slug))
		desc := cleanDescription(firstNonEmpty(post.YoastHeadJSON.Description, post.Excerpt.Rendered, post.Content.Rendered))
		categories := postCategories(post)
		category := altStoreCategory(categories)
		subtitle := appSubtitle(dl.ModInfo, categories, desc)
		date := postDate(post)
		size := parseSizeBytes(dl.Size)
		iconURL := appIcon(post)
		version := strings.TrimSpace(dl.Version)
		if version == "" {
			version = "1.0"
		}

		id := bundleID(cfg.bundlePrefix, post.Slug)
		if usedBundleIDs[id] > 0 {
			id = fmt.Sprintf("%s.%d", id, post.ID)
		}
		usedBundleIDs[id]++

		src.Apps = append(src.Apps, app{
			BundleIdentifier:     id,
			LocalizedDescription: desc,
			TintColor:            src.TintColor,
			Category:             category,
			IconURL:              iconURL,
			DeveloperName:        "IPAOMTK",
			DownloadURL:          dl.URL,
			Versions: []appVersion{
				{
					LocalizedDescription: versionDescription(dl.ModInfo, desc),
					Size:                 size,
					Date:                 date,
					Version:              version,
					DownloadURL:          dl.URL,
				},
			},
			Subtitle: subtitle,
			Name:     name,
		})
		st.apps++
	}
	return src, st
}

type bundleResult struct {
	index  int
	bundle string
	bytes  int64
	err    error
}

func inspectBundleIDs(ctx context.Context, client *http.Client, cfg config, apps []app) inspectStats {
	limit := len(apps)
	if cfg.inspectMax > 0 && cfg.inspectMax < limit {
		limit = cfg.inspectMax
	}
	var st inspectStats
	if limit == 0 {
		return st
	}

	jobs := make(chan int)
	results := make(chan bundleResult, limit)
	var wg sync.WaitGroup

	workers := cfg.inspectConc
	if workers > limit {
		workers = limit
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				bundle, bytesRead, err := inspectOneBundleID(ctx, client, apps[index].DownloadURL)
				results <- bundleResult{index: index, bundle: bundle, bytes: bytesRead, err: err}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := 0; i < limit; i++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- i:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	bundles := make(map[int]string, limit)
	for res := range results {
		st.attempted++
		st.bytes += res.bytes
		if res.err != nil || res.bundle == "" {
			st.failed++
			continue
		}
		bundles[res.index] = res.bundle
	}

	used := make(map[string]bool, len(apps))
	for i := range apps {
		candidate := bundles[i]
		if candidate != "" && !used[candidate] {
			apps[i].BundleIdentifier = candidate
			used[candidate] = true
			st.found++
			continue
		}
		if candidate != "" {
			st.duplicate++
		}

		fallback := apps[i].BundleIdentifier
		if used[fallback] {
			fallback = fmt.Sprintf("%s.%d", fallback, i+1)
		}
		apps[i].BundleIdentifier = fallback
		used[fallback] = true
	}

	return st
}

func inspectOneBundleID(ctx context.Context, client *http.Client, rawURL string) (string, int64, error) {
	size, err := remoteContentLength(ctx, client, rawURL)
	if err != nil {
		return "", 0, err
	}
	if size < 22 {
		return "", 0, errors.New("archive too small")
	}

	var bytesRead int64
	tailSize := int64(65536)
	if size < tailSize {
		tailSize = size
	}
	tailStart := size - tailSize
	tail, err := rangeFetch(ctx, client, rawURL, tailStart, size-1)
	bytesRead += int64(len(tail))
	if err != nil {
		return "", bytesRead, err
	}

	cdOffset, cdSize, err := parseEOCD(tail, tailStart)
	if err != nil {
		return "", bytesRead, err
	}
	if cdOffset < 0 || cdSize <= 0 || cdOffset+cdSize > size {
		return "", bytesRead, errors.New("invalid central directory bounds")
	}

	var central []byte
	if cdOffset >= tailStart && cdOffset+cdSize <= size {
		start := cdOffset - tailStart
		central = tail[start : start+cdSize]
	} else {
		central, err = rangeFetch(ctx, client, rawURL, cdOffset, cdOffset+cdSize-1)
		bytesRead += int64(len(central))
		if err != nil {
			return "", bytesRead, err
		}
	}

	entry, err := findInfoPlistEntry(central)
	if err != nil {
		return "", bytesRead, err
	}
	plistBytes, n, err := fetchZipEntry(ctx, client, rawURL, entry)
	bytesRead += n
	if err != nil {
		return "", bytesRead, err
	}

	var info map[string]any
	if _, err := plist.Unmarshal(plistBytes, &info); err != nil {
		return "", bytesRead, err
	}
	bundle, _ := info["CFBundleIdentifier"].(string)
	bundle = strings.TrimSpace(bundle)
	if bundle == "" {
		return "", bytesRead, errors.New("missing CFBundleIdentifier")
	}
	return bundle, bytesRead, nil
}

type zipEntry struct {
	name     string
	method   uint16
	compSize int64
	localOff int64
}

func remoteContentLength(ctx context.Context, client *http.Client, rawURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "ipaomtk-source-builder/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HEAD %s", resp.Status)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes") {
		return 0, errors.New("server does not advertise byte ranges")
	}
	if resp.ContentLength <= 0 {
		return 0, errors.New("missing content length")
	}
	return resp.ContentLength, nil
}

func rangeFetch(ctx context.Context, client *http.Client, rawURL string, start, end int64) ([]byte, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid byte range %d-%d", start, end)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "ipaomtk-source-builder/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("range request returned %s", resp.Status)
	}

	want := end - start + 1
	body, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != want {
		return nil, fmt.Errorf("range length mismatch: got %d want %d", len(body), want)
	}
	return body, nil
}

func parseEOCD(tail []byte, tailStart int64) (int64, int64, error) {
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(tail[i+20:]))
		if i+22+commentLen != len(tail) {
			continue
		}
		disk := binary.LittleEndian.Uint16(tail[i+4:])
		cdDisk := binary.LittleEndian.Uint16(tail[i+6:])
		if disk != 0 || cdDisk != 0 {
			return 0, 0, errors.New("multi-disk zip is unsupported")
		}
		cdSize32 := binary.LittleEndian.Uint32(tail[i+12:])
		cdOffset32 := binary.LittleEndian.Uint32(tail[i+16:])
		if cdSize32 == math.MaxUint32 || cdOffset32 == math.MaxUint32 {
			return 0, 0, errors.New("zip64 central directory is unsupported")
		}
		return int64(cdOffset32), int64(cdSize32), nil
	}
	return 0, 0, errors.New("zip end-of-central-directory not found")
}

func findInfoPlistEntry(central []byte) (zipEntry, error) {
	var best zipEntry
	for off := 0; off+46 <= len(central); {
		if binary.LittleEndian.Uint32(central[off:]) != 0x02014b50 {
			return zipEntry{}, errors.New("invalid central directory header")
		}
		method := binary.LittleEndian.Uint16(central[off+10:])
		compSize32 := binary.LittleEndian.Uint32(central[off+20:])
		localOff32 := binary.LittleEndian.Uint32(central[off+42:])
		nameLen := int(binary.LittleEndian.Uint16(central[off+28:]))
		extraLen := int(binary.LittleEndian.Uint16(central[off+30:]))
		commentLen := int(binary.LittleEndian.Uint16(central[off+32:]))
		entryLen := 46 + nameLen + extraLen + commentLen
		if off+entryLen > len(central) {
			return zipEntry{}, errors.New("truncated central directory entry")
		}

		name := string(central[off+46 : off+46+nameLen])
		if isMainInfoPlist(name) {
			if compSize32 == math.MaxUint32 || localOff32 == math.MaxUint32 {
				return zipEntry{}, errors.New("zip64 Info.plist entry is unsupported")
			}
			best = zipEntry{
				name:     name,
				method:   method,
				compSize: int64(compSize32),
				localOff: int64(localOff32),
			}
			break
		}
		off += entryLen
	}
	if best.name == "" {
		return zipEntry{}, errors.New("Payload/*.app/Info.plist not found")
	}
	return best, nil
}

func isMainInfoPlist(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 3 &&
		parts[0] == "Payload" &&
		strings.HasSuffix(parts[1], ".app") &&
		parts[2] == "Info.plist"
}

func fetchZipEntry(ctx context.Context, client *http.Client, rawURL string, entry zipEntry) ([]byte, int64, error) {
	if entry.compSize <= 0 || entry.compSize > 2*1024*1024 {
		return nil, 0, fmt.Errorf("suspicious Info.plist compressed size %d", entry.compSize)
	}

	local, err := rangeFetch(ctx, client, rawURL, entry.localOff, entry.localOff+29)
	bytesRead := int64(len(local))
	if err != nil {
		return nil, bytesRead, err
	}
	if binary.LittleEndian.Uint32(local) != 0x04034b50 {
		return nil, bytesRead, errors.New("invalid local file header")
	}
	nameLen := int64(binary.LittleEndian.Uint16(local[26:]))
	extraLen := int64(binary.LittleEndian.Uint16(local[28:]))
	dataStart := entry.localOff + 30 + nameLen + extraLen
	compData, err := rangeFetch(ctx, client, rawURL, dataStart, dataStart+entry.compSize-1)
	bytesRead += int64(len(compData))
	if err != nil {
		return nil, bytesRead, err
	}

	switch entry.method {
	case 0:
		return compData, bytesRead, nil
	case 8:
		r := flate.NewReader(bytes.NewReader(compData))
		defer r.Close()
		out, err := io.ReadAll(io.LimitReader(r, 4*1024*1024))
		if err != nil {
			return nil, bytesRead, err
		}
		return out, bytesRead, nil
	default:
		return nil, bytesRead, fmt.Errorf("unsupported zip compression method %d", entry.method)
	}
}

func writeJSON(cfg config, src sourceJSON) error {
	var data []byte
	var err error
	if cfg.pretty {
		data, err = json.MarshalIndent(src, "", "  ")
	} else {
		data, err = json.Marshal(src)
	}
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(cfg.output, data, 0o644)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func cleanAppName(raw string) string {
	name := cleanText(raw)
	name = strings.TrimSuffix(name, " For iOS")
	lower := strings.ToLower(name)
	for _, marker := range []string{" ipa v", " mod ipa v", " ipa "} {
		if idx := strings.LastIndex(lower, marker); idx > 0 {
			name = strings.TrimSpace(name[:idx])
			break
		}
	}
	name = strings.Trim(name, " -|")
	if name == "" {
		return "Untitled"
	}
	return name
}

func cleanDescription(raw string) string {
	text := cleanText(raw)
	if text == "" {
		return "No description available."
	}
	return text
}

var tagRE = regexp.MustCompile(`<[^>]+>`)
var wsRE = regexp.MustCompile(`\s+`)

func cleanText(raw string) string {
	raw = html.UnescapeString(raw)
	raw = tagRE.ReplaceAllString(raw, " ")
	raw = strings.ReplaceAll(raw, "\u00a0", " ")
	raw = wsRE.ReplaceAllString(raw, " ")
	return strings.TrimSpace(raw)
}

func postCategories(post wpPost) []string {
	seen := make(map[string]bool)
	var cats []string
	for _, group := range post.Embedded.Terms {
		for _, t := range group {
			name := cleanText(t.Name)
			if name == "" || seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			cats = append(cats, name)
		}
	}
	for _, node := range post.YoastHeadJSON.Schema.Graph {
		if !isArticle(node.Type) {
			continue
		}
		for _, section := range node.ArticleSection {
			name := cleanText(section)
			if name == "" || seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			cats = append(cats, name)
		}
	}
	return cats
}

func isArticle(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.EqualFold(v, "Article")
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.EqualFold(s, "Article") {
				return true
			}
		}
	}
	return false
}

func altStoreCategory(categories []string) string {
	for _, cat := range categories {
		c := strings.ToLower(cat)
		switch {
		case strings.Contains(c, "game"), strings.Contains(c, "action"), strings.Contains(c, "adventure"),
			strings.Contains(c, "arcade"), strings.Contains(c, "casual"), strings.Contains(c, "racing"),
			strings.Contains(c, "role"), strings.Contains(c, "simulation"), strings.Contains(c, "sports"),
			strings.Contains(c, "strategy"), strings.Contains(c, "trivia"):
			return "games"
		case strings.Contains(c, "developer"), strings.Contains(c, "trollstore"):
			return "developer"
		case strings.Contains(c, "utilit"):
			return "utilities"
		case strings.Contains(c, "photo"), strings.Contains(c, "video"), strings.Contains(c, "movie"),
			strings.Contains(c, "entertain"), strings.Contains(c, "graphics"), strings.Contains(c, "design"):
			return "entertainment"
		}
	}
	for _, cat := range categories {
		c := strings.ToLower(cat)
		if c != "ipa apps" {
			return slugify(c)
		}
	}
	return "utilities"
}

func appSubtitle(modInfo string, categories []string, desc string) string {
	modInfo = cleanText(modInfo)
	if modInfo != "" {
		return modInfo
	}
	for _, cat := range categories {
		c := strings.ToLower(cat)
		if c != "ipa apps" && c != "games" {
			return cleanText(cat)
		}
	}
	if idx := strings.Index(desc, "."); idx > 20 && idx < 80 {
		return strings.TrimSpace(desc[:idx])
	}
	if len(desc) > 80 {
		return strings.TrimSpace(desc[:77]) + "..."
	}
	return desc
}

func versionDescription(modInfo, desc string) string {
	modInfo = cleanText(modInfo)
	if modInfo != "" {
		return modInfo
	}
	return desc
}

func postDate(post wpPost) string {
	for _, raw := range []string{post.YoastArticleDate(), post.DateGMT, post.ModifiedGMT} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if t, err := time.Parse("2006-01-02T15:04:05", raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func (post wpPost) YoastArticleDate() string {
	for _, node := range post.YoastHeadJSON.Schema.Graph {
		if isArticle(node.Type) && node.DatePublished != "" {
			return node.DatePublished
		}
	}
	return ""
}

func parseSizeBytes(raw string) int64 {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) == 0 {
		return 0
	}
	num, err := strconv.ParseFloat(strings.ReplaceAll(parts[0], ",", ""), 64)
	if err != nil {
		return 0
	}
	unit := "B"
	if len(parts) > 1 {
		unit = strings.ToUpper(strings.TrimSuffix(parts[1], "s"))
	}
	mult := float64(1)
	switch unit {
	case "KB", "KIB":
		mult = 1024
	case "MB", "MIB":
		mult = 1024 * 1024
	case "GB", "GIB":
		mult = 1024 * 1024 * 1024
	}
	return int64(math.Round(num * mult))
}

func appIcon(post wpPost) string {
	if len(post.Embedded.Media) > 0 && post.Embedded.Media[0].SourceURL != "" {
		return post.Embedded.Media[0].SourceURL
	}
	if len(post.YoastHeadJSON.OGImage) > 0 && post.YoastHeadJSON.OGImage[0].URL != "" {
		return post.YoastHeadJSON.OGImage[0].URL
	}
	for _, node := range post.YoastHeadJSON.Schema.Graph {
		if node.ThumbnailURL != "" {
			return node.ThumbnailURL
		}
	}
	return sourceIconURL
}

func bundleID(prefix, slug string) string {
	slug = strings.Trim(slugify(slug), ".")
	if slug == "" {
		slug = "app"
	}
	return strings.Trim(prefix+"."+slug, ".")
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(value string) string {
	value = strings.ToLower(html.UnescapeString(value))
	value = slugRE.ReplaceAllString(value, ".")
	value = strings.Trim(value, ".")
	value = strings.ReplaceAll(value, "..", ".")
	return value
}

func exitf(format string, args ...any) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, format, args...)
	fmt.Fprintln(os.Stderr, buf.String())
	os.Exit(1)
}
