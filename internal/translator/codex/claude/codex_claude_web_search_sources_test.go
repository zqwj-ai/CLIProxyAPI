package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_WebSearchSourcesInclude(t *testing.T) {
	for _, toolType := range []string{"web_search_20250305", "web_search_20260209", "custom", ""} {
		t.Run(toolType, func(t *testing.T) {
			request := `{"messages":[{"role":"user","content":"search"}]}`
			if toolType != "" {
				request = `{"tools":[{"type":"` + toolType + `","name":"search","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"search"}]}`
			}
			out := ConvertClaudeRequestToCodex("gpt-5.4", []byte(request), true)
			include := gjson.GetBytes(out, "include").Array()
			wantSearch := strings.HasPrefix(toolType, "web_search_")
			foundSearch, foundReasoning := false, false
			for _, field := range include {
				foundSearch = foundSearch || field.String() == "web_search_call.action.sources"
				foundReasoning = foundReasoning || field.String() == "reasoning.encrypted_content"
			}
			if foundSearch != wantSearch || !foundReasoning {
				t.Fatalf("include = %s, want search sources=%v and encrypted reasoning", gjson.GetBytes(out, "include"), wantSearch)
			}
		})
	}
}

func TestCodexWebSearchResultContent_Sources(t *testing.T) {
	for _, tc := range []struct {
		name, root, item string
		wantCount        int
	}{
		{"action sources", `{}`, `{"action":{"sources":[{"type":"url","url":"https://docs.python.org/3/","title":"Python docs"}]}}`, 1},
		{"root action sources", `{"action":{"sources":[{"type":"url","url":"https://docs.python.org/3/","title":"Python docs"}]}}`, `{}`, 1},
		{"legacy results", `{}`, `{"results":[{"url":"https://docs.python.org/3/","title":"Python docs"}]}`, 1},
		{"root results", `{"results":[{"url":"https://docs.python.org/3/","title":"Python docs"}]}`, `{}`, 1},
		{"empty results with sources", `{}`, `{"results":[],"action":{"sources":[{"url":"https://docs.python.org/3/","title":"Python docs"}]}}`, 1},
		{"merged and deduplicated", `{}`, `{"results":[{"url":"https://docs.python.org/3/","title":"Python docs"}],"action":{"sources":[{"url":"https://docs.python.org/3/"},{"url":"https://www.python.org/"}]}}`, 2},
		{"skip missing URLs", `{}`, `{"action":{"sources":[{"title":"no URL"},{"url":"  "}]}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := codexWebSearchResultContent(gjson.Parse(tc.root), gjson.Parse(tc.item))
			results := gjson.ParseBytes(out).Array()
			if len(results) != tc.wantCount {
				t.Fatalf("got %s, want %d structured sources", out, tc.wantCount)
			}
			if tc.wantCount > 0 && (results[0].Get("url").String() != "https://docs.python.org/3/" || results[0].Get("title").String() != "Python docs" || results[0].Get("type").String() != "web_search_result") {
				t.Fatalf("incorrect structured source: %s", out)
			}
			if tc.wantCount == 2 && results[1].Get("title").String() != "https://www.python.org/" {
				t.Fatalf("missing URL title fallback: %s", out)
			}
		})
	}
}

func TestConvertCodexResponseToClaude_WebSearchSources(t *testing.T) {
	item := `{"type":"web_search_call","id":"ws_sources","status":"completed","action":{"type":"search","query":"Python official documentation","sources":[{"type":"url","url":"https://docs.python.org/3/","title":"Python documentation"},{"type":"url","url":"https://www.python.org/"}]}}`
	request := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Python official documentation"}]}`)
	assertContent := func(t *testing.T, block gjson.Result) {
		t.Helper()
		if block.Get("tool_use_id").String() != "ws_sources" || block.Get("content.#").Int() != 2 {
			t.Fatalf("missing structured search sources: %s", block)
		}
		if block.Get("content.0.url").String() != "https://docs.python.org/3/" || block.Get("content.0.title").String() != "Python documentation" || block.Get("content.0.type").String() != "web_search_result" {
			t.Fatalf("incorrect source fields: %s", block)
		}
		if block.Get("content.1.url").String() != "https://www.python.org/" || block.Get("content.1.title").String() != "https://www.python.org/" {
			t.Fatalf("missing untitled source: %s", block)
		}
	}
	t.Run("non-stream", func(t *testing.T) {
		out := ConvertCodexResponseToClaudeNonStream(context.Background(), "", request, nil, []byte(`{"type":"response.completed","response":{"id":"resp_sources","output":[`+item+`]}}`), nil)
		assertContent(t, gjson.GetBytes(out, "content.1"))
	})
	t.Run("stream", func(t *testing.T) {
		var param any
		var outputs [][]byte
		for _, chunk := range []string{
			`data: {"type":"response.created","response":{"id":"resp_sources","model":"gpt-5.4"}}`,
			`data: {"type":"response.web_search_call.completed","item_id":"ws_sources"}`,
			`data: {"type":"response.output_item.done","item":` + item + `}`,
			`data: {"type":"response.completed","response":{"output":[` + item + `]}}`,
		} {
			outputs = append(outputs, ConvertCodexResponseToClaude(context.Background(), "", request, nil, []byte(chunk), &param)...)
		}
		count := 0
		for _, line := range strings.Split(string(bytes.Join(outputs, nil)), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			block := gjson.Get(strings.TrimPrefix(line, "data: "), "content_block")
			if block.Get("type").String() == "web_search_tool_result" {
				assertContent(t, block)
				count++
			}
		}
		if count != 1 {
			t.Fatalf("got %d search result blocks, want 1", count)
		}
	})
}
