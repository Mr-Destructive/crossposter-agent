package workers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/mr-destructive/crossposter-agent/auth"
	crossposter_db "github.com/mr-destructive/crossposter-agent/db"
	db "github.com/mr-destructive/crossposter-agent/db"
	"github.com/mr-destructive/crossposter-agent/platforms"
)

type platformCredentialPayload struct {
	Kind                string `json:"kind"`
	Token               string `json:"token,omitempty"`
	HashnodeHost        string `json:"hashnode_host,omitempty"`
	MediumPublicationID string `json:"medium_publication_id,omitempty"`
	RedditSubreddit     string `json:"reddit_subreddit,omitempty"`
	Identifier          string `json:"identifier,omitempty"`
	AppPassword         string `json:"app_password,omitempty"`
	SubstackEndpoint    string `json:"substack_endpoint,omitempty"`
}

const (
	maxRetryAttempts = int64(5)
	baseRetryDelay   = 30 * time.Second
	maxRetryDelay    = 15 * time.Minute
)

func StartWorkerPool(ctx context.Context, queries *crossposter_db.Queries, jobs chan crossposter_db.PostJob, aesKey string, onRetry func(crossposter_db.PostJob)) {
	workerCount := 3
	for i := 0; i < workerCount; i++ {
		go worker(ctx, queries, jobs, aesKey, onRetry)
	}
}

func worker(ctx context.Context, queries *db.Queries, jobs <-chan db.PostJob, aesKey string, onRetry func(db.PostJob)) {
	for job := range jobs {
		post, err := queries.GetPostByID(ctx, job.PostID)
		if err != nil {
			log.Println("Failed to fetch post:", err)
			handleJobError(ctx, queries, job, err, onRetry)
			continue
		}

		account, err := queries.GetPlatformAccount(ctx, db.GetPlatformAccountParams{
			UserID:   post.UserID,
			Platform: job.Platform,
		})
		if err != nil {
			log.Println("Failed to fetch platform account:", err)
			handleJobError(ctx, queries, job, err, onRetry)
			continue
		}

		secret, err := auth.DecryptToken(account.TokenEncrypted, aesKey)
		if err != nil {
			log.Println("Failed to decrypt token:", err)
			handleJobError(ctx, queries, job, err, onRetry)
			continue
		}

		payload := platformCredentialPayload{}
		if err := json.Unmarshal([]byte(secret), &payload); err != nil {
			payload.Token = secret
		}
		_ = appendEvent(ctx, queries, post.UserID, job, "processing", "Worker started processing job", nil)

		opts := loadPostOptions(ctx, queries, post.UserID, post.ID, job.Platform)

		caps := platforms.Capabilities(job.Platform)
		payloadText := post.Content
		if !caps.SupportsLongForm && caps.SupportsShortForm {
			payloadText = platforms.BuildShortForm(post.Title, post.Content)
		}
		result := platforms.PublishResult{}
		switch job.Platform {
		case "devto":
			if payload.Token == "" {
				err = fmt.Errorf("missing Dev.to token")
				break
			}
			result, err = platforms.PostToDevtoWithOptions(post.Title, payloadText, payload.Token, opts)
		case "hashnode":
			if payload.Token == "" {
				err = fmt.Errorf("missing Hashnode token")
				break
			}
			if payload.HashnodeHost == "" {
				err = fmt.Errorf("missing Hashnode blog host")
				break
			}
			if len([]rune(post.Title)) < 6 {
				err = fmt.Errorf("hashnode validation: title must be >= 6 chars, got %d (%q)", len([]rune(post.Title)), post.Title)
				break
			}
			if len([]rune(post.Content)) < 5 {
				err = fmt.Errorf("hashnode validation: content must be >= 5 chars, got %d", len([]rune(post.Content)))
				break
			}
			hashnodePost, postErr := platforms.PostToHashnode(post.Title, payloadText, payload.Token, payload.HashnodeHost)
			err = postErr
			result = platforms.PublishResult{RemoteID: hashnodePost.ID, RemoteURL: hashnodePost.URL}
		case "bluesky":
			result, err = platforms.PostToBlueskyWithOptions(post.Title, payloadText, payload.Identifier, payload.AppPassword, payload.Token, opts)
		case "x":
			if payload.Token == "" {
				err = fmt.Errorf("missing X bearer token")
				break
			}
			result, err = platforms.PostToXWithOptions(post.Title, payloadText, payload.Token, opts)
		case "reddit":
			subreddit := payload.RedditSubreddit
			if opts.Subreddit != "" {
				subreddit = opts.Subreddit
			}
			if payload.Token == "" || subreddit == "" {
				err = fmt.Errorf("missing Reddit token or subreddit")
				break
			}
			result, err = platforms.PostToRedditWithOptions(post.Title, payloadText, payload.Token, subreddit, opts)
		case "medium":
			if payload.Token == "" || payload.MediumPublicationID == "" {
				err = fmt.Errorf("missing Medium token or publication id")
				break
			}
			result, err = platforms.PostToMediumWithOptions(post.Title, payloadText, payload.Token, payload.MediumPublicationID, opts)
		case "substack":
			if payload.Token == "" || payload.SubstackEndpoint == "" {
				err = fmt.Errorf("missing Substack endpoint or token")
				break
			}
			result, err = platforms.PostToSubstackWithOptions(post.Title, payloadText, payload.SubstackEndpoint, payload.Token, opts)
		default:
			err = fmt.Errorf("unsupported platform: %s", job.Platform)
		}

		if err != nil {
			log.Println("Job failed:", job.ID, err)
			_ = appendEvent(ctx, queries, post.UserID, job, "failed", err.Error(), nil)
			handleJobError(ctx, queries, job, err, onRetry)
			continue
		}

		if result.RemoteID != "" {
			if refErr := queries.UpsertPlatformPostRef(ctx, db.UpsertPlatformPostRefParams{
				UserID:   post.UserID,
				PostID:   post.ID,
				Platform: job.Platform,
				RemoteID: result.RemoteID,
				RemoteUrl: sql.NullString{
					String: result.RemoteURL,
					Valid:  result.RemoteURL != "",
				},
			}); refErr != nil {
				err = refErr
				log.Println("Job failed while storing remote reference:", job.ID, err)
				_ = appendEvent(ctx, queries, post.UserID, job, "failed", err.Error(), nil)
				handleJobError(ctx, queries, job, err, onRetry)
				continue
			}
		}

		nextAttempt := currentAttempts(job) + 1
		updateErr := queries.UpdateJobStatus(ctx, db.UpdateJobStatusParams{
			ID:          job.ID,
			Status:      sql.NullString{String: "success", Valid: true},
			RetryCount:  sql.NullInt64{Int64: nextAttempt, Valid: true},
			LastError:   sql.NullString{},
			ScheduledAt: sql.NullTime{},
		})
		if updateErr != nil {
			log.Println("Failed to update job status:", updateErr)
		}
		_ = appendEvent(ctx, queries, post.UserID, job, "success", "Posted successfully", map[string]any{
			"remote_id":  result.RemoteID,
			"remote_url": result.RemoteURL,
		})
	}
}

func handleJobError(ctx context.Context, queries *db.Queries, job db.PostJob, e error, onRetry func(db.PostJob)) {
	nextAttempt := currentAttempts(job) + 1
	lastError := sql.NullString{String: fmt.Sprintf("%v", e), Valid: true}
	if nextAttempt >= maxRetryAttempts {
		if err := queries.UpdateJobStatus(ctx, db.UpdateJobStatusParams{
			ID:          job.ID,
			Status:      sql.NullString{String: "failed", Valid: true},
			RetryCount:  sql.NullInt64{Int64: nextAttempt, Valid: true},
			LastError:   lastError,
			ScheduledAt: sql.NullTime{},
		}); err != nil {
			log.Println("Failed to set final job failure:", err)
		}
		_ = appendEvent(ctx, queries, 0, job, "failed_final", e.Error(), nil)
		return
	}

	delay := retryDelayForAttempt(nextAttempt)
	scheduled := time.Now().UTC().Add(delay)
	if err := queries.UpdateJobStatus(ctx, db.UpdateJobStatusParams{
		ID:          job.ID,
		Status:      sql.NullString{String: "pending", Valid: true},
		RetryCount:  sql.NullInt64{Int64: nextAttempt, Valid: true},
		LastError:   lastError,
		ScheduledAt: sql.NullTime{Time: scheduled, Valid: true},
	}); err != nil {
		log.Println("Failed to reschedule job retry:", err)
		return
	}
	_ = appendEvent(ctx, queries, 0, job, "retry_scheduled", fmt.Sprintf("Retry %d scheduled in %s", nextAttempt, delay), map[string]any{
		"retry_count":  nextAttempt,
		"scheduled_at": scheduled.UTC().Format(time.RFC3339),
	})

	if onRetry != nil {
		job.Status = sql.NullString{String: "pending", Valid: true}
		job.RetryCount = sql.NullInt64{Int64: nextAttempt, Valid: true}
		job.LastError = lastError
		job.ScheduledAt = sql.NullTime{Time: scheduled, Valid: true}
		onRetry(job)
	}
}

func loadPostOptions(ctx context.Context, queries *db.Queries, userID, postID int64, platform string) platforms.PostOptions {
	row, err := queries.GetPostPlatformOptions(ctx, db.GetPostPlatformOptionsParams{
		UserID:   userID,
		PostID:   postID,
		Platform: platform,
	})
	if err != nil {
		return platforms.PostOptions{}
	}
	var out platforms.PostOptions
	if err := json.Unmarshal([]byte(row.OptionsJson), &out); err != nil {
		return platforms.PostOptions{}
	}
	return out
}

func appendEvent(ctx context.Context, queries *db.Queries, userID int64, job db.PostJob, stage, message string, details map[string]any) error {
	if userID == 0 {
		post, err := queries.GetPostByID(ctx, job.PostID)
		if err == nil {
			userID = post.UserID
		}
	}
	if userID == 0 {
		return nil
	}
	var detailsJSON sql.NullString
	if len(details) > 0 {
		if raw, err := json.Marshal(details); err == nil {
			detailsJSON = sql.NullString{String: string(raw), Valid: true}
		}
	}
	return queries.CreateJobEvent(ctx, db.CreateJobEventParams{
		UserID:      userID,
		JobID:       job.ID,
		PostID:      job.PostID,
		Platform:    job.Platform,
		Stage:       stage,
		Message:     message,
		DetailsJson: detailsJSON,
	})
}

func currentAttempts(job db.PostJob) int64 {
	if !job.RetryCount.Valid || job.RetryCount.Int64 < 0 {
		return 0
	}
	return job.RetryCount.Int64
}

func retryDelayForAttempt(attempt int64) time.Duration {
	if attempt <= 1 {
		return baseRetryDelay
	}
	delay := baseRetryDelay
	for i := int64(1); i < attempt; i++ {
		delay *= 2
		if delay >= maxRetryDelay {
			return maxRetryDelay
		}
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}
