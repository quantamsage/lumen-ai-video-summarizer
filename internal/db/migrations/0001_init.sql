-- Users and authentication -------------------------------------------------

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Session tokens are stored as SHA-256 hashes so a database leak does not
-- expose usable session cookies.
CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at   TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    user_agent   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE password_resets (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP
);
CREATE INDEX idx_password_resets_user ON password_resets(user_id);

-- Videos -------------------------------------------------------------------

CREATE TABLE videos (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_type      TEXT    NOT NULL CHECK (source_type IN ('upload', 'url')),
    source_url       TEXT    NOT NULL DEFAULT '',
    title            TEXT    NOT NULL,
    description      TEXT    NOT NULL DEFAULT '',
    original_filename TEXT   NOT NULL DEFAULT '',
    -- storage_key is a server-generated relative path inside UPLOAD_DIR; it is
    -- never derived from user input.
    storage_key      TEXT    NOT NULL DEFAULT '',
    caption_key      TEXT    NOT NULL DEFAULT '',
    thumbnail_key    TEXT    NOT NULL DEFAULT '',
    thumbnail_url    TEXT    NOT NULL DEFAULT '',
    mime_type        TEXT    NOT NULL DEFAULT '',
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    duration_seconds REAL    NOT NULL DEFAULT 0,
    width            INTEGER NOT NULL DEFAULT 0,
    height           INTEGER NOT NULL DEFAULT 0,
    -- status of the overall processing pipeline
    status           TEXT    NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'processing', 'ready', 'failed')),
    summary_status   TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (summary_status IN ('pending', 'processing', 'ready', 'failed')),
    quiz_status      TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (quiz_status IN ('pending', 'processing', 'ready', 'failed', 'skipped')),
    error_message    TEXT    NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_videos_user_created ON videos(user_id, created_at DESC);
CREATE INDEX idx_videos_user_status ON videos(user_id, status);

-- Background processing jobs ----------------------------------------------

CREATE TABLE processing_jobs (
    id            INTEGER PRIMARY KEY,
    video_id      INTEGER NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL CHECK (kind IN ('process_video', 'generate_quiz')),
    -- JSON payload, e.g. quiz configuration for generate_quiz jobs
    payload       TEXT    NOT NULL DEFAULT '{}',
    status        TEXT    NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    stage         TEXT    NOT NULL DEFAULT '',
    progress      INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    run_after     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    error_message TEXT    NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at    TIMESTAMP,
    finished_at   TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_jobs_claim ON processing_jobs(status, run_after);
CREATE INDEX idx_jobs_video ON processing_jobs(video_id);
-- Protection against duplicate processing: at most one active job of each
-- kind per video.
CREATE UNIQUE INDEX idx_jobs_active_unique ON processing_jobs(video_id, kind)
    WHERE status IN ('queued', 'running');

-- Transcripts --------------------------------------------------------------

CREATE TABLE transcripts (
    id             INTEGER PRIMARY KEY,
    video_id       INTEGER NOT NULL UNIQUE REFERENCES videos(id) ON DELETE CASCADE,
    source         TEXT    NOT NULL CHECK (source IN ('captions', 'speech_to_text')),
    language       TEXT    NOT NULL DEFAULT '',
    has_timestamps INTEGER NOT NULL DEFAULT 0,
    full_text      TEXT    NOT NULL,
    word_count     INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE transcript_segments (
    id             INTEGER PRIMARY KEY,
    transcript_id  INTEGER NOT NULL REFERENCES transcripts(id) ON DELETE CASCADE,
    position       INTEGER NOT NULL,
    start_seconds  REAL    NOT NULL DEFAULT 0,
    end_seconds    REAL    NOT NULL DEFAULT 0,
    text           TEXT    NOT NULL,
    UNIQUE (transcript_id, position)
);

-- Summaries ----------------------------------------------------------------

CREATE TABLE summaries (
    id            INTEGER PRIMARY KEY,
    video_id      INTEGER NOT NULL UNIQUE REFERENCES videos(id) ON DELETE CASCADE,
    overview      TEXT    NOT NULL,
    detailed      TEXT    NOT NULL,
    -- JSON array of strings
    key_takeaways TEXT    NOT NULL DEFAULT '[]',
    -- JSON array of {note, is_external} objects
    learning_notes TEXT   NOT NULL DEFAULT '[]',
    provider      TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Topic breakdown: logical sections of the video.
CREATE TABLE summary_sections (
    id            INTEGER PRIMARY KEY,
    summary_id    INTEGER NOT NULL REFERENCES summaries(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    title         TEXT    NOT NULL,
    content       TEXT    NOT NULL,
    start_seconds REAL,
    end_seconds   REAL,
    UNIQUE (summary_id, position)
);

CREATE TABLE concepts (
    id          INTEGER PRIMARY KEY,
    summary_id  INTEGER NOT NULL REFERENCES summaries(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    term        TEXT    NOT NULL,
    definition  TEXT    NOT NULL,
    category    TEXT    NOT NULL DEFAULT 'concept',
    UNIQUE (summary_id, position)
);

-- Quizzes ------------------------------------------------------------------

CREATE TABLE quizzes (
    id                 INTEGER PRIMARY KEY,
    video_id           INTEGER NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    title              TEXT    NOT NULL,
    difficulty         TEXT    NOT NULL CHECK (difficulty IN ('easy', 'medium', 'hard')),
    -- 'full' or the summary_sections.position the quiz was scoped to
    scope              TEXT    NOT NULL DEFAULT 'full',
    requested_count    INTEGER NOT NULL,
    -- explanation when fewer questions than requested were generated
    shortfall_reason   TEXT    NOT NULL DEFAULT '',
    created_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_quizzes_video ON quizzes(video_id, created_at DESC);

CREATE TABLE quiz_questions (
    id                 INTEGER PRIMARY KEY,
    quiz_id            INTEGER NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
    position           INTEGER NOT NULL,
    type               TEXT    NOT NULL
                       CHECK (type IN ('single_choice', 'multiple_answer', 'true_false', 'short_answer')),
    prompt             TEXT    NOT NULL,
    explanation        TEXT    NOT NULL,
    -- short-answer: JSON array of accepted answers (first is canonical)
    accepted_answers   TEXT    NOT NULL DEFAULT '[]',
    timestamp_seconds  REAL,
    section_title      TEXT    NOT NULL DEFAULT '',
    UNIQUE (quiz_id, position)
);

CREATE TABLE quiz_options (
    id           INTEGER PRIMARY KEY,
    question_id  INTEGER NOT NULL REFERENCES quiz_questions(id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    text         TEXT    NOT NULL,
    is_correct   INTEGER NOT NULL DEFAULT 0,
    -- why this option is right or wrong
    explanation  TEXT    NOT NULL DEFAULT '',
    UNIQUE (question_id, position)
);

CREATE TABLE quiz_attempts (
    id              INTEGER PRIMARY KEY,
    quiz_id         INTEGER NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    score           REAL    NOT NULL DEFAULT 0,
    correct_count   INTEGER NOT NULL DEFAULT 0,
    total_count     INTEGER NOT NULL DEFAULT 0,
    started_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    TIMESTAMP
);
CREATE INDEX idx_attempts_user ON quiz_attempts(user_id, completed_at DESC);
CREATE INDEX idx_attempts_quiz ON quiz_attempts(quiz_id);

CREATE TABLE quiz_answers (
    id               INTEGER PRIMARY KEY,
    attempt_id       INTEGER NOT NULL REFERENCES quiz_attempts(id) ON DELETE CASCADE,
    question_id      INTEGER NOT NULL REFERENCES quiz_questions(id) ON DELETE CASCADE,
    -- JSON array of selected option ids (choice questions)
    selected_options TEXT    NOT NULL DEFAULT '[]',
    text_answer      TEXT    NOT NULL DEFAULT '',
    is_correct       INTEGER NOT NULL DEFAULT 0,
    -- how the answer was graded: exact, fuzzy, ai, or choice
    grading_method   TEXT    NOT NULL DEFAULT '',
    feedback         TEXT    NOT NULL DEFAULT '',
    UNIQUE (attempt_id, question_id)
);
