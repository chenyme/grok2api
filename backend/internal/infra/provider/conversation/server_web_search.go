package conversation

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
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

func anthropicServerToolUseID(raw string) string {
	if strings.HasPrefix(raw, "srvtoolu_") {
		return raw
	}
	if raw == "" {
		sum := sha1.Sum([]byte(fmt.Sprintf("ws-%d", len(raw))))
		return "srvtoolu_" + hex.EncodeToString(sum[:8])
	}
	// Build ids are long; keep stable prefix for multi-block pairing.
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, raw)
	if len(cleaned) > 48 {
		cleaned = cleaned[len(cleaned)-48:]
	}
	return "srvtoolu_" + cleaned
}

func parseWebSearchCallItem(item responseItem) (webSearchCall, bool) {
	if item.Type != "web_search_call" {
		return webSearchCall{}, false
	}
	call := webSearchCall{ID: anthropicServerToolUseID(item.ID)}
	action := item.Action
	if action == nil {
		call.Failed = true
		call.Code = "unavailable"
		return call, true
	}
	if q, _ := action["query"].(string); strings.TrimSpace(q) != "" {
		call.Query = strings.TrimSpace(q)
	}
	// Prefer action.sources[].url
	if rawSources, ok := action["sources"].([]any); ok {
		for _, raw := range rawSources {
			source, _ := raw.(map[string]any)
			if source == nil {
				continue
			}
			link, _ := source["url"].(string)
			link = strings.TrimSpace(link)
			if link == "" {
				continue
			}
			title, _ := source["title"].(string)
			title = strings.TrimSpace(title)
			if title == "" {
				title = titleFromURL(link)
			}
			call.Hits = append(call.Hits, webSearchHit{Title: title, URL: link})
		}
	}
	// Fallback: message annotations often have better titles; filled later by merge.
	if item.Status != "" && item.Status != "completed" && len(call.Hits) == 0 {
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
		link = strings.TrimSpace(link)
		title = strings.TrimSpace(title)
		if link == "" || title == "" {
			continue
		}
		// Skip numeric-only titles like "1","2" from Build
		if len(title) <= 2 {
			allDigit := true
			for _, r := range title {
				if r < '0' || r > '9' {
					allDigit = false
					break
				}
			}
			if allDigit {
				continue
			}
		}
		titles[link] = title
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
	if len(calls) <= 1 {
		return calls
	}
	order := make([]string, 0, len(calls))
	best := make(map[string]webSearchCall, len(calls))
	for _, call := range calls {
		id := call.ID
		if id == "" {
			order = append(order, fmt.Sprintf("__anon_%d", len(order)))
			best[order[len(order)-1]] = call
			continue
		}
		prev, exists := best[id]
		if !exists {
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
				seen := make(map[string]struct{}, len(call.Hits)+len(prev.Hits))
				merged := make([]webSearchHit, 0, len(call.Hits)+len(prev.Hits))
				for _, hit := range call.Hits {
					if _, ok := seen[hit.URL]; ok {
						continue
					}
					seen[hit.URL] = struct{}{}
					merged = append(merged, hit)
				}
				for _, hit := range prev.Hits {
					if _, ok := seen[hit.URL]; ok {
						continue
					}
					seen[hit.URL] = struct{}{}
					merged = append(merged, hit)
				}
				call.Hits = merged
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
	return len(calls)
}

func queryJSONPartial(query string) string {
	// Stable single-shot partial JSON for stream input_json_delta (CC regex-parses "query").
	raw, _ := json.Marshal(map[string]string{"query": query})
	return string(raw)
}
