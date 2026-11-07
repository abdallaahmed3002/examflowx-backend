package main

import (
	"encoding/json"
	"testing"
)

func TestParseQuestionsLocally_ArabicMCQ(t *testing.T) {
	sample := `
1. ما هي عاصمة جمهورية مصر العربية؟
أ) الإسكندرية
*ب) القاهرة
ج) الجيزة
د) أسوان

2. تدور الأرض حول الشمس.
(صح)

3. تتكون جزيئة الماء من ذرتي هيدروجين وذرة _____
الإجابة: أكسجين

4. اشرح أسباب التغير المناخي.
`
	qs := parseQuestionsLocally(sample)
	if len(qs) != 4 {
		t.Fatalf("expected 4 questions, got %d", len(qs))
	}

	// Q1: MCQ single
	q1 := qs[0]
	if q1.Kind != "single" {
		t.Errorf("q1 kind: expected single, got %s", q1.Kind)
	}
	if q1.CorrectAnswer != "القاهرة" {
		t.Errorf("q1 correct: expected القاهرة, got %s", q1.CorrectAnswer)
	}
	var opts []string
	_ = json.Unmarshal(q1.Options, &opts)
	if len(opts) != 4 {
		t.Errorf("q1 options count: expected 4, got %d", len(opts))
	}

	// Q2: True/False
	q2 := qs[1]
	if q2.Kind != "truefalse" {
		t.Errorf("q2 kind: expected truefalse, got %s", q2.Kind)
	}
	if q2.CorrectAnswer != "True" {
		t.Errorf("q2 correct: expected True, got %s", q2.CorrectAnswer)
	}

	// Q3: Fill
	q3 := qs[2]
	if q3.Kind != "fill" {
		t.Errorf("q3 kind: expected fill, got %s", q3.Kind)
	}
	if q3.CorrectAnswer != "أكسجين" {
		t.Errorf("q3 correct: expected أكسجين, got %s", q3.CorrectAnswer)
	}

	// Q4: Essay
	q4 := qs[3]
	if q4.Kind != "essay" {
		t.Errorf("q4 kind: expected essay, got %s", q4.Kind)
	}
}

func TestParseQuestionsLocally_WithAnswerKey(t *testing.T) {
	sample := `
س1: ما هو الكوكب الأحمر؟
- المريخ
- الزهرة
- المشتري

س2: تشرق الشمس من الغرب.
صح أو خطأ

مفتاح الإجابات:
1: أ
2: خطأ
`
	qs := parseQuestionsLocally(sample)
	if len(qs) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(qs))
	}

	if qs[0].CorrectAnswer != "المريخ" {
		t.Errorf("expected q1 correct to be المريخ, got %s", qs[0].CorrectAnswer)
	}
	if qs[1].CorrectAnswer != "False" {
		t.Errorf("expected q2 correct to be False, got %s", qs[1].CorrectAnswer)
	}
}

func TestCleanAndNormalizeQuestions(t *testing.T) {
	jsonResponse := "```json\n" + `[
		{
			"kind": "single",
			"prompt": "1. What is 2 + 2?",
			"options": ["3", "4", "5"],
			"correctAnswer": "4",
			"points": 2
		}
	]` + "\n```"

	qs, err := cleanAndNormalizeQuestions(jsonResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("expected 1 question, got %d", len(qs))
	}
	if qs[0].Prompt != "What is 2 + 2?" {
		t.Errorf("expected clean prompt, got %s", qs[0].Prompt)
	}
	if qs[0].Points != 2 {
		t.Errorf("expected points 2, got %d", qs[0].Points)
	}
}

