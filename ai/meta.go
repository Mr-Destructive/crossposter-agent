package ai

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Plan struct {
	Platforms []string                  `json:"platforms"`
	Title     string                    `json:"title"`
	Schedule  string                    `json:"schedule,omitempty"`
	Options   map[string]map[string]any `json:"options,omitempty"`
}

type ChatMessage struct {
	Role    string
	Content string
}

type Client struct {
	httpClient *http.Client
}

func NewClient(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

func (c *Client) Infer(ctx context.Context, intent, content, currentTitle string) (Plan, error) {
	cookies, err := c.getCookies(ctx)
	if err != nil {
		return Plan{}, err
	}
	accessToken, err := c.getAccessToken(ctx, cookies)
	if err != nil {
		return Plan{}, err
	}

	prompt := buildPlannerPrompt(intent, content, currentTitle)
	text, err := c.sendPrompt(ctx, accessToken, prompt)
	if err != nil {
		return Plan{}, err
	}
	obj := extractJSONObject(text)
	if obj == "" {
		return Plan{}, fmt.Errorf("meta response missing json")
	}
	var plan Plan
	if err := json.Unmarshal([]byte(obj), &plan); err != nil {
		return Plan{}, err
	}
	for i, p := range plan.Platforms {
		plan.Platforms[i] = strings.ToLower(strings.TrimSpace(p))
	}
	plan.Platforms = unique(plan.Platforms)
	plan.Title = strings.TrimSpace(plan.Title)
	return plan, nil
}

func (c *Client) Chat(ctx context.Context, history []ChatMessage, draftContext string) (string, error) {
	cookies, err := c.getCookies(ctx)
	if err != nil {
		return "", err
	}
	accessToken, err := c.getAccessToken(ctx, cookies)
	if err != nil {
		return "", err
	}
	prompt := buildChatPrompt(history, draftContext)
	text, err := c.sendPrompt(ctx, accessToken, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func (c *Client) getCookies(ctx context.Context) (map[string]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.meta.ai/", nil)
	req.Header.Set("user-agent", "Mozilla/5.0")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	cookies := map[string]string{
		"_js_datr":  extractBetween(html, `_js_datr":{"value":"`, `",`),
		"abra_csrf": extractBetween(html, `abra_csrf":{"value":"`, `",`),
		"datr":      extractBetween(html, `datr":{"value":"`, `",`),
		"lsd":       extractBetween(html, `"LSD",[],{"token":"`, `"}`),
	}
	if cookies["_js_datr"] == "" || cookies["abra_csrf"] == "" || cookies["datr"] == "" || cookies["lsd"] == "" {
		return nil, fmt.Errorf("failed to parse meta cookies")
	}
	return cookies, nil
}

func (c *Client) getAccessToken(ctx context.Context, cookies map[string]string) (string, error) {
	vars := map[string]any{
		"dob":             "1999-01-01",
		"icebreaker_type": "TEXT",
		"__relay_internal__pv__WebPixelRatiorelayprovider": 1,
	}
	varsJSON, _ := json.Marshal(vars)
	form := url.Values{}
	form.Set("lsd", cookies["lsd"])
	form.Set("fb_api_caller_class", "RelayModern")
	form.Set("fb_api_req_friendly_name", "useAbraAcceptTOSForTempUserMutation")
	form.Set("variables", string(varsJSON))
	form.Set("doc_id", "7604648749596940")

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.meta.ai/api/graphql/", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("x-fb-friendly-name", "useAbraAcceptTOSForTempUserMutation")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("cookie", fmt.Sprintf("_js_datr=%s; abra_csrf=%s; datr=%s;", cookies["_js_datr"], cookies["abra_csrf"], cookies["datr"]))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Data struct {
			Accept struct {
				Auth struct {
					AccessToken string `json:"access_token"`
				} `json:"new_temp_user_auth"`
			} `json:"xab_abra_accept_terms_of_service"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	token := strings.TrimSpace(parsed.Data.Accept.Auth.AccessToken)
	if token == "" {
		return "", fmt.Errorf("meta access token not returned")
	}
	time.Sleep(1100 * time.Millisecond)
	return token, nil
}

func (c *Client) sendPrompt(ctx context.Context, accessToken, message string) (string, error) {
	vars := map[string]any{
		"message":                map[string]string{"sensitive_string_value": message},
		"externalConversationId": newUUID(),
		"offlineThreadingId":     generateOfflineThreadingID(),
		"suggestedPromptIndex":   nil,
		"flashVideoRecapInput":   map[string]any{"images": []any{}},
		"flashPreviewInput":      nil,
		"promptPrefix":           nil,
		"entrypoint":             "ABRA__CHAT__TEXT",
		"icebreaker_type":        "TEXT",
		"__relay_internal__pv__AbraDebugDevOnlyrelayprovider": false,
		"__relay_internal__pv__WebPixelRatiorelayprovider":    1,
	}
	varsJSON, _ := json.Marshal(vars)
	form := url.Values{}
	form.Set("access_token", accessToken)
	form.Set("fb_api_caller_class", "RelayModern")
	form.Set("fb_api_req_friendly_name", "useAbraSendMessageMutation")
	form.Set("variables", string(varsJSON))
	form.Set("server_timestamps", "true")
	form.Set("doc_id", "7783822248314888")

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://graph.meta.ai/graphql?locale=user", strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("x-fb-friendly-name", "useAbraSendMessageMutation")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := extractFinalMessage(string(body))
	if text == "" {
		return "", fmt.Errorf("empty meta ai response")
	}
	return strings.TrimSpace(text), nil
}

func extractFinalMessage(raw string) string {
	lines := strings.Split(raw, "\n")
	final := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		msg := messageFromLine(obj)
		if msg == "" {
			continue
		}
		streamState := getString(getPath(obj, "data", "node", "bot_response_message", "streaming_state"))
		if streamState == "OVERALL_DONE" {
			return msg
		}
		final = msg
	}
	return final
}

func messageFromLine(obj map[string]any) string {
	content := getPath(obj, "data", "node", "bot_response_message", "composed_text", "content")
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text := strings.TrimSpace(getString(m["text"]))
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func buildPlannerPrompt(intent, content, currentTitle string) string {
	titleInstruction := "Infer a concise title based on intent."
	if currentTitle != "" {
		titleInstruction = "Respect the user-provided title exactly: " + currentTitle
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return `Return only compact JSON: {"platforms":["devto","hashnode",...],"title":"...","schedule":"2026-03-10T14:00:00Z","options":{"devto":{"tags":["go","api"]},"reddit":{"subreddit":"golang"},...}}. 
	Platforms allowed: devto,hashnode,bluesky,medium,reddit,x,substack. 
	Current UTC time is: ` + now + `
	If the intent contains an explicit time or time zone, set "schedule" to an ISO 8601 UTC timestamp that represents that time; otherwise set "schedule" to an empty string. 
	Options can include tags (array), subreddit (string), canonical_url (string).
	Intention: ` + intent + `
	Content: ` + content + `
	Title instruction: ` + titleInstruction
}

func buildChatPrompt(history []ChatMessage, draftContext string) string {
	var b strings.Builder
	b.WriteString("You are a cross-posting editor agent.\n")
	b.WriteString("Goals:\n")
	b.WriteString("1) Improve the post for multi-platform publishing.\n")
	b.WriteString("2) Ask concise follow-up questions when unclear.\n")
	b.WriteString("3) Suggest platform-specific fields and parameter choices.\n")
	b.WriteString("4) Keep responses actionable and brief.\n\n")
	b.WriteString("Current Draft Context:\n")
	b.WriteString(draftContext)
	b.WriteString("\n\nConversation:\n")
	for _, m := range history {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n")
	}
	b.WriteString("\nRespond with:\n")
	b.WriteString("- Short analysis\n")
	b.WriteString("- Concrete edits to make\n")
	b.WriteString("- Platform-field recommendations\n")
	b.WriteString("- One follow-up question if needed\n")
	return b.String()
}

func extractJSONObject(s string) string {
	start := -1
	depth := 0
	for i, r := range s {
		if r == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		}
		if r == '}' {
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					return s[start : i+1]
				}
			}
		}
	}
	return ""
}

func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

func getPath(m map[string]any, keys ...string) any {
	cur := any(m)
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

func getString(v any) string {
	s, _ := v.(string)
	return s
}

func unique(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateOfflineThreadingID() string {
	nowMs := time.Now().UnixMilli()
	r := make([]byte, 8)
	_, _ = rand.Read(r)
	rand64 := uint64(0)
	for _, v := range r {
		rand64 = (rand64 << 8) | uint64(v)
	}
	masked := rand64 & ((1 << 22) - 1)
	val := (uint64(nowMs) << 22) | masked
	return strconv.FormatUint(val, 10)
}
