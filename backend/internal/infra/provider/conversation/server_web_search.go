package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

// webSearchHit is the minimal Claude Code WebSearchTool success payload.
type webSearchHit struct {
	Title string
	URL   string
}

// webSearchCall is one Build web_search_call mapped for Anthropic Messages.
type webSearchCall struct {
	ID     string
	Query  string
	Hits   []webSearchHit
	Failed bool
	Code   string
}

const maxWebSearchCalls = 32

func unavailableWebSearchCall(query string) webSearchCall {
	fallback := map[string]string{"type": "web_search", "query": query, "error": "unavailable"}
	return webSearchCall{
		ID: anthropicServerToolUseID("", fallback), Query: query,
		Failed: true, Code: "unavailable",
	}
}

func anthropicServerToolUseID(raw string, fallback any) string {
	original := raw
	raw = strings.TrimPrefix(raw, "srvtoolu_")
	if raw == "" {
		encoded := []byte(original)
		if original == "" {
			encoded, _ = json.Marshal(fallback)
		}
		sum := sha256.Sum256(encoded)
		return "srvtoolu_" + hex.EncodeToString(sum[:16])
	}
	// Build ids are long; keep stable prefix for multi-block pairing.
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, raw)
	if len(cleaned) > 48 || cleaned != raw {
		sum := sha256.Sum256([]byte(original))
		if len(cleaned) > 31 {
			cleaned = cleaned[:31]
		}
		cleaned += "_" + hex.EncodeToString(sum[:8])
	}
	return "srvtoolu_" + cleaned
}

func parseWebSearchCallItem(item responseItem) (webSearchCall, bool) {
	if item.Type != "web_search_call" {
		return webSearchCall{}, false
	}
	action := item.Action
	call := webSearchCall{ID: anthropicServerToolUseID(item.ID, action)}
	if action == nil {
		call.Failed = true
		call.Code = "unavailable"
		return call, true
	}
	if q, _ := action["query"].(string); strings.TrimSpace(q) != "" {
		call.Query = truncateRunes(strings.TrimSpace(q), 4096)
	}
	// Prefer action.sources[].url
	if rawSources, ok := action["sources"].([]any); ok {
		seen := make(map[string]struct{}, len(rawSources))
		for _, raw := range rawSources {
			if len(call.Hits) >= searchresult.MaxResults {
				break
			}
			source, _ := raw.(map[string]any)
			if source == nil {
				continue
			}
			link, _ := source["url"].(string)
			link, valid := searchresult.NormalizeURL(link)
			if !valid {
				continue
			}
			if _, exists := seen[link]; exists {
				continue
			}
			seen[link] = struct{}{}
			title, _ := source["title"].(string)
			title = searchresult.NormalizeTitle(title, titleFromURL(link))
			call.Hits = append(call.Hits, webSearchHit{Title: title, URL: link})
		}
	}
	// Fallback: message annotations often have better titles; filled later by merge.
	status := strings.ToLower(strings.TrimSpace(item.Status))
	if (status == "failed" || status == "incomplete") && len(call.Hits) == 0 {
		call.Failed = true
		call.Code = "unavailable"
	}
	return call, true
}

func titleFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Host
}

func mergeAnnotationTitles(calls []webSearchCall, annotations []map[string]any) []webSearchCall {
	if len(annotations) == 0 || len(calls) == 0 {
		return calls
	}
	titles := make(map[string]string)
	for _, ann := range annotations {
		if strings.TrimSpace(fmt.Sprint(ann["type"])) != "url_citation" {
			// nested url_citation object
			if nested, ok := ann["url_citation"].(map[string]any); ok {
				ann = nested
			} else {
				continue
			}
		}
		link, _ := ann["url"].(string)
		title, _ := ann["title"].(string)
		link, valid := searchresult.NormalizeURL(link)
		title = strings.TrimSpace(title)
		if !valid || title == "" {
			continue
		}
		if unhelpfulCitationTitle(title) {
			continue
		}
		titles[link] = searchresult.NormalizeTitle(title, link)
	}
	for i := range calls {
		for j := range calls[i].Hits {
			if better, ok := titles[calls[i].Hits[j].URL]; ok {
				calls[i].Hits[j].Title = better
			}
		}
	}
	return calls
}

func unhelpfulCitationTitle(title string) bool {
	value := strings.ToLower(strings.TrimSpace(title))
	if value == "" {
		return true
	}
	isDigits := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		for _, r := range candidate {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if isDigits(value) {
		return true
	}
	for _, prefix := range []string{"source", "citation"} {
		if strings.HasPrefix(value, prefix) && isDigits(strings.Trim(strings.TrimPrefix(value, prefix), " #:-")) {
			return true
		}
	}
	return false
}

func extractMessageAnnotations(item responseItem) []map[string]any {
	if item.Type != "message" {
		return nil
	}
	var out []map[string]any
	for _, content := range item.Content {
		for _, ann := range content.Annotations {
			if m, ok := ann.(map[string]any); ok {
				out = append(out, m)
			} else {
				// re-marshal generic
				raw, _ := json.Marshal(ann)
				var m map[string]any
				if json.Unmarshal(raw, &m) == nil {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// dedupeWebSearchCalls keeps one entry per call id, preferring the payload with
// more hits / a non-empty query. Build sometimes repeats web_search_call items.
func dedupeWebSearchCalls(calls []webSearchCall) []webSearchCall {
	if len(calls) == 0 {
		return nil
	}
	order := make([]string, 0, len(calls))
	best := make(map[string]webSearchCall, len(calls))
	for _, call := range calls {
		call.Hits = boundedWebSearchHits(call.Hits)
		if !call.Failed && strings.TrimSpace(call.Query) == "" && len(call.Hits) == 0 {
			continue
		}
		id := call.ID
		if id == "" {
			if len(order) >= maxWebSearchCalls {
				continue
			}
			order = append(order, fmt.Sprintf("__anon_%d", len(order)))
			best[order[len(order)-1]] = call
			continue
		}
		prev, exists := best[id]
		if !exists {
			if len(order) >= maxWebSearchCalls {
				continue
			}
			order = append(order, id)
			best[id] = call
			continue
		}
		// Prefer richer payload.
		score := func(c webSearchCall) int {
			n := len(c.Hits) * 10
			if strings.TrimSpace(c.Query) != "" {
				n += 5
			}
			if !c.Failed {
				n += 1
			}
			return n
		}
		if score(call) >= score(prev) {
			// Keep non-empty query from either side.
			if call.Query == "" {
				call.Query = prev.Query
			}
			// Union hits by URL if both have sources.
			if len(prev.Hits) > 0 && len(call.Hits) > 0 {
				call.Hits = boundedWebSearchHits(call.Hits, prev.Hits)
			} else if len(call.Hits) == 0 {
				call.Hits = prev.Hits
			}
			best[id] = call
		} else if prev.Query == "" && call.Query != "" {
			prev.Query = call.Query
			best[id] = prev
		}
	}
	out := make([]webSearchCall, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	return out
}

func boundedWebSearchHits(groups ...[]webSearchHit) []webSearchHit {
	seen := make(map[string]struct{}, searchresult.MaxResults)
	bounded := make([]webSearchHit, 0, searchresult.MaxResults)
	for _, hits := range groups {
		for _, hit := range hits {
			if len(bounded) >= searchresult.MaxResults {
				return bounded
			}
			if _, exists := seen[hit.URL]; exists {
				continue
			}
			seen[hit.URL] = struct{}{}
			bounded = append(bounded, hit)
		}
	}
	return bounded
}

func appendServerWebSearchContent(content []any, calls []webSearchCall) []any {
	calls = dedupeWebSearchCalls(calls)
	for _, call := range calls {
		content = append(content, map[string]any{
			"type":  "server_tool_use",
			"id":    call.ID,
			"name":  "web_search",
			"input": map[string]any{"query": call.Query},
		})
		if call.Failed {
			code := call.Code
			if code == "" {
				code = "unavailable"
			}
			content = append(content, map[string]any{
				"type":        "web_search_tool_result",
				"tool_use_id": call.ID,
				"content": map[string]any{
					"type":       "web_search_tool_result_error",
					"error_code": code,
				},
			})
			continue
		}
		hits := make([]any, 0, len(call.Hits))
		for _, hit := range call.Hits {
			hits = append(hits, map[string]any{
				"type":  "web_search_result",
				"title": hit.Title,
				"url":   hit.URL,
			})
		}
		content = append(content, map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": call.ID,
			"content":     hits,
		})
	}
	return content
}

func webSearchRequestCount(calls []webSearchCall) int {
	return len(dedupeWebSearchCalls(calls))
}

func queryJSONPartial(query string) string {
	// Stable single-shot partial JSON for stream input_json_delta (CC regex-parses "query").
	raw, _ := json.Marshal(map[string]string{"query": query})
	return string(raw)
}
