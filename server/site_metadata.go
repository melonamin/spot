package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	siteMetadataFileName = "_spot.json"
	maxSiteTitleLength   = 80
	maxSiteDescLength    = 240
	maxSiteTagCount      = 8
	maxSiteTagLength     = 32
	maxTagPromptFiles    = 80
	maxTagPromptPathLen  = 120
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
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

var (
	siteTagRe        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	titleTagRe       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaDescRe       = regexp.MustCompile(`(?is)<meta\s+[^>]*(?:name|property)\s*=\s*["'](?:description|og:description)["'][^>]*content\s*=\s*(?:"([^"]*)"|'([^']*)')[^>]*>`)
	metaDescReAlt    = regexp.MustCompile(`(?is)<meta\s+[^>]*content\s*=\s*(?:"([^"]*)"|'([^']*)')[^>]*(?:name|property)\s*=\s*["'](?:description|og:description)["'][^>]*>`)
	headingRe        = regexp.MustCompile(`(?is)<h[1-2][^>]*>(.*?)</h[1-2]>`)
	stripTagsRe      = regexp.MustCompile(`(?is)<[^>]+>`)
	jsonObjectBounds = regexp.MustCompile(`(?s)\{.*\}`)
	tagHyphenRunRe   = regexp.MustCompile(`-+`)
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
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&fields); err != nil {
		return deploySiteMetadata{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return deploySiteMetadata{}, errors.New("metadata must contain a single JSON object")
		}
		return deploySiteMetadata{}, err
	}
	if fields == nil {
		return deploySiteMetadata{}, errors.New("metadata must be a JSON object")
	}
	tagsSpecified := false
	for key, value := range fields {
		switch key {
		case "title":
			if err := json.Unmarshal(value, &raw.Title); err != nil {
				return deploySiteMetadata{}, fmt.Errorf("title must be a string: %w", err)
			}
		case "description":
			if err := json.Unmarshal(value, &raw.Description); err != nil {
				return deploySiteMetadata{}, fmt.Errorf("description must be a string: %w", err)
			}
		case "tags":
			tagsSpecified = true
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				raw.Tags = nil
				continue
			}
			if err := json.Unmarshal(value, &raw.Tags); err != nil {
				return deploySiteMetadata{}, fmt.Errorf("tags must be an array: %w", err)
			}
		default:
			return deploySiteMetadata{}, fmt.Errorf("unknown field %q", key)
		}
	}
	out := deploySiteMetadata{
		SiteMetadata: SiteMetadata{
			Title:       cleanText(raw.Title, maxSiteTitleLength),
			Description: cleanText(raw.Description, maxSiteDescLength),
		},
		TagsSpecified: tagsSpecified,
	}
	if tagsSpecified {
		tags, err := normalizeSiteTags(raw.Tags)
		if err != nil {
			return deploySiteMetadata{}, err
		}
		out.Tags = tags
	}
	return out, nil
}

func normalizeSiteTags(tags []string) ([]string, error) {
	return collectSiteTags(tags, false)
}

// collectSiteTags normalizes, dedupes, and caps a tag list. When lenient it
// skips entries that fail validation; otherwise the first bad entry is an
// error. The strict mode guards writes; the lenient mode keeps reads robust so
// one malformed stored tag cannot wipe an otherwise valid list.
func collectSiteTags(tags []string, lenient bool) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, min(len(tags), maxSiteTagCount))
	for _, tag := range tags {
		normalized := normalizeSiteTag(tag)
		if normalized == "" {
			continue
		}
		if len(normalized) > maxSiteTagLength || !siteTagRe.MatchString(normalized) {
			if lenient {
				continue
			}
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
	tag = tagHyphenRunRe.ReplaceAllString(tag, "-")
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
			for _, value := range match[1:] {
				if value != "" {
					return html.UnescapeString(stripTagsRe.ReplaceAllString(value, ""))
				}
			}
			return ""
		}
	}
	return ""
}

func cleanText(text string, maxLen int) string {
	text = strings.Join(strings.Fields(html.UnescapeString(text)), " ")
	// Drop C0/C1 control codes and bidi overrides that survive whitespace
	// folding: they are invisible in the gallery yet can carry through to
	// the stored value and spoof or corrupt the rendered title/description.
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, text)
	if len([]rune(text)) <= maxLen {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:maxLen]))
}

// resolveSiteMetadata builds the metadata to persist synchronously during a
// deploy. AI tag suggestion is deliberately excluded — it runs in the
// background after the deploy responds (scheduleAutoTag) so the network round
// trip never blocks the response or the per-site lock. When a deploy declares
// no tags the site's existing tags are kept, so an unrelated content update
// neither drops them nor re-rolls AI-suggested ones.
func resolveSiteMetadata(meta deploySiteMetadata, existingTags []string) SiteMetadata {
	tags := meta.Tags
	if !meta.TagsSpecified {
		tags = existingTags
	}
	return SiteMetadata{
		Title:       cleanText(meta.Title, maxSiteTitleLength),
		Description: cleanText(meta.Description, maxSiteDescLength),
		Tags:        cloneSiteTags(tags),
	}
}

// shouldAutoTag reports whether a just-deployed site is eligible for background
// AI tagging: it must be public, have declared no tags and have none stored, and
// the AI proxy must be configured.
func (s *Server) shouldAutoTag(meta deploySiteMetadata, existingTags []string, restricted bool) bool {
	return !restricted && !meta.TagsSpecified && len(existingTags) == 0 &&
		s.ai != nil && s.ai.configured()
}

// scheduleAutoTag suggests gallery tags for a freshly deployed public site off
// the request path. The AI call runs without holding the per-site lock; the
// worker then takes the lock, re-reads the row, and only writes tags if the
// site still has none, so a concurrent deploy that sets tags or metadata wins.
func (s *Server) scheduleAutoTag(site string, files []deployFile, base SiteMetadata) {
	updater, ok := s.deployAuth.(siteMetadataUpdater)
	if !ok {
		return
	}
	reader, _ := s.deployAuth.(siteMetadataReader)
	s.bgTasks.Add(1)
	go func() {
		defer s.bgTasks.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tags := s.suggestSiteTags(ctx, site, files, base)
		if len(tags) == 0 {
			return
		}
		lock := s.siteMutationLock(site)
		lock.Lock()
		defer lock.Unlock()
		current := base
		if reader != nil {
			meta, err := reader.SiteMetadata(ctx, site)
			if err != nil {
				log.Printf("auto-tag %s: read metadata: %v", site, err)
				return
			}
			current = meta
		}
		if len(current.Tags) > 0 {
			return
		}
		current.Tags = tags
		if err := updater.UpdateSiteMetadata(ctx, site, current); err != nil {
			log.Printf("auto-tag %s: update metadata: %v", site, err)
		}
	}()
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
		fileNames = append(fileNames, tagPromptPath(f.path))
		if f.path == "index.html" {
			index = string(f.data)
		}
	}
	sort.Strings(fileNames)
	fileSummary := strings.Join(fileNames, ", ")
	if len(fileNames) > maxTagPromptFiles {
		fileSummary = strings.Join(fileNames[:maxTagPromptFiles], ", ")
		fileSummary += fmt.Sprintf(", ... and %d more", len(fileNames)-maxTagPromptFiles)
	}
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
		site, meta.Title, meta.Description, strings.Join(headings, ", "), fileSummary)
}

func tagPromptPath(path string) string {
	runes := []rune(path)
	if len(runes) <= maxTagPromptPathLen {
		return path
	}
	return string(runes[:maxTagPromptPathLen]) + "..."
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
	valid, _ := collectSiteTags(tags, true)
	return valid
}

func cloneSiteTags(tags []string) []string {
	if len(tags) == 0 {
		return []string{}
	}
	return append([]string(nil), tags...)
}
