package events

import "time"

const SchemaVersion = "v1"

const (
	ItemStatusNew        = "NEW"
	ItemStatusAnalyzed   = "ANALYZED"
	ItemStatusDispatched = "DISPATCHED"
	ItemStatusFailed     = "FAILED"
)

type RawJobItem struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	FeedURL       string    `json:"feed_url"`
	SourceLabel   string    `json:"source_label,omitempty"`
	GUID          string    `json:"guid,omitempty"`
	URL           string    `json:"url"`
	Title         string    `json:"title"`
	Description   string    `json:"description,omitempty"`
	Links         []string  `json:"links,omitempty"`
	ImageURLs     []string  `json:"image_urls,omitempty"`
	PublishedAt   time.Time `json:"published_at"`
	FetchedAt     time.Time `json:"fetched_at"`
}

type AnalyzedJob struct {
	SchemaVersion       string    `json:"schema_version"`
	ID                  string    `json:"id"`
	GUID                string    `json:"guid,omitempty"`
	SourceURL           string    `json:"source_url"`
	SourceLabel         string    `json:"source_label,omitempty"`
	Title               string    `json:"title"`
	JobCategory         string    `json:"job_category"`
	Role                string    `json:"role"`
	Company             string    `json:"company"`
	Seniority           string    `json:"seniority"`
	Location            string    `json:"location"`
	RemoteType          string    `json:"remote_type"`
	TechStack           []string  `json:"tech_stack"`
	ContractType        string    `json:"contract_type"`
	Salary              string    `json:"salary"`
	JobPostQualityScore int       `json:"job_post_quality_score"`
	JobPostQualityRank  string    `json:"job_post_quality_rank"`
	JobPostMissing      []string  `json:"job_post_missing_fields,omitempty"`
	Language            string    `json:"language"`
	SummaryIT           string    `json:"summary_it"`
	OriginalLinks       []string  `json:"original_links,omitempty"`
	OriginalImages      []string  `json:"original_images,omitempty"`
	Confidence          float64   `json:"confidence"`
	CreatedAt           time.Time `json:"created_at"`
}

type ProcessingError struct {
	SchemaVersion string    `json:"schema_version"`
	Stage         string    `json:"stage"`
	ItemID        string    `json:"item_id"`
	Topic         string    `json:"topic"`
	ErrorCode     string    `json:"error_code"`
	ErrorMessage  string    `json:"error_message"`
	Retryable     bool      `json:"retryable"`
	Attempt       int       `json:"attempt"`
	CreatedAt     time.Time `json:"created_at"`
}

type ProcessedReceipt struct {
	SchemaVersion string    `json:"schema_version"`
	ItemID        string    `json:"item_id"`
	GUID          string    `json:"guid,omitempty"`
	URL           string    `json:"url"`
	Title         string    `json:"title"`
	Destination   string    `json:"destination"`
	ProcessedAt   time.Time `json:"processed_at"`
}
