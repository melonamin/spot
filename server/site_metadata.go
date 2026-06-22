package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	siteMetadataFileName = "_spot.json"
	maxSiteTitleLength   = 80
	maxSiteDescLength    = 240
	maxSiteTagCount      = 8
	maxSiteTagLength     = 32
)

type SiteMetadata struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type deploySiteMetadata struct {
	SiteMetadata
	TagsSpecified bool
}

type siteMetadataFile struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Tags        *[]string `json:"tags"`
}

var (
	siteTagRe        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	titleTagRe       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaDescRe       = regexp.MustCompile(`(?is)<meta\s+[^>]*(?:name|property)\s*=\s*["'](?:description|og:description)["'][^>]*content\s*=\s*["']([^"']*)["'][^>]*>`)
	metaDescReAlt    = regexp.MustCompile(`(?is)<meta\s+[^>]*content\s*=\s*["']([^"']*)["'][^>]*(?:name|property)\s*=\s*["'](?:description|og:description)["'][^>]*>`)
	headingRe        = regexp.MustCompile(`(?is)<h[1-2][^>]*>(.*?)</h[1-2]>`)
	stripTagsRe      = regexp.MustCompile(`(?is)<[^>]+>`)
	jsonObjectBounds = regexp.MustCompile(`(?s)\{.*\}`)
)

func metadataForDeploy(site string, files []deployFile) (deploySiteMetadata, error) {
	var out deploySiteMetadata
	for _, f := range files {
		if f.path == siteMetadataFileName {
			meta, err := parseSiteMetadataFile(f.data)
			if err != nil {
				return deploySiteMetadata{}, fmt.Errorf("invalid %s: %w", siteMetadataFileName, err)
			}
			out = meta
			break
		}
	}
	for _, f := range files {
		if f.path != "index.html" {
			continue
		}
		fromHTML := metadataFromIndexHTML(f.data)
		if out.Title == "" {
			out.Title = fromHTML.Title
		}
		if out.Description == "" {
			out.Description = fromHTML.Description
		}
		break
	}
	if out.Title == "" {
		out.Title = site
	}
	return out, nil
}

func parseSiteMetadataFile(data []byte) (deploySiteMetadata, error) {
	var raw siteMetadataFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return deploySiteMetadata{}, err
	}
	out := deploySiteMetadata{
		SiteMetadata: SiteMetadata{
			Title:       cleanText(raw.Title, maxSiteTitleLength),
			Description: cleanText(raw.Description, maxSiteDescLength),
		},
		TagsSpecified: raw.Tags != nil,
	}
	if raw.Tags != nil {
		tags, err := normalizeSiteTags(*raw.Tags)
		if err != nil {
			return deploySiteMetadata{}, err
		}
		out.Tags = tags
	}
	return out, nil
}

func normalizeSiteTags(tags []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, min(len(tags), maxSiteTagCount))
	for _, tag := range tags {
		normalized := normalizeSiteTag(tag)
		if normalized == "" {
			continue
		}
		if len(normalized) > maxSiteTagLength || !siteTagRe.MatchString(normalized) {
			return nil, fmt.Errorf("tag %q must be 1-%d lowercase letters, digits, or hyphens", tag, maxSiteTagLength)
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
		if len(out) == maxSiteTagCount {
			break
		}
	}
	return out, nil
}

func normalizeSiteTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	tag = strings.ReplaceAll(tag, "_", "-")
	tag = strings.Join(strings.Fields(tag), "-")
	tag = regexp.MustCompile(`-+`).ReplaceAllString(tag, "-")
	return strings.Trim(tag, "-")
}

func metadataFromIndexHTML(data []byte) SiteMetadata {
	body := string(data)
	return SiteMetadata{
		Title:       cleanText(firstHTMLMatch(body, titleTagRe), maxSiteTitleLength),
		Description: cleanText(firstHTMLMatch(body, metaDescRe, metaDescReAlt), maxSiteDescLength),
	}
}

func firstHTMLMatch(body string, res ...*regexp.Regexp) string {
	for _, re := range res {
		match := re.FindStringSubmatch(body)
		if len(match) > 1 {
			return html.UnescapeString(stripTagsRe.ReplaceAllString(match[1], ""))
		}
	}
	return ""
}

func cleanText(text string, maxLen int) string {
	text = strings.Join(strings.Fields(html.UnescapeString(text)), " ")
	if len([]rune(text)) <= maxLen {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:maxLen]))
}

func deployRestrictsAccess(site string, files []deployFile) bool {
	for _, f := range files {
		if f.path != accessFileName {
			continue
		}
		policy, err := parseAccessPolicy(site, f.data)
		return err != nil || (policy != nil && policy.RestrictsAccess())
	}
	return false
}

func (s *Server) completeSiteMetadata(ctx context.Context, site string, files []deployFile, meta deploySiteMetadata, restricted bool) SiteMetadata {
	out := meta.SiteMetadata
	if !meta.TagsSpecified && len(out.Tags) == 0 && !restricted {
		if tags := s.suggestSiteTags(ctx, site, files, out); len(tags) > 0 {
			out.Tags = tags
		}
	}
	return SiteMetadata{
		Title:       cleanText(out.Title, maxSiteTitleLength),
		Description: cleanText(out.Description, maxSiteDescLength),
		Tags:        cloneSiteTags(out.Tags),
	}
}

func (s *Server) suggestSiteTags(ctx context.Context, site string, files []deployFile, meta SiteMetadata) []string {
	if s.ai == nil || !s.ai.configured() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	prompt := siteTagPrompt(site, files, meta)
	res, err := s.ai.generateChat(ctx, aiChatRequest{
		System:    "You tag small static websites for an internal gallery. Return only compact JSON.",
		Messages:  []aiChatMessage{{Role: "user", Content: prompt}},
		MaxTokens: 300,
	})
	if err != nil {
		log.Printf("site metadata: AI tag suggestion for %s failed: %v", site, err)
		return nil
	}
	var raw struct {
		Tags []string `json:"tags"`
	}
	text := strings.TrimSpace(res.Text)
	if !strings.HasPrefix(text, "{") {
		if match := jsonObjectBounds.FindString(text); match != "" {
			text = match
		}
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		log.Printf("site metadata: AI tag suggestion for %s returned invalid JSON", site)
		return nil
	}
	tags, err := normalizeSiteTags(raw.Tags)
	if err != nil {
		log.Printf("site metadata: AI tag suggestion for %s returned invalid tags: %v", site, err)
		return nil
	}
	return tags
}

func siteTagPrompt(site string, files []deployFile, meta SiteMetadata) string {
	var index string
	var fileNames []string
	for _, f := range files {
		fileNames = append(fileNames, f.path)
		if f.path == "index.html" {
			index = string(f.data)
		}
	}
	sort.Strings(fileNames)
	var headings []string
	for _, m := range headingRe.FindAllStringSubmatch(index, 4) {
		if len(m) > 1 {
			headings = append(headings, cleanText(stripTagsRe.ReplaceAllString(m[1], ""), 80))
		}
	}
	return fmt.Sprintf(`Site name: %s
Title: %s
Description: %s
Headings: %s
Files: %s

Suggest 3 to 5 gallery tags. Rules: tags must be generic, lowercase, short, useful for filtering, and use hyphens for spaces. Avoid names, emails, and private-looking terms. Return exactly: {"tags":["tag-one","tag-two"]}`,
		site, meta.Title, meta.Description, strings.Join(headings, ", "), strings.Join(fileNames, ", "))
}

func encodeSiteTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	data, err := json.Marshal(cloneSiteTags(tags))
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeSiteTags(raw string) []string {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	valid, err := normalizeSiteTags(tags)
	if err != nil {
		return []string{}
	}
	return valid
}

func cloneSiteTags(tags []string) []string {
	if len(tags) == 0 {
		return []string{}
	}
	return append([]string(nil), tags...)
}
