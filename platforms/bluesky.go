package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

func PostToBluesky(title, content, identifier, appPassword, fallbackAccessToken string) error {
	_, err := PostToBlueskyWithOptions(title, content, identifier, appPassword, fallbackAccessToken, PostOptions{})
	return err
}

func PostToBlueskyWithOptions(title, content, identifier, appPassword, fallbackAccessToken string, opts PostOptions) (PublishResult, error) {
	const blueskyTextLimit = 300

	accessToken := fallbackAccessToken
	did := ""

	if identifier != "" && appPassword != "" {
		var err error
		accessToken, did, err = createBlueskySession(identifier, appPassword)
		if err != nil {
			return PublishResult{}, err
		}
	}

	if accessToken == "" {
		return PublishResult{}, fmt.Errorf("missing bluesky credentials")
	}

	if did == "" {
		var err error
		did, err = resolveDID(accessToken)
		if err != nil {
			return PublishResult{}, err
		}
	}

	chunks := buildBlueskyThreadChunks(title, content, blueskyTextLimit)
	if len(chunks) == 0 {
		return PublishResult{}, fmt.Errorf("empty bluesky post content")
	}

	var root *strongRef
	var parent *strongRef
	for _, chunk := range chunks {
		created, err := createRecord(accessToken, did, chunk, root, parent)
		if err != nil {
			return PublishResult{}, err
		}
		if root == nil {
			root = created
		}
		parent = created
	}
	result := PublishResult{}
	if root != nil {
		result.RemoteID = root.URI
		if url := blueskyWebURLFromURI(root.URI); url != "" {
			result.RemoteURL = url
		}
	}
	return result, nil
}

type strongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

func createRecord(accessToken, did, text string, root, parent *strongRef) (*strongRef, error) {
	postData := map[string]any{
		"repo":       did,
		"collection": "app.bsky.feed.post",
		"record": map[string]any{
			"$type":     "app.bsky.feed.post",
			"text":      text,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	if root != nil && parent != nil {
		postData["record"].(map[string]any)["reply"] = map[string]any{
			"root": map[string]string{
				"uri": root.URI,
				"cid": root.CID,
			},
			"parent": map[string]string{
				"uri": parent.URI,
				"cid": parent.CID,
			},
		}
	}
	body, _ := json.Marshal(postData)

	req, _ := http.NewRequest("POST", "https://bsky.social/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bluesky createRecord API error: %s: %s", resp.Status, string(respBody))
	}

	var out strongRef
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.URI == "" || out.CID == "" {
		return nil, fmt.Errorf("bluesky createRecord response missing uri/cid")
	}
	return &out, nil
}

func createBlueskySession(identifier, appPassword string) (string, string, error) {
	payload := map[string]string{
		"identifier": identifier,
		"password":   appPassword,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://bsky.social/xrpc/com.atproto.server.createSession", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("bluesky createSession API error: %s: %s", resp.Status, string(respBody))
	}

	var session struct {
		AccessJwt string `json:"accessJwt"`
		DID       string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", "", err
	}
	if session.AccessJwt == "" || session.DID == "" {
		return "", "", fmt.Errorf("bluesky session response missing accessJwt or did")
	}
	return session.AccessJwt, session.DID, nil
}

func resolveDID(accessToken string) (string, error) {
	req, _ := http.NewRequest("GET", "https://bsky.social/xrpc/com.atproto.server.getSession", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("bluesky getSession API error: %s: %s", resp.Status, string(respBody))
	}

	var session struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", err
	}
	if session.DID == "" {
		return "", fmt.Errorf("bluesky getSession response missing did")
	}
	return session.DID, nil
}
func buildBlueskyThreadChunks(title, content string, limit int) []string {
	units := splitIntoSentences(content)
	if len(units) == 0 {
		return nil
	}

	chunks := make([]string, 0)
	current := ""
	for _, unit := range units {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}

		parts := splitLongUnit(unit, limit)
		for _, part := range parts {
			if current == "" {
				current = part
				continue
			}
			candidate := current + " " + part
			if textLength(candidate) <= limit {
				current = candidate
				continue
			}
			chunks = append(chunks, current)
			current = part
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func splitIntoSentences(text string) []string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	runes := []rune(trimmed)
	sentences := make([]string, 0)
	var b strings.Builder
	for i, r := range runes {
		b.WriteRune(r)
		if !isSentenceBoundaryRune(r) {
			continue
		}
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		s := strings.TrimSpace(b.String())
		if s != "" {
			sentences = append(sentences, s)
		}
		b.Reset()
	}

	rest := strings.TrimSpace(b.String())
	if rest != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

func isSentenceBoundaryRune(r rune) bool {
	return r == '.' || r == '!' || r == '?'
}

func splitLongUnit(unit string, limit int) []string {
	if textLength(unit) <= limit {
		return []string{unit}
	}

	words := strings.Fields(unit)
	if len(words) == 0 {
		return splitByRunes(unit, limit)
	}

	parts := make([]string, 0)
	current := ""
	for _, w := range words {
		if textLength(w) > limit {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
			parts = append(parts, splitByRunes(w, limit)...)
			continue
		}
		if current == "" {
			current = w
			continue
		}
		candidate := current + " " + w
		if textLength(candidate) <= limit {
			current = candidate
			continue
		}
		parts = append(parts, current)
		current = w
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

func splitByRunes(s string, limit int) []string {
	runes := []rune(s)
	if len(runes) == 0 || limit <= 0 {
		return nil
	}
	out := make([]string, 0, (len(runes)+limit-1)/limit)
	for start := 0; start < len(runes); start += limit {
		end := start + limit
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}

func textLength(s string) int {
	return len([]rune(s))
}

func blueskyWebURLFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) < 5 {
		return ""
	}
	did := parts[2]
	rkey := parts[len(parts)-1]
	if did == "" || rkey == "" {
		return ""
	}
	return "https://bsky.app/profile/" + did + "/post/" + rkey
}
