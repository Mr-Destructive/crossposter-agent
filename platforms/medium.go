package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func PostToMedium(title, content, token, publicationID string) error {
	_, err := PostToMediumWithOptions(title, content, token, publicationID, PostOptions{})
	return err
}

func PostToMediumWithOptions(title, content, token, publicationID string, opts PostOptions) (PublishResult, error) {
	publishStatus := "public"
	if opts.PublishMode == "draft" {
		publishStatus = "draft"
	}
	payload := map[string]any{
		"title":         title,
		"contentFormat": "markdown",
		"content":       content,
		"publishStatus": publishStatus,
	}
	if opts.CanonicalURL != "" {
		payload["canonicalUrl"] = opts.CanonicalURL
	}
	if len(opts.Tags) > 0 {
		payload["tags"] = opts.Tags
	}
	body, _ := json.Marshal(payload)

	url := "https://api.medium.com/v1/publications/" + publicationID + "/posts"
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return PublishResult{}, fmt.Errorf("medium API error: %s: %s", resp.Status, string(raw))
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	return PublishResult{RemoteID: out.Data.ID, RemoteURL: out.Data.URL}, nil
}
