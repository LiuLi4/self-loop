// loop-bridge：self-loop 的飞书（Feishu / Lark）Open API 桥接 CLI。
//
// 只做"机械"读写，不含任何需求解析业务逻辑（需求切分交给编排层的 intake agent
// 做语义处理）。子命令全部 stdout 输出 JSON、stderr 输出错误：
//
//	loop-bridge doc-dump    --doc <document_id> | --wiki <wiki_node_token>   # 拉取在线 docx 文档全部 block 文本
//	    # --wiki：先把知识库节点 token 解析成底层 docx，再拉取（仅支持 obj_type=docx）
//	loop-bridge sheet-dump  --sheet <spreadsheet_token>                  # 读电子表格全部分表的单元格值
//	loop-bridge resolve-wiki --node <wiki_node_token>                    # 仅解析 wiki 节点 → {obj_token, obj_type}
//	loop-bridge ensure-board --app <app_token> [--table <table_id>]      # 幂等建好 issue 看板的 9 个字段
//	loop-bridge issues-list --app <app_token> --table <table_id>         # 列 Bitable 看板全部 issue
//	loop-bridge issue-upsert --app <app_token> --table <table_id> [--key-field external_key] < records.json
//	    # 按 key-field 幂等 upsert（有则 update，无则 create），records.json 形如 {"records":[{"fields":{...}}]}
//
// 凭据纪律：
//   - app_id / app_secret 只从环境变量 FEISHU_APP_ID / FEISHU_APP_SECRET 读取；
//   - 绝不接受命令行传入凭据、绝不打印 token / secret、绝不落盘；
//   - 默认走 https://open.feishu.cn，可用 FEISHU_BASE_URL 覆盖（国际版 larksuite 等）。
//
// 退出码：0 正常；1 运行/网络/接口失败；2 参数或环境错误。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := parseFlags(os.Args[2:])

	switch cmd {
	case "doc-dump":
		runDocDump(args)
	case "sheet-dump":
		runSheetDump(args)
	case "resolve-wiki":
		runResolveWiki(args)
	case "ensure-board":
		runEnsureBoard(args)
	case "issues-list":
		runIssuesList(args)
	case "issue-upsert":
		runIssueUpsert(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "未知子命令:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法:
  loop-bridge doc-dump     --doc <document_id> | --wiki <wiki_node_token>
  loop-bridge sheet-dump   --sheet <spreadsheet_token>
  loop-bridge resolve-wiki --node <wiki_node_token>
  loop-bridge ensure-board --app <app_token> [--table <table_id>]
  loop-bridge issues-list  --app <app_token> --table <table_id>
  loop-bridge issue-upsert --app <app_token> --table <table_id> [--key-field external_key] < records.json

环境变量: FEISHU_APP_ID, FEISHU_APP_SECRET（必填）; FEISHU_BASE_URL（可选，默认 https://open.feishu.cn）`)
}

// parseFlags 把 --k v 形式解析成 map，足够覆盖本 CLI 的简单需求。
func parseFlags(argv []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "--") && i+1 < len(argv) {
			m[strings.TrimPrefix(a, "--")] = argv[i+1]
			i++
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// 子命令：doc-dump
// ---------------------------------------------------------------------------

func runDocDump(args map[string]string) {
	doc := args["doc"]
	wiki := args["wiki"]
	if doc == "" && wiki == "" {
		fmt.Fprintln(os.Stderr, "缺少 --doc <document_id> 或 --wiki <wiki_node_token>")
		os.Exit(2)
	}
	c := mustClient()
	if doc == "" {
		// wiki 链接：先把知识库节点解析成底层文档对象
		objToken, objType, err := c.resolveWikiNode(wiki)
		if err != nil {
			fmt.Fprintln(os.Stderr, "解析 wiki 节点失败:", err)
			os.Exit(1)
		}
		if objType != "docx" {
			fmt.Fprintf(os.Stderr, "wiki 节点 obj_type=%s，doc-dump 仅支持 docx 文档\n", objType)
			os.Exit(2)
		}
		doc = objToken
	}
	blocks, err := c.fetchAllDocBlocks(doc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "拉取文档失败:", err)
		os.Exit(1)
	}
	flat := flattenBlocks(blocks)
	writeJSON(flat)
}

// ---------------------------------------------------------------------------
// 子命令：resolve-wiki
// ---------------------------------------------------------------------------

func runResolveWiki(args map[string]string) {
	node := args["node"]
	if node == "" {
		fmt.Fprintln(os.Stderr, "缺少 --node <wiki_node_token>")
		os.Exit(2)
	}
	c := mustClient()
	objToken, objType, err := c.resolveWikiNode(node)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析 wiki 节点失败:", err)
		os.Exit(1)
	}
	writeJSON(map[string]string{"obj_token": objToken, "obj_type": objType})
}

// ---------------------------------------------------------------------------
// 子命令：sheet-dump（读电子表格）
// ---------------------------------------------------------------------------

// 读取上限，避免对超大表构造病态区间；需求表通常远小于此。
const (
	maxSheetRows = 5000
	maxSheetCols = 100
)

func runSheetDump(args map[string]string) {
	sheet := args["sheet"]
	if sheet == "" {
		fmt.Fprintln(os.Stderr, "缺少 --sheet <spreadsheet_token>")
		os.Exit(2)
	}
	c := mustClient()
	metas, err := c.listSheets(sheet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "列电子表格分表失败:", err)
		os.Exit(1)
	}
	type outSheet struct {
		Title  string  `json:"title"`
		Values [][]any `json:"values"`
	}
	out := struct {
		Sheets []outSheet `json:"sheets"`
	}{}
	for _, m := range metas {
		rows, cols := m.RowCount, m.ColCount
		if rows <= 0 || rows > maxSheetRows {
			rows = maxSheetRows
		}
		if cols <= 0 || cols > maxSheetCols {
			cols = maxSheetCols
		}
		rng := fmt.Sprintf("%s!A1:%s%d", m.SheetID, colLetter(cols), rows)
		vals, err := c.readSheetValues(sheet, rng)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取分表 %q 失败: %v\n", m.Title, err)
			continue
		}
		out.Sheets = append(out.Sheets, outSheet{Title: m.Title, Values: vals})
	}
	writeJSON(out)
}

// colLetter 把列序号（1-based）转成 A1 记法的列字母：1→A，26→Z，27→AA。
func colLetter(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	if s == "" {
		s = "A"
	}
	return s
}

// ---------------------------------------------------------------------------
// 子命令：ensure-board（幂等建好 issue 看板字段）
// ---------------------------------------------------------------------------

// fieldSpec 描述 issue 看板需要的一个字段。Type: 1=多行文本 2=数字 3=单选。
type fieldSpec struct {
	Name    string
	Type    int
	Options []string // 仅单选用
}

// boardFields 是 issue 看板的字段契约（与 workflow 写回的字段对应）。
var boardFields = []fieldSpec{
	{"external_key", 1, nil},
	{"requirement", 1, nil},
	{"title", 1, nil},
	{"type", 3, []string{"bug", "gap", "blocker", "spec-question"}},
	{"status", 3, []string{"open", "in_progress", "verifying", "resolved", "wont_fix"}},
	{"severity", 3, []string{"p0", "p1", "p2"}},
	{"acceptance_ref", 1, nil},
	{"evidence", 1, nil},
	{"updated_round", 2, nil},
	{"parent_key", 1, nil}, // 子记录(如用户答复)挂到父 issue 的 external_key
}

func runEnsureBoard(args map[string]string) {
	app, table := args["app"], args["table"]
	if app == "" {
		fmt.Fprintln(os.Stderr, "缺少 --app <app_token>")
		os.Exit(2)
	}
	c := mustClient()
	if table == "" {
		// 未指定表则用多维表格里的第一张表
		tables, err := c.listTables(app)
		if err != nil {
			fmt.Fprintln(os.Stderr, "列数据表失败:", err)
			os.Exit(1)
		}
		if len(tables) == 0 {
			fmt.Fprintln(os.Stderr, "该多维表格没有任何数据表")
			os.Exit(1)
		}
		table = tables[0]
	}
	existing, err := c.listFieldNames(app, table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "列字段失败:", err)
		os.Exit(1)
	}
	created := []string{}
	for _, f := range boardFields {
		if existing[f.Name] {
			continue
		}
		if err := c.createField(app, table, f); err != nil {
			fmt.Fprintf(os.Stderr, "创建字段 %q 失败: %v\n", f.Name, err)
			os.Exit(1)
		}
		created = append(created, f.Name)
	}
	writeJSON(map[string]any{"app": app, "table": table, "created": created, "fields_total": len(boardFields)})
}

// ---------------------------------------------------------------------------
// 子命令：issues-list
// ---------------------------------------------------------------------------

func runIssuesList(args map[string]string) {
	app, table := args["app"], args["table"]
	if app == "" || table == "" {
		fmt.Fprintln(os.Stderr, "缺少 --app <app_token> 或 --table <table_id>")
		os.Exit(2)
	}
	c := mustClient()
	recs, err := c.fetchAllRecords(app, table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "列 Bitable 记录失败:", err)
		os.Exit(1)
	}
	writeJSON(recs)
}

// ---------------------------------------------------------------------------
// 子命令：issue-upsert
// ---------------------------------------------------------------------------

func runIssueUpsert(args map[string]string) {
	app, table := args["app"], args["table"]
	if app == "" || table == "" {
		fmt.Fprintln(os.Stderr, "缺少 --app <app_token> 或 --table <table_id>")
		os.Exit(2)
	}
	keyField := args["key-field"]
	if keyField == "" {
		keyField = "external_key"
	}

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 stdin 失败:", err)
		os.Exit(2)
	}
	var payload struct {
		Records []struct {
			Fields map[string]any `json:"fields"`
		} `json:"records"`
	}
	if err := json.Unmarshal(in, &payload); err != nil {
		fmt.Fprintln(os.Stderr, "解析输入 JSON 失败（期望 {\"records\":[{\"fields\":{...}}]}）:", err)
		os.Exit(2)
	}
	incoming := make([]map[string]any, 0, len(payload.Records))
	for _, r := range payload.Records {
		incoming = append(incoming, r.Fields)
	}

	c := mustClient()
	existing, err := c.fetchAllRecords(app, table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取现有记录失败:", err)
		os.Exit(1)
	}

	creates, updates := planUpsert(existing, incoming, keyField)
	if err := c.batchCreate(app, table, creates); err != nil {
		fmt.Fprintln(os.Stderr, "批量创建失败:", err)
		os.Exit(1)
	}
	if err := c.batchUpdate(app, table, updates); err != nil {
		fmt.Fprintln(os.Stderr, "批量更新失败:", err)
		os.Exit(1)
	}
	writeJSON(map[string]int{"created": len(creates), "updated": len(updates)})
}

// ---------------------------------------------------------------------------
// 纯逻辑（可单测，无网络）
// ---------------------------------------------------------------------------

// FlatBlock 是文档 block 的扁平化表示，供 intake agent 语义切分需求。
type FlatBlock struct {
	Type  int    `json:"type"`            // 飞书 docx block_type
	Level int    `json:"level,omitempty"` // 标题级别 1..9，正文为 0
	Text  string `json:"text"`            // 拼接后的纯文本
}

// rawBlock 只解析我们需要的字段，其余忽略。
type rawBlock struct {
	BlockType int            `json:"block_type"`
	extra     map[string]any // 用于通用提取文本
}

// flattenBlocks 把原始 block 列表转成 FlatBlock，跳过无文本块。
// 通过通用方式（任何含 elements 数组的子对象都视作富文本）提取文本，
// 不硬编码每种 block 类型的字段名，对飞书新增块类型更鲁棒。
func flattenBlocks(blocks []rawBlock) []FlatBlock {
	out := make([]FlatBlock, 0, len(blocks))
	for _, b := range blocks {
		text := extractElementsText(b.extra)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, FlatBlock{
			Type:  b.BlockType,
			Level: headingLevel(b.BlockType),
			Text:  text,
		})
	}
	return out
}

// headingLevel：docx block_type 3..11 对应 heading1..9。
func headingLevel(blockType int) int {
	if blockType >= 3 && blockType <= 11 {
		return blockType - 2
	}
	return 0
}

// extractElementsText 在任意 block 子结构里寻找 {"elements":[{"text_run":{"content":...}}]}
// 并拼接其 content。对 text / heading / bullet / ordered / quote 等均适用。
func extractElementsText(v any) string {
	var sb strings.Builder
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if els, ok := n["elements"].([]any); ok {
				for _, e := range els {
					if em, ok := e.(map[string]any); ok {
						if tr, ok := em["text_run"].(map[string]any); ok {
							if c, ok := tr["content"].(string); ok {
								sb.WriteString(c)
							}
						}
					}
				}
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(v)
	return sb.String()
}

// planUpsert 按 keyField 把 incoming 拆成需新建 / 需更新两组。
// existing 中同 key 的记录返回其 record_id 以便更新；其余新建。
func planUpsert(existing []Record, incoming []map[string]any, keyField string) (creates []map[string]any, updates []recordUpdate) {
	byKey := map[string]string{} // external_key -> record_id
	for _, r := range existing {
		k := extractText(r.Fields[keyField])
		if k != "" {
			byKey[k] = r.RecordID
		}
	}
	for _, fields := range incoming {
		k := extractText(fields[keyField])
		if id, ok := byKey[k]; ok && k != "" {
			updates = append(updates, recordUpdate{RecordID: id, Fields: fields})
		} else {
			creates = append(creates, fields)
		}
	}
	return creates, updates
}

// extractText 把 Bitable 字段值归一成字符串：
// 文本字段在 v1 接口里可能是 string，也可能是 [{"text":"...","type":"text"}]。
func extractText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, e := range t {
			if em, ok := e.(map[string]any); ok {
				if s, ok := em["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// ---------------------------------------------------------------------------
// 飞书 HTTP 客户端
// ---------------------------------------------------------------------------

type client struct {
	base  string
	token string
	http  *http.Client
}

// Record 是 Bitable 记录的精简表示。
type Record struct {
	RecordID string         `json:"record_id"`
	Fields   map[string]any `json:"fields"`
}

type recordUpdate struct {
	RecordID string         `json:"record_id"`
	Fields   map[string]any `json:"fields"`
}

func mustClient() *client {
	id := os.Getenv("FEISHU_APP_ID")
	secret := os.Getenv("FEISHU_APP_SECRET")
	if id == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "FEISHU_APP_ID / FEISHU_APP_SECRET 未设置（应在 shell profile 中导出）")
		os.Exit(2)
	}
	base := os.Getenv("FEISHU_BASE_URL")
	if base == "" {
		base = "https://open.feishu.cn"
	}
	base = strings.TrimRight(base, "/")
	c := &client{base: base, http: &http.Client{Timeout: 30 * time.Second}}
	if err := c.auth(id, secret); err != nil {
		fmt.Fprintln(os.Stderr, "获取 tenant_access_token 失败:", err)
		os.Exit(1)
	}
	return c
}

// auth 用 app_id/app_secret 换 tenant_access_token；token 只存内存，绝不打印。
func (c *client) auth(id, secret string) error {
	body, _ := json.Marshal(map[string]string{"app_id": id, "app_secret": secret})
	req, _ := http.NewRequest(http.MethodPost,
		c.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Code != 0 || out.Token == "" {
		return fmt.Errorf("飞书返回 code=%d msg=%s", out.Code, out.Msg)
	}
	c.token = out.Token
	return nil
}

// doAPI 发起带鉴权的请求并校验飞书统一返回码。
func (c *client) doAPI(method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("接口 %s 返回 code=%d msg=%s", path, envelope.Code, envelope.Msg)
	}
	return envelope.Data, nil
}

// fetchAllDocBlocks 翻页拉取文档全部 block。
func (c *client) fetchAllDocBlocks(docID string) ([]rawBlock, error) {
	var all []rawBlock
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "500")
		q.Set("document_revision_id", "-1")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		data, err := c.doAPI(http.MethodGet,
			"/open-apis/docx/v1/documents/"+url.PathEscape(docID)+"/blocks?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items     []json.RawMessage `json:"items"`
			PageToken string            `json:"page_token"`
			HasMore   bool              `json:"has_more"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var meta struct {
				BlockType int `json:"block_type"`
			}
			_ = json.Unmarshal(item, &meta)
			var extra map[string]any
			_ = json.Unmarshal(item, &extra)
			all = append(all, rawBlock{BlockType: meta.BlockType, extra: extra})
		}
		if !page.HasMore || page.PageToken == "" {
			break
		}
		pageToken = page.PageToken
	}
	return all, nil
}

// resolveWikiNode 把知识库（wiki）节点 token 解析成底层文档对象（obj_token + obj_type）。
// docx 文档时 obj_token 即可直接作为 doc-dump 的 document_id。
func (c *client) resolveWikiNode(nodeToken string) (objToken, objType string, err error) {
	q := url.Values{}
	q.Set("token", nodeToken)
	data, err := c.doAPI(http.MethodGet, "/open-apis/wiki/v2/spaces/get_node?"+q.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Node struct {
			ObjToken string `json:"obj_token"`
			ObjType  string `json:"obj_type"`
		} `json:"node"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", err
	}
	if out.Node.ObjToken == "" {
		return "", "", fmt.Errorf("未解析到 obj_token（确认应用有 wiki 读权限且节点 token 正确）")
	}
	return out.Node.ObjToken, out.Node.ObjType, nil
}

// sheetMeta 是电子表格中一个分表的元信息。
type sheetMeta struct {
	SheetID  string
	Title    string
	RowCount int
	ColCount int
}

// listSheets 列出电子表格的全部分表及其网格尺寸。
func (c *client) listSheets(token string) ([]sheetMeta, error) {
	path := "/open-apis/sheets/v3/spreadsheets/" + url.PathEscape(token) + "/sheets/query"
	data, err := c.doAPI(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Sheets []struct {
			SheetID        string `json:"sheet_id"`
			Title          string `json:"title"`
			GridProperties struct {
				RowCount    int `json:"row_count"`
				ColumnCount int `json:"column_count"`
			} `json:"grid_properties"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	res := make([]sheetMeta, 0, len(out.Sheets))
	for _, s := range out.Sheets {
		res = append(res, sheetMeta{SheetID: s.SheetID, Title: s.Title,
			RowCount: s.GridProperties.RowCount, ColCount: s.GridProperties.ColumnCount})
	}
	return res, nil
}

// readSheetValues 读取某分表指定 A1 区间的单元格值（二维数组）。
func (c *client) readSheetValues(token, rng string) ([][]any, error) {
	path := "/open-apis/sheets/v2/spreadsheets/" + url.PathEscape(token) + "/values/" + url.PathEscape(rng)
	data, err := c.doAPI(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		ValueRange struct {
			Values [][]any `json:"values"`
		} `json:"valueRange"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.ValueRange.Values, nil
}

// listTables 列出多维表格的全部数据表 table_id（按顺序）。
func (c *client) listTables(app string) ([]string, error) {
	path := "/open-apis/bitable/v1/apps/" + url.PathEscape(app) + "/tables?page_size=100"
	data, err := c.doAPI(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			TableID string `json:"table_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		ids = append(ids, it.TableID)
	}
	return ids, nil
}

// listFieldNames 返回某数据表已存在的字段名集合。
func (c *client) listFieldNames(app, table string) (map[string]bool, error) {
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/fields?page_size=100",
		url.PathEscape(app), url.PathEscape(table))
	data, err := c.doAPI(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			FieldName string `json:"field_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, it := range out.Items {
		names[it.FieldName] = true
	}
	return names, nil
}

// createField 在数据表里新建一个字段（单选字段带上选项）。
func (c *client) createField(app, table string, f fieldSpec) error {
	body := map[string]any{"field_name": f.Name, "type": f.Type}
	if len(f.Options) > 0 {
		opts := make([]map[string]any, 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, map[string]any{"name": o})
		}
		body["property"] = map[string]any{"options": opts}
	}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/fields",
		url.PathEscape(app), url.PathEscape(table))
	_, err := c.doAPI(http.MethodPost, path, body)
	return err
}

// fetchAllRecords 翻页拉取 Bitable 表全部记录。
func (c *client) fetchAllRecords(app, table string) ([]Record, error) {
	var all []Record
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "500")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records?%s",
			url.PathEscape(app), url.PathEscape(table), q.Encode())
		data, err := c.doAPI(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items     []Record `json:"items"`
			PageToken string   `json:"page_token"`
			HasMore   bool     `json:"has_more"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if !page.HasMore || page.PageToken == "" {
			break
		}
		pageToken = page.PageToken
	}
	return all, nil
}

func (c *client) batchCreate(app, table string, creates []map[string]any) error {
	if len(creates) == 0 {
		return nil
	}
	recs := make([]map[string]any, 0, len(creates))
	for _, f := range creates {
		recs = append(recs, map[string]any{"fields": f})
	}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records/batch_create",
		url.PathEscape(app), url.PathEscape(table))
	_, err := c.doAPI(http.MethodPost, path, map[string]any{"records": recs})
	return err
}

func (c *client) batchUpdate(app, table string, updates []recordUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	recs := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		recs = append(recs, map[string]any{"record_id": u.RecordID, "fields": u.Fields})
	}
	path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records/batch_update",
		url.PathEscape(app), url.PathEscape(table))
	_, err := c.doAPI(http.MethodPost, path, map[string]any{"records": recs})
	return err
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "编码输出失败:", err)
		os.Exit(1)
	}
}
