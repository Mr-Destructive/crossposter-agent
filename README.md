# Crossposter Agent

Crossposter Agent distributes content across multiple platforms simultaneously. It supports scheduling, AI-assisted planning, and platform-specific configurations like tags and canonical URLs.

## Demo

![Crossposter Demo](https://github.com/Mr-Destructive/crossposter-agent/raw/main/demos/demo.gif)

## Features

*   Support for Dev.to, Hashnode, Bluesky, Medium, Reddit, X, and Substack.
*   AI-assisted intent analysis to suggest titles and platforms.
*   Interactive compose sessions for refining posts.
*   Scheduled posting with timezone support.
*   Job tracking dashboard with real-time status and error logs.
*   Platform-specific options like series, tags, and subreddit targeting.

## Tech Stack

*   Backend: Go
*   Database: SQLite
*   Frontend: HTML Templates, Vanilla CSS
*   AI: Integration for content planning and chat sessions

## Getting Started

1.  Clone the repository.
2.  Set up your `.env` file with an `AES_KEY` (32 bytes) and AI provider credentials.
3.  Run the application:
    ```bash
    go run main.go
    ```
4.  Access the dashboard at `http://localhost:8080`.

## Architecture

The project uses a worker pool to handle asynchronous job delivery. Each platform integration is isolated in the `platforms/` directory, while `db/` contains the SQL schema and generated queries via sqlc.
