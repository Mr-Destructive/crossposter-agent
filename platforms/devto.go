package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type DevtoArticle struct {
	Article struct {
		Title        string `json:"title"`
		Published    bool   `json:"published"`
		BodyMarkdown string `json:"body_markdown"`
	} `json:"article"`
}

func PostToDevto(title, content, apiKey string) error {
	_, err := PostToDevtoWithOptions(title, content, apiKey, PostOptions{})
	return err
}

func PostToDevtoWithOptions(title, content, apiKey string, opts PostOptions) (PublishResult, error) {
	article := DevtoArticle{}
	article.Article.Title = title
	article.Article.Published = opts.PublishMode != "draft"
	article.Article.BodyMarkdown = content

	payload := map[string]any{
		"article": map[string]any{
			"title":         article.Article.Title,
			"published":     article.Article.Published,
			"body_markdown": article.Article.BodyMarkdown,
		},
	}
	if len(opts.Tags) > 0 {
		payload["article"].(map[string]any)["tags"] = opts.Tags
	}
	if opts.Series != "" {
		payload["article"].(map[string]any)["series"] = opts.Series
	}
	if opts.CanonicalURL != "" {
		payload["article"].(map[string]any)["canonical_url"] = opts.CanonicalURL
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://dev.to/api/articles", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return PublishResult{}, fmt.Errorf("devto API error: %s", resp.Status)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &out)
	result := PublishResult{RemoteURL: out.URL}
	if out.ID > 0 {
		result.RemoteID = fmt.Sprintf("%d", out.ID)
	}
	return result, nil
}
