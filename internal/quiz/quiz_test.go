package quiz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/quantamsage/ai-video-summarizer/internal/ai/aitest"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

func raw(t, prompt string, opts ...string) RawQuestion {
	r := RawQuestion{Type: t, Prompt: prompt, Explanation: "because"}
	for _, o := range opts {
		correct := strings.HasPrefix(o, "*")
		r.Options = append(r.Options, struct {
			Text        string `json:"text"`
			IsCorrect   bool   `json:"is_correct"`
			Explanation string `json:"explanation"`
		}{Text: strings.TrimPrefix(o, "*"), IsCorrect: correct, Explanation: "x"})
	}
	return r
}

func TestValidateQuestion(t *testing.T) {
	cases := []struct {
		name string
		q    RawQuestion
		ok   bool
	}{
		{"single ok", raw(model.SingleChoice, "Which keyword starts a goroutine?", "*go", "run", "spawn", "thread"), true},
		{"single two correct", raw(model.SingleChoice, "Q?", "*a", "*b", "c", "d"), false},
		{"single no correct", raw(model.SingleChoice, "Q?", "a", "b", "c"), false},
		{"single too few", raw(model.SingleChoice, "Q?", "*a", "b"), false},
		{"duplicate options", raw(model.SingleChoice, "Q?", "*a", "A", "c"), false},
		{"catch-all", raw(model.SingleChoice, "Q?", "*a", "b", "All of the above"), false},
		{"multi ok", raw(model.MultipleAnswer, "Pick two?", "*a", "*b", "c", "d"), true},
		{"multi one correct", raw(model.MultipleAnswer, "Pick?", "*a", "b", "c"), false},
		{"multi all correct", raw(model.MultipleAnswer, "Pick?", "*a", "*b", "*c"), false},
		{"tf ok", raw(model.TrueFalse, "Channels are typed.", "false", "*TRUE"), true},
		{"tf bad labels", raw(model.TrueFalse, "Channels are typed.", "*Yes", "No"), false},
		{"empty prompt", raw(model.SingleChoice, " ", "*a", "b", "c"), false},
		{"unknown type", raw("essay", "Q?"), false},
		{"reveals answer", raw(model.SingleChoice, "Since a buffered channel blocks only when full, when does it block?",
			"*a buffered channel blocks only when full", "it never blocks at all", "on every single send", "only when it is empty"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := ValidateQuestion(c.q)
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v err=%v", c.ok, err)
			}
			if c.ok && q.Type == model.TrueFalse && (q.Options[0].Text != "True" || !q.Options[0].IsCorrect) {
				t.Fatalf("true/false not normalized: %+v", q.Options)
			}
		})
	}

	sa := RawQuestion{Type: model.ShortAnswer, Prompt: "Name the keyword", Explanation: "e", AcceptedAnswers: []string{"go", " Go ", ""}}
	q, err := ValidateQuestion(sa)
	if err != nil || len(q.AcceptedAnswers) != 1 || q.Options != nil {
		t.Fatalf("short answer: %+v %v", q, err)
	}
	sa.AcceptedAnswers = nil
	if _, err := ValidateQuestion(sa); err == nil {
		t.Fatal("short answer without accepted answers must fail")
	}
}

func TestSanitizeDropsDuplicatesAndDisallowedTypes(t *testing.T) {
	ts := 30.0
	bad := 9999.0
	q1 := raw(model.SingleChoice, "What does the select statement do in Go?", "*waits", "b1", "c1", "d1")
	q1.TimestampSeconds = &ts
	q2 := raw(model.SingleChoice, "In Go, what does the select statement do?", "*waits", "b2", "c2", "d2")
	q3 := raw(model.TrueFalse, "Goroutines are threads.", "True", "*False")
	q4 := raw(model.SingleChoice, "Which package provides WaitGroup?", "*sync", "os", "io", "fmt")
	q4.TimestampSeconds = &bad
	out, rejected := Sanitize([]RawQuestion{q1, q2, q3, q4}, []string{model.SingleChoice}, 600, true)
	if len(out) != 2 || rejected != 2 {
		t.Fatalf("got %d questions, %d rejected", len(out), rejected)
	}
	if out[0].Timestamp == nil || *out[0].Timestamp != 30 {
		t.Fatal("valid timestamp should be kept")
	}
	if out[1].Timestamp != nil || out[1].Position != 1 {
		t.Fatalf("out-of-range timestamp should be dropped: %+v", out[1])
	}
}

const genResponse = `{"questions":[
 {"type":"single_choice","prompt":"Which keyword starts a goroutine?","options":[{"text":"go","is_correct":true,"explanation":"Stated in video"},{"text":"run","is_correct":false,"explanation":"no"},{"text":"spawn","is_correct":false,"explanation":"no"},{"text":"async","is_correct":false,"explanation":"no"}],"accepted_answers":[],"explanation":"The video says go starts one.","timestamp_seconds":12,"section_title":"Goroutines"},
 {"type":"single_choice","prompt":"Broken question","options":[{"text":"a","is_correct":true,"explanation":""},{"text":"b","is_correct":true,"explanation":""},{"text":"c","is_correct":false,"explanation":""}],"accepted_answers":[],"explanation":"x","timestamp_seconds":null,"section_title":""},
 {"type":"short_answer","prompt":"What type connects goroutines?","options":[],"accepted_answers":["channel","chan"],"explanation":"Channels connect goroutines.","timestamp_seconds":40,"section_title":"Channels"},
 {"type":"true_false","prompt":"Unbuffered sends always succeed immediately.","options":[{"text":"True","is_correct":false,"explanation":"no"},{"text":"False","is_correct":true,"explanation":"they block"}],"accepted_answers":[],"explanation":"They block until received.","timestamp_seconds":50,"section_title":"Channels"}
],"shortfall_reason":""}`

func material() Material {
	return Material{
		Title: "Go concurrency", Duration: 120,
		Segments: []model.Segment{{Start: 0, End: 20, Text: "use go to start a goroutine"}, {Start: 20, End: 60, Text: "channels connect goroutines and unbuffered sends block"}},
	}
}

func TestGenerateValidatesAndVerifies(t *testing.T) {
	mock := (&aitest.Mock{}).
		On("Review these quiz questions", `{"results":[{"index":0,"valid":true,"reason":""},{"index":1,"valid":false,"reason":"unsupported"},{"index":2,"valid":true,"reason":""}]}`).
		On("Write 4 questions", genResponse)
	g := &Generator{AI: mock}
	q, err := g.Generate(context.Background(), model.QuizConfig{Count: 4, Difficulty: "Medium",
		Types: []string{model.SingleChoice, model.ShortAnswer, model.TrueFalse}, Section: -1}, material())
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Questions) != 2 {
		t.Fatalf("expected 2 questions after validation and verification, got %d", len(q.Questions))
	}
	if q.ShortfallReason == "" || !strings.Contains(q.ShortfallReason, "discarded") {
		t.Fatalf("shortfall should be explained: %q", q.ShortfallReason)
	}
	if q.Difficulty != "medium" || q.RequestedCount != 4 || q.Scope != "full" {
		t.Fatalf("quiz metadata: %+v", q)
	}
	if q.Questions[1].Type != model.TrueFalse {
		t.Fatalf("verifier rejection should remove the short answer: %+v", q.Questions[1])
	}
}

func TestGenerateNoQuestionsWithReason(t *testing.T) {
	mock := (&aitest.Mock{}).On("Write 5 questions", `{"questions":[],"shortfall_reason":"The video is only an intro jingle."}`)
	g := &Generator{AI: mock}
	_, err := g.Generate(context.Background(), model.QuizConfig{Count: 5, Difficulty: "easy",
		Types: []string{model.SingleChoice}, Section: -1}, material())
	if !errors.Is(err, ErrNoQuestions) || !strings.Contains(err.Error(), "intro jingle") {
		t.Fatalf("expected ErrNoQuestions with reason, got %v", err)
	}
}

func TestGenerateMalformedResponse(t *testing.T) {
	mock := (&aitest.Mock{}).On("Write 3 questions", `{"questions": "nope"`)
	g := &Generator{AI: mock}
	_, err := g.Generate(context.Background(), model.QuizConfig{Count: 3, Difficulty: "easy",
		Types: []string{model.SingleChoice}, Section: -1}, material())
	if err == nil {
		t.Fatal("expected error for malformed AI response")
	}
}

func TestSectionScopedQuizUsesWindow(t *testing.T) {
	start, end := 20.0, 60.0
	m := material()
	m.Section = &model.Section{Position: 1, Title: "Channels", Start: &start, End: &end}
	mock := (&aitest.Mock{}).
		On("Review these", `{"results":[{"index":0,"valid":true,"reason":""}]}`).
		On("Write 1 questions", genResponse)
	g := &Generator{AI: mock}
	q, err := g.Generate(context.Background(), model.QuizConfig{Count: 1, Difficulty: "hard",
		Types: []string{model.SingleChoice}, Section: 1}, m)
	if err != nil {
		t.Fatal(err)
	}
	prompt := mock.Calls[0].Prompt
	if strings.Contains(prompt, "use go to start") || !strings.Contains(prompt, `section "Channels"`) {
		t.Fatalf("section quiz should only include the section's transcript")
	}
	if q.Scope != "1" || !strings.Contains(q.Title, "Channels") {
		t.Fatalf("scope/title: %+v", q)
	}
}

func TestValidateConfig(t *testing.T) {
	bad := []model.QuizConfig{
		{Count: 0, Difficulty: "easy", Types: []string{model.SingleChoice}},
		{Count: 31, Difficulty: "easy", Types: []string{model.SingleChoice}},
		{Count: 5, Difficulty: "extreme", Types: []string{model.SingleChoice}},
		{Count: 5, Difficulty: "easy"},
		{Count: 5, Difficulty: "easy", Types: []string{"essay"}},
	}
	for i, c := range bad {
		if err := ValidateConfig(&c); err == nil {
			t.Errorf("case %d should be invalid", i)
		}
	}
	ok := model.QuizConfig{Count: 5, Difficulty: " HARD ", Types: []string{model.TrueFalse, model.TrueFalse}}
	if err := ValidateConfig(&ok); err != nil || ok.Difficulty != "hard" || len(ok.Types) != 1 {
		t.Fatalf("normalize: %+v %v", ok, err)
	}
}

func gradingQuiz() *model.Quiz {
	return &model.Quiz{ID: 1, Questions: []model.Question{
		{ID: 10, Type: model.SingleChoice, Options: []model.Option{{ID: 100, IsCorrect: true}, {ID: 101}, {ID: 102}}},
		{ID: 11, Type: model.MultipleAnswer, Options: []model.Option{{ID: 110, IsCorrect: true}, {ID: 111, IsCorrect: true}, {ID: 112}}},
		{ID: 12, Type: model.TrueFalse, Options: []model.Option{{ID: 120}, {ID: 121, IsCorrect: true}}},
		{ID: 13, Type: model.ShortAnswer, Prompt: "Keyword?", AcceptedAnswers: []string{"The goroutine"}},
		{ID: 14, Type: model.ShortAnswer, Prompt: "How many?", AcceptedAnswers: []string{"3"}},
		{ID: 15, Type: model.ShortAnswer, Prompt: "Concept?", AcceptedAnswers: []string{"mutual exclusion"}},
	}}
}

func TestGradeDeterministic(t *testing.T) {
	g := &Grader{}
	subs := map[int64]Submission{
		10: {OptionIDs: []int64{100}},
		11: {OptionIDs: []int64{111, 110, 110}},
		12: {OptionIDs: []int64{120}},
		13: {Text: "  goroutine! "},
		14: {Text: "3.0"},
		15: {Text: "mutual exclusoin"},
	}
	a := g.Grade(context.Background(), gradingQuiz(), subs)
	want := []bool{true, true, false, true, true, true}
	for i, ans := range a.Answers {
		if ans.IsCorrect != want[i] {
			t.Errorf("question %d: got %v want %v (method %s)", i, ans.IsCorrect, want[i], ans.GradingMethod)
		}
	}
	if a.Answers[5].GradingMethod != MethodFuzzy {
		t.Errorf("typo should be graded fuzzy, got %s", a.Answers[5].GradingMethod)
	}
	if a.CorrectCount != 5 || a.TotalCount != 6 || a.Score != 83.3 {
		t.Errorf("score: %+v", a)
	}
}

func TestGradeRejectsForeignAndExtraOptions(t *testing.T) {
	g := &Grader{}
	q := gradingQuiz()
	a := g.Grade(context.Background(), q, map[int64]Submission{
		10: {OptionIDs: []int64{100, 101}}, // two picks on single choice
		11: {OptionIDs: []int64{110, 111, 999}},
	})
	if a.Answers[0].IsCorrect {
		t.Error("multiple selections on single choice must be incorrect")
	}
	if !a.Answers[1].IsCorrect {
		t.Error("foreign option ids should be ignored")
	}
	if a.Answers[2].GradingMethod != MethodNone {
		t.Error("unanswered question should be marked unanswered")
	}
}

func TestGradeShortAnswerUsesAIOnlyWhenNeeded(t *testing.T) {
	mock := (&aitest.Mock{}).On("Learner's answer", `{"equivalent": true, "feedback": "Same idea."}`)
	g := &Grader{AI: mock}
	q := &model.Quiz{Questions: []model.Question{
		{ID: 1, Type: model.ShortAnswer, Prompt: "Why use a mutex?", AcceptedAnswers: []string{"to prevent data races"}},
		{ID: 2, Type: model.ShortAnswer, Prompt: "Keyword?", AcceptedAnswers: []string{"go"}},
	}}
	a := g.Grade(context.Background(), q, map[int64]Submission{
		1: {Text: "so two goroutines don't modify shared memory at once"},
		2: {Text: "GO"},
	})
	if !a.Answers[0].IsCorrect || a.Answers[0].GradingMethod != MethodAI || a.Answers[0].Feedback == "" {
		t.Fatalf("semantic grading: %+v", a.Answers[0])
	}
	if a.Answers[1].GradingMethod != MethodExact {
		t.Fatalf("exact match should not use AI: %+v", a.Answers[1])
	}
	if mock.CallCount("Learner's answer") != 1 {
		t.Fatalf("AI should be called once, got %d", mock.CallCount("Learner's answer"))
	}
	if !strings.Contains(mock.Calls[0].Prompt, "to prevent data races") {
		t.Fatal("answer key must be passed to the judge unchanged")
	}
	if q.Questions[0].AcceptedAnswers[0] != "to prevent data races" {
		t.Fatal("answer key was modified")
	}
}

func TestGradeShortAnswerAIFailureFallsBack(t *testing.T) {
	mock := (&aitest.Mock{}).On("Learner's answer", `garbage`)
	g := &Grader{AI: mock}
	q := &model.Quiz{Questions: []model.Question{
		{ID: 1, Type: model.ShortAnswer, Prompt: "Why?", AcceptedAnswers: []string{"because of races"}},
	}}
	a := g.Grade(context.Background(), q, map[int64]Submission{1: {Text: "something different"}})
	if a.Answers[0].IsCorrect || a.Answers[0].Feedback == "" {
		t.Fatalf("AI failure should mark incorrect with an explanation: %+v", a.Answers[0])
	}
}

func TestNormalizeAnswer(t *testing.T) {
	cases := map[string]string{
		"  The Goroutine! ": "goroutine",
		"C++":               "c++",
		"3.14.":             "3.14",
		"a":                 "a",
		"an  apple, pie":    "apple pie",
	}
	for in, want := range cases {
		if got := NormalizeAnswer(in); got != want {
			t.Errorf("NormalizeAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}
