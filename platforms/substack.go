package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func PostToSubstack(title, content, endpoint, token string) error {
	_, err := PostToSubstackWithOptions(title, content, endpoint, token, PostOptions{})
	return err
}

func PostToSubstackWithOptions(title, content, endpoint, token string, opts PostOptions) (PublishResult, error) {
	payload := map[string]string{
		"title":   title,
		"content": content,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", endpoint, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return PublishResult{}, fmt.Errorf("substack endpoint error: %s: %s", resp.Status, string(raw))
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	result := PublishResult{}
	if id, ok := out["id"].(string); ok {
		result.RemoteID = id
	}
	if url, ok := out["url"].(string); ok {
		result.RemoteURL = url
	}
	return result, nil
}
