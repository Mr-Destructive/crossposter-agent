package platforms

type PostOptions struct {
	Tags         []string `json:"tags,omitempty"`
	Series       string   `json:"series,omitempty"`
	PublishMode  string   `json:"publish_mode,omitempty"`
	CanonicalURL string   `json:"canonical_url,omitempty"`
	Subreddit    string   `json:"subreddit,omitempty"`
}

type PublishResult struct {
	RemoteID  string
	RemoteURL string
}
