package main

import "testing"

func TestHeadingLevel(t *testing.T) {
	cases := map[int]int{2: 0, 3: 1, 4: 2, 11: 9, 12: 0, 1: 0}
	for blockType, want := range cases {
		if got := headingLevel(blockType); got != want {
			t.Errorf("headingLevel(%d)=%d, want %d", blockType, got, want)
		}
	}
}

func TestExtractElementsText(t *testing.T) {
	// 模拟一个 heading block：{"heading1":{"elements":[{"text_run":{"content":"需求一"}}]}}
	block := map[string]any{
		"heading1": map[string]any{
			"elements": []any{
				map[string]any{"text_run": map[string]any{"content": "需求"}},
				map[string]any{"text_run": map[string]any{"content": "一"}},
			},
		},
	}
	if got := extractElementsText(block); got != "需求一" {
		t.Errorf("extractElementsText=%q, want 需求一", got)
	}

	// 无文本块返回空串
	if got := extractElementsText(map[string]any{"divider": map[string]any{}}); got != "" {
		t.Errorf("空块应返回空串, got %q", got)
	}
}

func TestFlattenBlocks(t *testing.T) {
	blocks := []rawBlock{
		{BlockType: 3, extra: map[string]any{"heading1": map[string]any{"elements": []any{
			map[string]any{"text_run": map[string]any{"content": "REQ-1 标题"}}}}}},
		{BlockType: 2, extra: map[string]any{"text": map[string]any{"elements": []any{
			map[string]any{"text_run": map[string]any{"content": "正文一段"}}}}}},
		{BlockType: 22, extra: map[string]any{"divider": map[string]any{}}}, // 无文本，应跳过
	}
	flat := flattenBlocks(blocks)
	if len(flat) != 2 {
		t.Fatalf("应保留 2 个有文本的块, got %d", len(flat))
	}
	if flat[0].Level != 1 || flat[0].Text != "REQ-1 标题" {
		t.Errorf("标题块解析错误: %+v", flat[0])
	}
	if flat[1].Level != 0 || flat[1].Text != "正文一段" {
		t.Errorf("正文块解析错误: %+v", flat[1])
	}
}

func TestPlanUpsert(t *testing.T) {
	existing := []Record{
		{RecordID: "rec_a", Fields: map[string]any{"external_key": "REQ-1#issue-1"}},
		// 文本字段也可能是富文本数组形式
		{RecordID: "rec_b", Fields: map[string]any{"external_key": []any{
			map[string]any{"text": "REQ-2#issue-1", "type": "text"}}}},
	}
	incoming := []map[string]any{
		{"external_key": "REQ-1#issue-1", "status": "resolved"}, // 命中 rec_a → update
		{"external_key": "REQ-2#issue-1", "status": "open"},     // 命中 rec_b（富文本）→ update
		{"external_key": "REQ-3#issue-1", "status": "open"},     // 新 → create
	}
	creates, updates := planUpsert(existing, incoming, "external_key")
	if len(creates) != 1 || len(updates) != 2 {
		t.Fatalf("creates=%d updates=%d, want 1 / 2", len(creates), len(updates))
	}
	if creates[0]["external_key"] != "REQ-3#issue-1" {
		t.Errorf("新建记录 key 错误: %v", creates[0]["external_key"])
	}
	ids := map[string]bool{updates[0].RecordID: true, updates[1].RecordID: true}
	if !ids["rec_a"] || !ids["rec_b"] {
		t.Errorf("更新应命中 rec_a 和 rec_b, got %+v", updates)
	}
}

func TestColLetter(t *testing.T) {
	cases := map[int]string{1: "A", 26: "Z", 27: "AA", 52: "AZ", 53: "BA", 0: "A"}
	for n, want := range cases {
		if got := colLetter(n); got != want {
			t.Errorf("colLetter(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestExtractText(t *testing.T) {
	if got := extractText("plain"); got != "plain" {
		t.Errorf("string: got %q", got)
	}
	rich := []any{map[string]any{"text": "AB", "type": "text"}, map[string]any{"text": "CD"}}
	if got := extractText(rich); got != "ABCD" {
		t.Errorf("rich text: got %q", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
}
