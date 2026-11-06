package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// gradeSQL grades an attempt exclusively from the immutable attempt_questions
// snapshot, joined with stored answers and manual grading records. The second
// value is the total available points.
const gradeSQL = `
SELECT coalesce(sum(
  CASE
    WHEN aq.kind='essay' THEN coalesce(g.points,0)
    WHEN aq.kind IN ('single','truefalse','image') THEN
      CASE WHEN a.value IS NOT NULL AND lower(trim(a.value))=lower(trim(aq.correct_answer)) THEN aq.points ELSE 0 END
    WHEN aq.kind='short' THEN
      CASE WHEN a.value IS NOT NULL
        AND lower(trim(regexp_replace(a.value,'\s+',' ','g')))=lower(trim(regexp_replace(aq.correct_answer,'\s+',' ','g')))
        THEN aq.points ELSE 0 END
    WHEN aq.kind='fill' THEN
      CASE WHEN a.value IS NOT NULL AND EXISTS (
        SELECT 1 FROM unnest(string_to_array(aq.correct_answer,'|')) alt
        WHERE trim(alt)<>'' AND lower(trim(alt))=lower(trim(a.value)))
        THEN aq.points ELSE 0 END
    WHEN aq.kind='numeric' THEN
      CASE WHEN a.value ~ '^\s*-?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][-+]?[0-9]+)?\s*$'
        AND aq.correct_answer ~ '^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][-+]?[0-9]+)?$'
        AND round(btrim(a.value)::numeric,4)=round(aq.correct_answer::numeric,4)
        THEN aq.points ELSE 0 END
    WHEN aq.kind='multi' AND a.value ~ '^\s*\[' THEN
      CASE WHEN (
        SELECT coalesce(jsonb_agg(el ORDER BY el),'[]'::jsonb)
        FROM jsonb_array_elements_text(a.value::jsonb) el
      ) = (
        SELECT coalesce(jsonb_agg(el ORDER BY el),'[]'::jsonb)
        FROM jsonb_array_elements_text(aq.correct_answer::jsonb) el
      ) THEN aq.points ELSE 0 END
    WHEN aq.kind='ordering' AND a.value ~ '^\s*\[' THEN
      CASE WHEN (
        SELECT coalesce(jsonb_agg(el ORDER BY idx),'[]'::jsonb)
        FROM jsonb_array_elements_text(a.value::jsonb) WITH ORDINALITY AS t(el, idx)
      ) = (
        SELECT coalesce(jsonb_agg(el ORDER BY idx),'[]'::jsonb)
        FROM jsonb_array_elements_text(aq.correct_answer::jsonb) WITH ORDINALITY AS t(el, idx)
      ) THEN aq.points ELSE 0 END
    WHEN aq.kind='matching' AND a.value ~ '^\s*\{' THEN
      CASE WHEN a.value::jsonb = aq.correct_answer::jsonb THEN aq.points ELSE 0 END
    ELSE 0 END
),0), coalesce(sum(aq.points),0)
FROM attempt_questions aq
LEFT JOIN answers a ON a.attempt_id=aq.attempt_id AND a.question_id=aq.question_id
LEFT JOIN grading_records g ON g.attempt_id=aq.attempt_id AND g.question_id=aq.question_id
WHERE aq.attempt_id=$1`

var questionKinds = map[string]bool{
	"single": true, "multi": true, "truefalse": true, "short": true,
	"fill": true, "numeric": true, "essay": true, "matching": true,
	"ordering": true, "image": true,
}

// answerLimit caps stored answer sizes per kind.
var answerLimit = map[string]int{
	"single": 500, "multi": 4000, "truefalse": 10, "short": 1000,
	"fill": 1000, "numeric": 50, "essay": 20000, "matching": 4000,
	"ordering": 4000, "image": 500,
}

func decodeOptions(raw json.RawMessage) ([]string, map[string]any, error) {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("options must be an array or object")
	}
	return nil, obj, nil
}

func validateAnswer(kind, value string, options json.RawMessage, correct string) error {
	if len(value) > answerLimit[kind] {
		return fmt.Errorf("answer too long")
	}
	opts, obj, err := decodeOptions(options)
	if err != nil {
		return err
	}
	switch kind {
	case "single", "image":
		if value == "" { // unanswered is persisted as empty
			return nil
		}
		for _, o := range opts {
			if value == o {
				return nil
			}
		}
		return fmt.Errorf("answer is not one of the options")
	case "truefalse":
		if value == "" || value == "True" || value == "False" {
			return nil
		}
		return fmt.Errorf("answer must be True or False")
	case "multi":
		if value == "" {
			return nil
		}
		var sel []string
		if json.Unmarshal([]byte(value), &sel) != nil {
			return fmt.Errorf("answer must be a JSON array")
		}
		for _, s := range sel {
			found := false
			for _, o := range opts {
				if s == o {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("answer contains an unknown option")
			}
		}
		return nil
	case "ordering":
		if value == "" {
			return nil
		}
		var sel []string
		if json.Unmarshal([]byte(value), &sel) != nil || len(sel) != len(opts) {
			return fmt.Errorf("answer must include every item exactly once")
		}
		want := map[string]int{}
		for _, o := range opts {
			want[o]++
		}
		for _, s := range sel {
			if want[s] == 0 {
				return fmt.Errorf("answer contains an unknown item")
			}
			want[s]--
		}
		return nil
	case "matching":
		if value == "" {
			return nil
		}
		var sel map[string]string
		if json.Unmarshal([]byte(value), &sel) != nil || obj == nil {
			return fmt.Errorf("answer must be a mapping object")
		}
		left, okL := obj["left"].([]any)
		right, okR := obj["right"].([]any)
		if !okL || !okR {
			return fmt.Errorf("invalid matching options")
		}
		validRight := map[string]bool{}
		for _, r := range right {
			validRight[fmt.Sprint(r)] = true
		}
		validLeft := map[string]bool{}
		for _, l := range left {
			validLeft[fmt.Sprint(l)] = true
		}
		for k, v := range sel {
			if !validLeft[k] || !validRight[v] {
				return fmt.Errorf("answer contains an unknown pair")
			}
		}
		return nil
	case "numeric":
		if value == "" {
			return nil
		}
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
			return fmt.Errorf("answer must be a number")
		}
		return nil
	case "short", "fill", "essay":
		return nil
	}
	return fmt.Errorf("unknown question kind")
}

// validateQuestionInput checks teacher-provided question payloads per kind.
func validateQuestionInput(p questionInput) error {
	if strings.TrimSpace(p.Prompt) == "" || len(p.Prompt) > 10000 || p.Points < 1 || p.Points > 1000 {
		return fmt.Errorf("prompt and points (1–1000) required")
	}
	if !questionKinds[p.Kind] {
		return fmt.Errorf("unsupported question kind")
	}
	if len(p.ImageURL) > 2048 {
		return fmt.Errorf("image URL too long")
	}
	if p.ImageURL != "" && !strings.HasPrefix(p.ImageURL, "/uploads/") && !strings.HasPrefix(p.ImageURL, "https://") {
		return fmt.Errorf("image URL must be an uploaded path or an https URL")
	}
	if p.Kind == "image" && p.ImageURL == "" {
		return fmt.Errorf("image URL required for image questions")
	}
	if p.Kind != "image" && p.ImageURL != "" && !strings.HasPrefix(p.ImageURL, "/uploads/") && !strings.HasPrefix(p.ImageURL, "https://") {
		return fmt.Errorf("invalid image URL")
	}
	opts, obj, err := decodeOptions(p.Options)
	if err != nil {
		return err
	}
	switch p.Kind {
	case "single", "image":
		if len(opts) < 2 || len(opts) > 8 {
			return fmt.Errorf("provide 2–8 options")
		}
		for _, o := range opts {
			if strings.TrimSpace(o) == "" {
				return fmt.Errorf("options cannot be empty")
			}
			if o == p.CorrectAnswer {
				return nil
			}
		}
		return fmt.Errorf("correct answer must match an option")
	case "multi":
		if len(opts) < 2 || len(opts) > 8 {
			return fmt.Errorf("provide 2–8 options")
		}
		var correct []string
		if json.Unmarshal([]byte(p.CorrectAnswer), &correct) != nil || len(correct) == 0 {
			return fmt.Errorf("correct answers must be a non-empty JSON array")
		}
		seen := map[string]bool{}
		for _, c := range correct {
			found := false
			for _, o := range opts {
				if c == o {
					found = true
				}
			}
			if !found || seen[c] {
				return fmt.Errorf("correct answers must be unique options")
			}
			seen[c] = true
		}
		return nil
	case "truefalse":
		if p.CorrectAnswer != "True" && p.CorrectAnswer != "False" {
			return fmt.Errorf("correct answer must be True or False")
		}
		return nil
	case "short":
		if strings.TrimSpace(p.CorrectAnswer) == "" || len(p.CorrectAnswer) > 1000 {
			return fmt.Errorf("expected answer required (max 1000 chars)")
		}
		return nil
	case "fill":
		if strings.TrimSpace(p.CorrectAnswer) == "" || len(p.CorrectAnswer) > 2000 {
			return fmt.Errorf("expected answer required; separate alternatives with |")
		}
		for _, alt := range strings.Split(p.CorrectAnswer, "|") {
			if len(alt) > 1000 {
				return fmt.Errorf("answer alternative too long")
			}
		}
		return nil
	case "numeric":
		f, err := strconv.ParseFloat(strings.TrimSpace(p.CorrectAnswer), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("correct answer must be a number")
		}
		if len(strings.TrimSpace(p.CorrectAnswer)) > 40 {
			return fmt.Errorf("numeric answer too long")
		}
		return nil
	case "essay":
		if p.CorrectAnswer != "" && len(p.CorrectAnswer) > 10000 {
			return fmt.Errorf("rubric too long")
		}
		return nil
	case "ordering":
		if len(opts) < 2 || len(opts) > 10 {
			return fmt.Errorf("provide 2–10 items in correct order")
		}
		seen := map[string]bool{}
		for _, o := range opts {
			if strings.TrimSpace(o) == "" || seen[o] {
				return fmt.Errorf("items must be non-empty and unique")
			}
			seen[o] = true
		}
		// The correct order is the order the teacher entered the items.
		return nil
	case "matching":
		if obj == nil {
			return fmt.Errorf("options must be {\"left\":[…],\"right\":[…]}")
		}
		left, okL := obj["left"].([]any)
		right, okR := obj["right"].([]any)
		if !okL || !okR || len(left) < 2 || len(left) > 8 || len(right) != len(left) {
			return fmt.Errorf("matching needs 2–8 left items and the same number of right items")
		}
		seenL, seenR := map[string]bool{}, map[string]bool{}
		for _, v := range left {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" || seenL[s] {
				return fmt.Errorf("left items must be non-empty and unique")
			}
			seenL[s] = true
		}
		for _, v := range right {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" || seenR[s] {
				return fmt.Errorf("right items must be non-empty and unique")
			}
			seenR[s] = true
		}
		var correct map[string]string
		if json.Unmarshal([]byte(p.CorrectAnswer), &correct) != nil || len(correct) != len(left) {
			return fmt.Errorf("correct pairs must map every left item once")
		}
		for l, rr := range correct {
			if !seenL[l] || !seenR[rr] {
				return fmt.Errorf("correct pair references an unknown item")
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported question kind")
}

// normalizeCorrect stores canonical correct answers per kind.
func normalizeCorrect(p questionInput, opts json.RawMessage) (string, error) {
	if p.Kind == "ordering" {
		var items []string
		if err := json.Unmarshal(opts, &items); err != nil {
			return "", err
		}
		b, err := json.Marshal(items)
		return string(b), err
	}
	return p.CorrectAnswer, nil
}
