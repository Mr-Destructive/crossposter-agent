package platforms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const hashnodeGraphQLEndpoint = "https://gql.hashnode.com"

type hashnodeGraphQLResp[T any] struct {
	Data   T `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type HashnodePost struct {
	ID  string
	URL string
}

func PostToHashnode(title, content, apiKey, blogHost string) (HashnodePost, error) {
	publicationID, err := getHashnodePublicationID(apiKey, blogHost)
	if err != nil {
		return HashnodePost{}, err
	}

	mutation := `mutation PublishPost($input: PublishPostInput!) {
  publishPost(input: $input) {
    post {
      id
      url
    }
  }
}`
	variables := map[string]any{
		"input": map[string]any{
			"publicationId":   publicationID,
			"title":           title,
			"contentMarkdown": content,
		},
	}

	var out struct {
		PublishPost struct {
			Post struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"post"`
		} `json:"publishPost"`
	}

	if err := runHashnodeGraphQL(apiKey, mutation, variables, &out); err != nil {
		return HashnodePost{}, err
	}
	if out.PublishPost.Post.ID == "" {
		return HashnodePost{}, fmt.Errorf("hashnode publish failed: empty post id")
	}
	return HashnodePost{ID: out.PublishPost.Post.ID, URL: out.PublishPost.Post.URL}, nil
}

func UpdateHashnodePost(postID, title, content, apiKey, blogHost string) (HashnodePost, error) {
	mutation := `mutation UpdatePost($input: UpdatePostInput!) {
  updatePost(input: $input) {
    post {
      id
      url
    }
  }
}`
	variables := map[string]any{
		"input": map[string]any{
			"id":              postID,
			"title":           title,
			"contentMarkdown": content,
		},
	}
	if blogHost != "" {
		publicationID, err := getHashnodePublicationID(apiKey, blogHost)
		if err != nil {
			return HashnodePost{}, err
		}
		variables["input"].(map[string]any)["publicationId"] = publicationID
	}

	var out struct {
		UpdatePost struct {
			Post struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"post"`
		} `json:"updatePost"`
	}

	if err := runHashnodeGraphQL(apiKey, mutation, variables, &out); err != nil {
		return HashnodePost{}, err
	}
	if out.UpdatePost.Post.ID == "" {
		return HashnodePost{}, fmt.Errorf("hashnode update failed: empty post id")
	}
	return HashnodePost{ID: out.UpdatePost.Post.ID, URL: out.UpdatePost.Post.URL}, nil
}

func RemoveHashnodePost(postID, apiKey string) error {
	mutation := `mutation RemovePost($input: RemovePostInput!) {
  removePost(input: $input) {
    post {
      id
    }
  }
}`
	variables := map[string]any{
		"input": map[string]any{
			"id": postID,
		},
	}

	var out struct {
		RemovePost struct {
			Post struct {
				ID string `json:"id"`
			} `json:"post"`
		} `json:"removePost"`
	}
	if err := runHashnodeGraphQL(apiKey, mutation, variables, &out); err != nil {
		return err
	}
	if out.RemovePost.Post.ID == "" {
		return fmt.Errorf("hashnode remove failed: empty post id")
	}
	return nil
}

func getHashnodePublicationID(apiKey, blogHost string) (string, error) {
	query := `query Publication($host: String!) {
  publication(host: $host) {
    id
  }
}`
	variables := map[string]any{"host": blogHost}

	var out struct {
		Publication struct {
			ID string `json:"id"`
		} `json:"publication"`
	}

	if err := runHashnodeGraphQL(apiKey, query, variables, &out); err != nil {
		return "", err
	}
	if out.Publication.ID == "" {
		return "", fmt.Errorf("hashnode publication not found for host %s", blogHost)
	}
	return out.Publication.ID, nil
}

func runHashnodeGraphQL(apiKey, query string, variables map[string]any, out any) error {
	payload := map[string]any{
		"query":     query,
		"variables": variables,
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", hashnodeGraphQLEndpoint, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hashnode API error: %s: %s", resp.Status, string(respBody))
	}

	wrapped := hashnodeGraphQLResp[json.RawMessage]{}
	if err := json.Unmarshal(respBody, &wrapped); err != nil {
		return err
	}
	if len(wrapped.Errors) > 0 {
		return fmt.Errorf("hashnode graphql error: %s", wrapped.Errors[0].Message)
	}
	if len(wrapped.Data) == 0 {
		return fmt.Errorf("hashnode graphql returned empty data")
	}
	if err := json.Unmarshal(wrapped.Data, out); err != nil {
		return err
	}
	return nil
}
