package platforms

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func PostToReddit(title, content, bearerToken, subreddit string) error {
	_, err := PostToRedditWithOptions(title, content, bearerToken, subreddit, PostOptions{})
	return err
}

func PostToRedditWithOptions(title, content, bearerToken, subreddit string, opts PostOptions) (PublishResult, error) {
	targetSubreddit := subreddit
	if opts.Subreddit != "" {
		targetSubreddit = opts.Subreddit
	}
	values := url.Values{}
	values.Set("api_type", "json")
	values.Set("kind", "self")
	values.Set("sr", strings.TrimPrefix(targetSubreddit, "r/"))
	values.Set("title", title)
	values.Set("text", content)

	req, _ := http.NewRequest("POST", "https://oauth.reddit.com/api/submit", strings.NewReader(values.Encode()))
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "crossposter-agent/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return PublishResult{}, fmt.Errorf("reddit API error: %s: %s", resp.Status, string(raw))
	}
	var parsed struct {
		JSON struct {
			Data struct {
				URL string `json:"url"`
				ID  string `json:"id"`
			} `json:"data"`
		} `json:"json"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return PublishResult{RemoteID: parsed.JSON.Data.ID, RemoteURL: parsed.JSON.Data.URL}, nil
}
