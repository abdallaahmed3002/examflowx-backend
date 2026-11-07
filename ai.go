package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type aiParsePayload struct {
	Text     string   `json:"text"`
	Images   []string `json:"images"`
	APIKey   string   `json:"apiKey"`
	Provider string   `json:"provider"` // "gemini", "openai", or ""
}

type aiParseResultQuestion struct {
	Kind          string          `json:"kind"`
	Prompt        string          `json:"prompt"`
	Options       json.RawMessage `json:"options"`
	CorrectAnswer string          `json:"correctAnswer"`
	Points        int             `json:"points"`
	ImageURL      string          `json:"imageUrl"`
}

type aiParseResponse struct {
	Questions []aiParseResultQuestion `json:"questions"`
	Source    string                  `json:"source"`
	Count     int                     `json:"count"`
}

const aiPromptInstructions = `You are an expert exam parser and educational assessment specialist.
Analyze the provided text and/or images (which may contain handwritten, typed, printed worksheets, notes, or test questions in Arabic or English).
Extract every question into a structured JSON array.

Return a JSON object with this exact shape:
{
  "questions": [
    {
      "kind": "single",
      "prompt": "ما هي عاصمة جمهورية مصر العربية؟",
      "options": ["الإسكندرية", "القاهرة", "الجيزة", "أسوان"],
      "correctAnswer": "القاهرة",
      "points": 1,
      "imageUrl": ""
    }
  ]
}

Rules for Question Kinds and Formats:
1. "single": Multiple choice with exactly one correct answer.
   - "options": JSON array of 2 to 8 strings.
   - "correctAnswer": The exact string of the correct option.
2. "multi": Multiple choice with one or more correct answers.
   - "options": JSON array of 2 to 8 strings.
   - "correctAnswer": JSON stringified array of correct option strings, e.g. "[\"opt1\",\"opt2\"]".
3. "truefalse": True or False question.
   - "options": [].
   - "correctAnswer": Exactly "True" or "False".
4. "short": Short answer / recall question.
   - "options": [].
   - "correctAnswer": Model answer string.
5. "fill": Fill in the blank question.
   - "options": [].
   - "correctAnswer": Expected word(s), or alternatives separated by | (e.g. "أكسجين|oxygen").
6. "numeric": Numeric calculation or math answer.
   - "options": [].
   - "correctAnswer": A number as a string (e.g. "42" or "3.14").
7. "essay": Long answer or explanation question.
   - "options": [].
   - "correctAnswer": Rubric / grading criteria or model answer (or empty).
8. "ordering": Items to be arranged in a specific sequence.
   - "options": JSON array of 2 to 10 strings in their CORRECT final order.
   - "correctAnswer": Same JSON stringified array of strings in correct order.
9. "matching": Match left items with right items.
   - "options": {"left": ["A", "B"], "right": ["1", "2"]}.
   - "correctAnswer": JSON stringified object mapping left to right, e.g. "{\"A\":\"1\",\"B\":\"2\"}".
10. "image": Visual question with choices.
   - "options": JSON array of 2 to 8 strings.
   - "correctAnswer": Exact string of correct option.
   - "imageUrl": Image URL if applicable.

Important Rules:
- Detect the correct answer from markers like an asterisk (*), checkmark (✓), bold, underline, circled letter, or an answer key section at the bottom. If no answer is marked, select the most likely correct answer based on educational knowledge, or default to the first option.
- Strip question numbering (like "1.", "س1:", "Q2-") from the prompt text so it is clean.
- Ensure all text retains original Arabic or English wording and mathematical symbols.
- If images were provided, transcribe the questions and choices accurately.
- Return ONLY valid JSON.`

// aiParseQuestions is the HTTP handler for POST /api/v1/ai/parse
func (s *server) aiParseQuestions(w http.ResponseWriter, r *http.Request, teacherID string) {
	var p aiParsePayload
	// Allow up to 25MB request body for base64 images
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, 400, "Invalid request body or payload too large")
		return
	}

	p.Text = strings.TrimSpace(p.Text)
	if p.Text == "" && len(p.Images) == 0 {
		fail(w, 400, "Text or image is required for parsing")
		return
	}

	// Determine API Key and Provider
	apiKey := strings.TrimSpace(p.APIKey)
	provider := strings.ToLower(strings.TrimSpace(p.Provider))

	if apiKey == "" {
		if k := os.Getenv("GEMINI_API_KEY"); k != "" {
			apiKey = k
			if provider == "" {
				provider = "gemini"
			}
		} else if k := os.Getenv("OPENAI_API_KEY"); k != "" {
			apiKey = k
			if provider == "" {
				provider = "openai"
			}
		}
	}

	// If no provider explicitly given, guess by API key format
	if provider == "" && apiKey != "" {
		if strings.HasPrefix(apiKey, "AIza") {
			provider = "gemini"
		} else {
			provider = "openai"
		}
	}

	var questions []aiParseResultQuestion
	var source string
	var err error

	// If we have an API key, try the cloud LLM first
	if apiKey != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
		defer cancel()

		if provider == "gemini" {
			questions, err = parseWithGemini(ctx, apiKey, p.Text, p.Images)
			source = "gemini"
		} else {
			questions, err = parseWithOpenAI(ctx, apiKey, p.Text, p.Images)
			source = "openai"
		}

		if err != nil && len(p.Images) > 0 && p.Text == "" {
			fail(w, 502, fmt.Sprintf("AI parsing error: %v", err))
			return
		}
	}

	// Fallback to local heuristic parser if AI key wasn't provided or failed with text present
	if len(questions) == 0 && p.Text != "" {
		questions = parseQuestionsLocally(p.Text)
		source = "local_heuristic"
	}

	if len(questions) == 0 {
		if apiKey == "" && len(p.Images) > 0 {
			fail(w, 400, "AI API Key required to analyze images (يرجى إدخال مفتاح Gemini أو OpenAI)")
			return
		}
		fail(w, 422, "No questions could be extracted. Please check the text formatting or try adding more question details.")
		return
	}

	write(w, 200, aiParseResponse{
		Questions: questions,
		Source:    source,
		Count:     len(questions),
	})
}

// Gemini Vision / Text Parser
func parseWithGemini(ctx context.Context, apiKey, text string, images []string) ([]aiParseResultQuestion, error) {
	type inlineData struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	type part struct {
		Text       string      `json:"text,omitempty"`
		InlineData *inlineData `json:"inlineData,omitempty"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type geminiReq struct {
		Contents         []content `json:"contents"`
		GenerationConfig struct {
			ResponseMimeType string  `json:"responseMimeType"`
			Temperature      float64 `json:"temperature"`
		} `json:"generationConfig"`
	}

	var parts []part
	promptText := aiPromptInstructions + "\n\nExam Content to Parse:\n" + text
	parts = append(parts, part{Text: promptText})

	// Add images
	for _, img := range images {
		mimeType, b64Data, err := resolveImageBytes(img)
		if err == nil && b64Data != "" {
			parts = append(parts, part{
				InlineData: &inlineData{
					MimeType: mimeType,
					Data:     b64Data,
				},
			})
		}
	}

	var gReq geminiReq
	gReq.Contents = []content{{Parts: parts}}
	gReq.GenerationConfig.ResponseMimeType = "application/json"
	gReq.GenerationConfig.Temperature = 0.1

	reqBody, err := json.Marshal(gReq)
	if err != nil {
		return nil, err
	}

	// Try gemini-1.5-flash or gemini-2.0-flash
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-1.5-flash"
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gemini API error (%d): %s", resp.StatusCode, string(body))
	}

	type geminiCandidate struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	type geminiResp struct {
		Candidates []geminiCandidate `json:"candidates"`
	}

	var gResp geminiResp
	if err := json.Unmarshal(body, &gResp); err != nil {
		return nil, err
	}
	if len(gResp.Candidates) == 0 || len(gResp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from Gemini")
	}

	responseText := gResp.Candidates[0].Content.Parts[0].Text
	return cleanAndNormalizeQuestions(responseText)
}

// OpenAI / OpenAI-compatible Vision / Text Parser
func parseWithOpenAI(ctx context.Context, apiKey, text string, images []string) ([]aiParseResultQuestion, error) {
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	type openAIContentPart struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}

	type openAIMessage struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}

	var userParts []openAIContentPart
	userParts = append(userParts, openAIContentPart{
		Type: "text",
		Text: "Please extract all questions from this exam content:\n\n" + text,
	})

	for _, img := range images {
		mimeType, b64Data, err := resolveImageBytes(img)
		if err == nil && b64Data != "" {
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64Data)
			userParts = append(userParts, openAIContentPart{
				Type: "image_url",
				ImageURL: &struct {
					URL string `json:"url"`
				}{URL: dataURI},
			})
		}
	}

	messages := []openAIMessage{
		{Role: "system", Content: aiPromptInstructions},
		{Role: "user", Content: userParts},
	}

	reqPayload := map[string]any{
		"model":           model,
		"messages":        messages,
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.1,
	}

	reqBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI API error (%d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if len(res.Choices) == 0 {
		return nil, fmt.Errorf("no response choices from OpenAI")
	}

	return cleanAndNormalizeQuestions(res.Choices[0].Message.Content)
}

// resolveImageBytes handles data URLs and local /uploads/ paths
func resolveImageBytes(img string) (string, string, error) {
	img = strings.TrimSpace(img)
	if strings.HasPrefix(img, "data:") {
		// data:image/png;base64,xxxx
		parts := strings.SplitN(img, ",", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid data URI")
		}
		mime := "image/jpeg"
		if semi := strings.Index(parts[0], ";"); semi > 5 {
			mime = parts[0][5:semi]
		}
		return mime, parts[1], nil
	}

	if strings.HasPrefix(img, "/uploads/") {
		fileName := filepath.Base(img)
		filePath := filepath.Join(uploadDir(), fileName)
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", "", err
		}
		mime := http.DetectContentType(data)
		b64 := base64.StdEncoding.EncodeToString(data)
		return mime, b64, nil
	}

	return "", "", fmt.Errorf("unsupported image format")
}

// cleanAndNormalizeQuestions sanitizes the JSON output from AI models
func cleanAndNormalizeQuestions(rawJSON string) ([]aiParseResultQuestion, error) {
	// Strip markdown code fences if any
	s := strings.TrimSpace(rawJSON)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}
	s = strings.TrimSpace(s)

	type intermediateQ struct {
		Kind          string `json:"kind"`
		Prompt        string `json:"prompt"`
		Options       any    `json:"options"`
		CorrectAnswer any    `json:"correctAnswer"`
		Points        int    `json:"points"`
		ImageURL      string `json:"imageUrl"`
	}

	var items []intermediateQ

	// Try parsing as {"questions": [...]} or directly as [...]
	var container struct {
		Questions []intermediateQ `json:"questions"`
	}
	if err := json.Unmarshal([]byte(s), &container); err == nil && len(container.Questions) > 0 {
		items = container.Questions
	} else {
		_ = json.Unmarshal([]byte(s), &items)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("could not parse valid question array from AI output")
	}

	var results []aiParseResultQuestion
	for _, item := range items {
		normQ := normalizeParsedQuestion(item.Kind, item.Prompt, item.Options, item.CorrectAnswer, item.Points, item.ImageURL)
		if normQ.Prompt != "" {
			results = append(results, normQ)
		}
	}

	return results, nil
}

func normalizeParsedQuestion(kind, prompt string, rawOptions, rawCorrect any, points int, imageURL string) aiParseResultQuestion {
	prompt = strings.TrimSpace(prompt)
	// Strip leading numbering: e.g. "1.", "1-", "س1:", "س1-", "Q1.", "Q1:"
	reNumber := regexp.MustCompile(`^(\d+[\.\-\:\)]|[سQ]\s*\d+[\.\-\:\)]|السؤال\s+[\w\d]+[\.\-\:\)])\s*`)
	prompt = reNumber.ReplaceAllString(prompt, "")

	kind = strings.ToLower(strings.TrimSpace(kind))
	if !questionKinds[kind] {
		// Default fallback kind
		if rawOptions != nil {
			kind = "single"
		} else {
			kind = "short"
		}
	}

	if points <= 0 {
		points = 1
	}

	var optionsJSON []byte = []byte("[]")
	var correctAnswer string

	switch kind {
	case "single", "image":
		var opts []string
		if slice, ok := rawOptions.([]any); ok {
			for _, o := range slice {
				str := strings.TrimSpace(fmt.Sprint(o))
				if str != "" {
					opts = append(opts, str)
				}
			}
		} else if strSlice, ok := rawOptions.([]string); ok {
			for _, str := range strSlice {
				str = strings.TrimSpace(str)
				if str != "" {
					opts = append(opts, str)
				}
			}
		}
		if len(opts) > 0 {
			optionsJSON, _ = json.Marshal(opts)
		}

		// Correct answer must match one of the options
		corrStr := strings.TrimSpace(fmt.Sprint(rawCorrect))
		if corrStr == "<nil>" {
			corrStr = ""
		}
		// If answer is letter like "A", "B", "أ", "ب", map it to option
		mapped := mapOptionLetter(corrStr, opts)
		if mapped != "" {
			correctAnswer = mapped
		} else {
			// Find direct match
			found := false
			for _, o := range opts {
				if strings.EqualFold(o, corrStr) {
					correctAnswer = o
					found = true
					break
				}
			}
			if !found && len(opts) > 0 {
				correctAnswer = opts[0]
			}
		}

	case "multi":
		var opts []string
		if slice, ok := rawOptions.([]any); ok {
			for _, o := range slice {
				str := strings.TrimSpace(fmt.Sprint(o))
				if str != "" {
					opts = append(opts, str)
				}
			}
		}
		optionsJSON, _ = json.Marshal(opts)

		var correctArr []string
		if cSlice, ok := rawCorrect.([]any); ok {
			for _, c := range cSlice {
				cs := strings.TrimSpace(fmt.Sprint(c))
				m := mapOptionLetter(cs, opts)
				if m != "" {
					correctArr = append(correctArr, m)
				} else if cs != "" {
					correctArr = append(correctArr, cs)
				}
			}
		} else if cStr, ok := rawCorrect.(string); ok && cStr != "" {
			_ = json.Unmarshal([]byte(cStr), &correctArr)
			if len(correctArr) == 0 && cStr != "" {
				correctArr = []string{cStr}
			}
		}
		if len(correctArr) == 0 && len(opts) > 0 {
			correctArr = []string{opts[0]}
		}
		cBytes, _ := json.Marshal(correctArr)
		correctAnswer = string(cBytes)

	case "truefalse":
		optionsJSON = []byte("[]")
		cStr := strings.ToLower(strings.TrimSpace(fmt.Sprint(rawCorrect)))
		if strings.Contains(cStr, "true") || strings.Contains(cStr, "صح") || strings.Contains(cStr, "صواب") || strings.Contains(cStr, "نعم") {
			correctAnswer = "True"
		} else {
			correctAnswer = "False"
		}

	case "matching":
		if obj, ok := rawOptions.(map[string]any); ok {
			optionsJSON, _ = json.Marshal(obj)
		}
		if cMap, ok := rawCorrect.(map[string]any); ok {
			b, _ := json.Marshal(cMap)
			correctAnswer = string(b)
		} else if cStr, ok := rawCorrect.(string); ok {
			correctAnswer = cStr
		}

	case "ordering":
		var opts []string
		if slice, ok := rawOptions.([]any); ok {
			for _, o := range slice {
				str := strings.TrimSpace(fmt.Sprint(o))
				if str != "" {
					opts = append(opts, str)
				}
			}
		}
		optionsJSON, _ = json.Marshal(opts)
		correctAnswer = string(optionsJSON)

	default: // short, fill, numeric, essay
		optionsJSON = []byte("[]")
		correctAnswer = strings.TrimSpace(fmt.Sprint(rawCorrect))
		if correctAnswer == "<nil>" {
			correctAnswer = ""
		}
	}

	return aiParseResultQuestion{
		Kind:          kind,
		Prompt:        prompt,
		Options:       optionsJSON,
		CorrectAnswer: correctAnswer,
		Points:        points,
		ImageURL:      imageURL,
	}
}

// mapOptionLetter maps letters like "أ", "ب", "ج", "د", "A", "B", "C", "D", "1", "2" to option texts
func mapOptionLetter(key string, opts []string) string {
	key = strings.TrimSpace(key)
	arLetters := []string{"أ", "ا", "ب", "ج", "د", "هـ", "و", "ز"}
	enLetters := []string{"A", "B", "C", "D", "E", "F", "G", "H"}

	for i, l := range arLetters {
		if strings.EqualFold(key, l) && i < len(opts) {
			return opts[i]
		}
	}
	for i, l := range enLetters {
		if strings.EqualFold(key, l) && i < len(opts) {
			return opts[i]
		}
	}
	// Also numeric indices e.g. "1", "2"
	if idx, err := strconv.Atoi(key); err == nil && idx >= 1 && idx <= len(opts) {
		return opts[idx-1]
	}
	return ""
}

func matchOptionLine(line string) (label string, text string, isCorrect bool, ok bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "*") && len(line) > 1 && line[1] != ' ' {
		isCorrect = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
	}
	if strings.HasPrefix(line, "✓") {
		isCorrect = true
		line = strings.TrimSpace(strings.TrimPrefix(line, "✓"))
	}

	// 1. Matches: "أ) ...", "(أ) ...", "[أ] ...", "أ - ...", "أ. ...", "أ: ..."
	reLetter := regexp.MustCompile(`^(?:[\(\[]\s*([أ-يa-zA-Z\d]{1,2})\s*[\)\]]|([أ-يa-zA-Z\d]{1,2})\s*[\)\]\.\-\:])\s*(.*)$`)
	if m := reLetter.FindStringSubmatch(line); m != nil {
		l := m[1]
		if l == "" {
			l = m[2]
		}
		t := m[3]
		if strings.HasSuffix(t, "*") || strings.Contains(t, "✓") || strings.Contains(t, "[x]") || strings.Contains(t, "(صح)") {
			isCorrect = true
			t = strings.Trim(t, "*✓ ")
			t = strings.ReplaceAll(t, "[x]", "")
			t = strings.ReplaceAll(t, "(صح)", "")
			t = strings.TrimSpace(t)
		}
		return l, t, isCorrect, true
	}

	// 2. Matches bullet options: "- ...", "• ...", "* ..."
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "• ") || strings.HasPrefix(line, "* ") {
		t := strings.TrimSpace(line[2:])
		if strings.HasSuffix(t, "*") || strings.Contains(t, "✓") || strings.Contains(t, "[x]") || strings.Contains(t, "(صح)") {
			isCorrect = true
			t = strings.Trim(t, "*✓ ")
			t = strings.ReplaceAll(t, "[x]", "")
			t = strings.ReplaceAll(t, "(صح)", "")
			t = strings.TrimSpace(t)
		}
		return "", t, isCorrect, true
	}

	return "", "", false, false
}

// parseQuestionsLocally is a smart offline heuristic parser that parses Arabic and English exams without any external API.
func parseQuestionsLocally(text string) []aiParseResultQuestion {
	lines := strings.Split(text, "\n")
	type parsedBlock struct {
		number      string
		prompt      string
		options     []string
		correctMark int
		isTF        bool
		tfAnswer    string
		isEssay     bool
		isFill      bool
		fillAnswer  string
	}

	var blocks []parsedBlock
	var current *parsedBlock

	// Question start regex:
	// Matches: "1.", "1-", "1)", "1:", "س1:", "س 1:", "س1-", "Q1.", "Q1:", "السؤال الأول:", "السؤال 1:"
	reQStart := regexp.MustCompile(`^(?i)(?:(\d+)\s*[\.\-\)\:]|[سQ]\s*(\d+)\s*[\.\-\)\:]|السؤال\s+(?:رقم\s*)?([0-9\p{Arabic}]+)\s*[\:\.\-])\s*(.*)$`)

	// Answer key block at bottom
	reAnswerKeyHeader := regexp.MustCompile(`(?i)(?:مفتاح\s+الإجابات|الإجابات\s+النموذجية|حل\s+الأسئلة|Answer\s*Key|Answers)\s*[\:\-]?`)
	var answerKeyMap = make(map[string]string)
	inAnswerKey := false

	flushCurrent := func() {
		if current != nil && strings.TrimSpace(current.prompt) != "" {
			blocks = append(blocks, *current)
			current = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Check if we entered answer key section at bottom
		if reAnswerKeyHeader.MatchString(trimmed) {
			flushCurrent()
			inAnswerKey = true
			continue
		}

		if inAnswerKey {
			// Lines like: "1: ب", "2- صح", "3. A", "س1: ج"
			reKey := regexp.MustCompile(`(?:[سQ]?\s*(\d+))\s*[\:\.\-\)]\s*([^\s,]+)`)
			matches := reKey.FindAllStringSubmatch(trimmed, -1)
			for _, m := range matches {
				if len(m) >= 3 {
					answerKeyMap[m[1]] = m[2]
				}
			}
			continue
		}

		// Check if line starts a new question
		if match := reQStart.FindStringSubmatch(trimmed); match != nil {
			flushCurrent()

			qNum := match[1]
			if qNum == "" {
				qNum = match[2]
			}
			if qNum == "" {
				qNum = match[3]
			}
			qPrompt := match[4]

			current = &parsedBlock{
				number:      qNum,
				prompt:      qPrompt,
				correctMark: -1,
			}

			// Check for inline True/False keywords in prompt
			pLower := strings.ToLower(qPrompt)
			if strings.Contains(pLower, "صح أم خطأ") || strings.Contains(pLower, "صح أو خطأ") || strings.Contains(pLower, "صواب أو خطأ") || strings.Contains(pLower, "صواب أم خطأ") || strings.Contains(pLower, "true or false") || strings.Contains(pLower, "(صح/خطأ)") {
				current.isTF = true
			}
			if strings.Contains(qPrompt, "_____") || strings.Contains(qPrompt, ".....") {
				current.isFill = true
			}
			continue
		}

		// If no active question block, start one with this text
		if current == nil {
			current = &parsedBlock{
				number:      fmt.Sprintf("%d", len(blocks)+1),
				prompt:      trimmed,
				correctMark: -1,
			}
			continue
		}

		// Check if line is a standalone True/False answer indicator
		tLine := strings.ToLower(trimmed)
		if tLine == "صح" || tLine == "صواب" || tLine == "(صح)" || tLine == "(صواب)" || tLine == "[صح]" || tLine == "[صواب]" || tLine == "true" || tLine == "(true)" {
			current.isTF = true
			current.tfAnswer = "True"
			continue
		}
		if tLine == "خطأ" || tLine == "(خطأ)" || tLine == "[خطأ]" || tLine == "false" || tLine == "(false)" {
			current.isTF = true
			current.tfAnswer = "False"
			continue
		}

		// Check if line is an option
		if _, optText, isCorrect, ok := matchOptionLine(trimmed); ok {
			current.options = append(current.options, optText)
			if isCorrect {
				current.correctMark = len(current.options) - 1
			}
			continue
		}

		// Check for fill / short answer indicator: "الإجابة: ..." or "Ans: ..."
		if strings.HasPrefix(trimmed, "الإجابة:") || strings.HasPrefix(trimmed, "الجواب:") || strings.HasPrefix(trimmed, "Ans:") || strings.HasPrefix(trimmed, "Answer:") {
			ans := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
			current.fillAnswer = ans
			current.isFill = true
			continue
		}

		// Otherwise, append to existing prompt
		current.prompt += " " + trimmed
	}
	flushCurrent()

	var results []aiParseResultQuestion
	for i, b := range blocks {
		prompt := strings.TrimSpace(b.prompt)
		if prompt == "" {
			continue
		}

		// Check answer key map for this question number
		keyVal := ""
		if b.number != "" {
			keyVal = answerKeyMap[b.number]
		}
		if keyVal == "" {
			keyVal = answerKeyMap[fmt.Sprintf("%d", i+1)]
		}

		if b.isTF || (len(b.options) == 0 && (strings.Contains(prompt, "صح أم خطأ") || strings.Contains(prompt, "صح أو خطأ") || strings.Contains(prompt, "صواب أم خطأ") || strings.Contains(prompt, "صواب أو خطأ"))) {
			tfAns := b.tfAnswer
			if keyVal != "" {
				kLower := strings.ToLower(keyVal)
				if strings.Contains(kLower, "صح") || strings.Contains(kLower, "صواب") || strings.Contains(kLower, "true") {
					tfAns = "True"
				} else if strings.Contains(kLower, "خطأ") || strings.Contains(kLower, "false") {
					tfAns = "False"
				}
			}
			if tfAns == "" {
				tfAns = "True" // default
			}
			results = append(results, aiParseResultQuestion{
				Kind:          "truefalse",
				Prompt:        prompt,
				Options:       []byte("[]"),
				CorrectAnswer: tfAns,
				Points:        1,
			})
			continue
		}

		if len(b.options) >= 2 {
			optsJSON, _ := json.Marshal(b.options)
			corr := ""
			if b.correctMark >= 0 && b.correctMark < len(b.options) {
				corr = b.options[b.correctMark]
			} else if keyVal != "" {
				corr = mapOptionLetter(keyVal, b.options)
			}
			if corr == "" && len(b.options) > 0 {
				corr = b.options[0]
			}

			results = append(results, aiParseResultQuestion{
				Kind:          "single",
				Prompt:        prompt,
				Options:       optsJSON,
				CorrectAnswer: corr,
				Points:        1,
			})
			continue
		}

		if b.isFill || strings.Contains(prompt, "_____") {
			results = append(results, aiParseResultQuestion{
				Kind:          "fill",
				Prompt:        prompt,
				Options:       []byte("[]"),
				CorrectAnswer: b.fillAnswer,
				Points:        1,
			})
			continue
		}

		// Detect essay questions
		pLower := strings.ToLower(prompt)
		isEssay := strings.HasPrefix(pLower, "اشرح") || strings.HasPrefix(pLower, "وضح") || strings.HasPrefix(pLower, "علل") || strings.HasPrefix(pLower, "اذكر") || strings.HasPrefix(pLower, "ما رأيك") || strings.HasPrefix(pLower, "explain") || strings.HasPrefix(pLower, "describe")
		kind := "short"
		if isEssay {
			kind = "essay"
		}

		results = append(results, aiParseResultQuestion{
			Kind:          kind,
			Prompt:        prompt,
			Options:       []byte("[]"),
			CorrectAnswer: b.fillAnswer,
			Points:        1,
		})
	}

	return results
}
