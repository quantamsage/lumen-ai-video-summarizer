// Package model defines the domain types shared by the store, the
// processing pipeline and the web layer.
package model

import "time"

// Video processing states.
const (
	VideoQueued     = "queued"
	VideoProcessing = "processing"
	VideoReady      = "ready"
	VideoFailed     = "failed"

	StagePending    = "pending"
	StageProcessing = "processing"
	StageReady      = "ready"
	StageFailed     = "failed"
	StageSkipped    = "skipped"

	SourceUpload = "upload"
	SourceURL    = "url"
)

// Job kinds and states.
const (
	JobProcessVideo = "process_video"
	JobGenerateQuiz = "generate_quiz"

	JobQueued    = "queued"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
)

// Question types.
const (
	SingleChoice   = "single_choice"
	MultipleAnswer = "multiple_answer"
	TrueFalse      = "true_false"
	ShortAnswer    = "short_answer"
)

// QuestionTypes lists all supported question types in display order.
var QuestionTypes = []string{SingleChoice, MultipleAnswer, TrueFalse, ShortAnswer}

// Difficulties lists the supported quiz difficulties.
var Difficulties = []string{"easy", "medium", "hard"}

type User struct {
	ID           int64
	Email        string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Video struct {
	ID               int64
	UserID           int64
	SourceType       string
	SourceURL        string
	Title            string
	Description      string
	OriginalFilename string
	StorageKey       string
	CaptionKey       string
	ThumbnailKey     string
	ThumbnailURL     string
	MimeType         string
	SizeBytes        int64
	DurationSeconds  float64
	Width            int
	Height           int
	Status           string
	SummaryStatus    string
	QuizStatus       string
	ErrorMessage     string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Playable reports whether the video file is stored locally and can be
// streamed to the browser.
func (v *Video) Playable() bool { return v.StorageKey != "" }

type Job struct {
	ID           int64
	VideoID      int64
	Kind         string
	Payload      string
	Status       string
	Stage        string
	Progress     int
	Attempts     int
	MaxAttempts  int
	RunAfter     time.Time
	ErrorMessage string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

type Transcript struct {
	ID            int64
	VideoID       int64
	Source        string // "captions" or "speech_to_text"
	Language      string
	HasTimestamps bool
	FullText      string
	WordCount     int
	CreatedAt     time.Time
}

// Segment is a timed span of transcript text.
type Segment struct {
	Position int
	Start    float64
	End      float64
	Text     string
}

type Summary struct {
	ID            int64
	VideoID       int64
	Overview      string
	Detailed      string
	KeyTakeaways  []string
	LearningNotes []LearningNote
	Sections      []Section
	Concepts      []Concept
	Provider      string
	Model         string
	CreatedAt     time.Time
}

// LearningNote is supplementary explanation. External notes contain context
// that was not stated in the video itself.
type LearningNote struct {
	Note       string `json:"note"`
	IsExternal bool   `json:"is_external"`
}

// Section is one entry of the topic breakdown.
type Section struct {
	Position int
	Title    string
	Content  string
	Start    *float64
	End      *float64
}

type Concept struct {
	Term       string
	Definition string
	Category   string // term, technique, idea, definition...
}

type Quiz struct {
	ID              int64
	VideoID         int64
	Title           string
	Difficulty      string
	Scope           string
	RequestedCount  int
	ShortfallReason string
	CreatedAt       time.Time
	QuestionCount   int
	Questions       []Question
}

type Question struct {
	ID              int64
	Position        int
	Type            string
	Prompt          string
	Explanation     string
	AcceptedAnswers []string
	Timestamp       *float64
	SectionTitle    string
	Options         []Option
}

// CorrectOptionIDs returns the ids of all correct options.
func (q *Question) CorrectOptionIDs() []int64 {
	var ids []int64
	for _, o := range q.Options {
		if o.IsCorrect {
			ids = append(ids, o.ID)
		}
	}
	return ids
}

type Option struct {
	ID          int64
	Position    int
	Text        string
	IsCorrect   bool
	Explanation string
}

type Attempt struct {
	ID           int64
	QuizID       int64
	UserID       int64
	Score        float64 // percentage 0-100
	CorrectCount int
	TotalCount   int
	StartedAt    time.Time
	CompletedAt  *time.Time
	Answers      []Answer
}

type Answer struct {
	QuestionID      int64
	SelectedOptions []int64
	TextAnswer      string
	IsCorrect       bool
	GradingMethod   string
	Feedback        string
}

// QuizConfig is the user-selected configuration for quiz generation.
type QuizConfig struct {
	Count      int      `json:"count"`
	Difficulty string   `json:"difficulty"`
	Types      []string `json:"types"`
	// Section is the summary section position to scope to, or -1 for the
	// whole video.
	Section int `json:"section"`
}
