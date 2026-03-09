package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func PostToX(title, content, bearerToken string) error {
	_, err := PostToXWithOptions(title, content, bearerToken, PostOptions{})
	return err
}

func PostToXWithOptions(title, content, bearerToken string, opts PostOptions) (PublishResult, error) {
	text := buildShortFormText(title, content, 280)
	payload := map[string]string{"text": text}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "https://api.x.com/2/tweets", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PublishResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return PublishResult{}, fmt.Errorf("x API error: %s: %s", resp.Status, string(raw))
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	return PublishResult{RemoteID: out.Data.ID}, nil
}
